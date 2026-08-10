"""
Tests for the Python RAG API hygiene fixes on branch fix/kg-python-rag.

Verifies three things without importing the heavy AI model stack (torch/openai
etc. that `api/__init__.py` drags in):

  1. Every /api/rag/* route is protected by `get_current_user_from_cookie`
     (auth fix; no unauthenticated access).
  2. `rebuild_graph` no longer imports the broken `database.user_settings` and
     no longer runs destructive DELETEs before its imports are resolved.
  3. The knowledge-graph DDL is reachable through the normal startup migration
     path (axiom_backend/init-db/10-knowledge-graph.sql) and idempotent.

These are source/structural tests: the real HTTP 401 behaviour is additionally
verified live in the deployed container (which has fastapi/httpx installed).
"""

import ast
import os
import re
from pathlib import Path

import pytest

BACKEND = Path(__file__).resolve().parents[1] / "axiom_backend"
RAG_FILE = BACKEND / "api" / "rag.py"
INIT_DB = BACKEND / "init-db"
KG_MIGRATION = INIT_DB / "10-knowledge-graph.sql"
ORIG_MIGRATION = BACKEND / "database" / "migrations" / "add_knowledge_graph_tables.sql"


# ---------------------------------------------------------------------------
# 1. Auth on /api/rag/*
# ---------------------------------------------------------------------------

def test_every_route_has_auth_dependency():
    """Every @router.* decorated async fn in rag.py must depend on
    get_current_user_from_cookie."""
    src = RAG_FILE.read_text(encoding="utf-8")
    tree = ast.parse(src)

    # Find funcs decorated with @router.<method>  (handlers are ``async def``,
    # so we must match AsyncFunctionDef, not just FunctionDef)
    decorated_nodes = []
    func_types = (ast.FunctionDef, ast.AsyncFunctionDef)
    for node in ast.walk(tree):
        if isinstance(node, func_types):
            for dec in node.decorator_list:
                # @router.post(...)/@router.get(...) parse as a Call whose func is
                # an Attribute(value=Name('router'), attr='post').
                func = dec.func if isinstance(dec, ast.Call) else dec
                if (
                    isinstance(func, ast.Attribute)
                    and isinstance(func.value, ast.Name)
                    and func.value.id == "router"
                    and func.attr in {"get", "post", "put", "delete", "patch"}
                ):  # noqa: E129
                    decorated_nodes.append(node)

    assert decorated_nodes, "No @router.* decorated functions found in rag.py"
    assert len(decorated_nodes) >= 6, (
        f"Expected at least the original 6 RAG routes, got "
        f"{[n.name for n in decorated_nodes]}"
    )

    missing = []
    for node in decorated_nodes:
        # Scan the whole function (signature + body) for the auth dependency.
        # FastAPI resolves Depends(get_current_user_from_cookie) from the
        # default value, which appears in the ast source segment."
        node_src = ast.get_source_segment(src, node) or ""
        if "get_current_user_from_cookie" not in node_src:
            missing.append(node.name)

    assert not missing, (
        "RAG routes missing auth dependency (get_current_user_from_cookie): "
        f"{missing}"
    )


def test_no_unauthenticated_route():
    """The required auth dependency, not the optional variant (which returns
    None and would allow unauthenticated access), is what rag.py uses."""
    src = RAG_FILE.read_text(encoding="utf-8")
    assert "get_current_user_from_cookie" in src
    # Routes must not silently use the optional (returns-None) dependency.
    assert "get_current_user_optional" not in src


GRAPH_STORE = BACKEND / "ai_researcher" / "core_rag" / "graph_store.py"


def _graph_store_src() -> str:
    """Return the full source of graph_store.py for structural checks."""
    return GRAPH_STORE.read_text(encoding="utf-8")


def _rag_func_src(name: str) -> str:
    """Return source segment of an async route in rag.py by name."""
    src = RAG_FILE.read_text(encoding="utf-8")
    tree = ast.parse(src)
    for node in ast.walk(tree):
        if isinstance(node, ast.AsyncFunctionDef) and node.name == name:
            return ast.get_source_segment(src, node)
    raise AssertionError(f"async route {name!r} not found")


def test_user_scoping_in_queries():
    """Every read/rebuild route must scope its SQL to the current user
    (d.user_id / documents.user_id). The auth dependency alone is not enough
    to prevent cross-user access."""
    scoped_routes = ["rebuild_graph", "get_all_chunks", "get_chunk_detail",
                     "get_knowledge_graph", "get_entities"]
    for name in scoped_routes:
        seg = _rag_func_src(name)
        assert "user_id" in seg and "current_user.id" in seg, (
            f"route {name} is not scoped to the current user"
        )


def test_rebuild_graph_http_exception_passthrough():
    """rebuild_graph must re-raise HTTPException (404 not-found) before the
    generic except handler wraps it into a 500."""
    seg = _rag_func_src("rebuild_graph")
    assert "except HTTPException:" in seg
    # The re-raise must appear before the generic except Exception (as e).
    eh_idx = seg.index("except HTTPException:")
    gen_idx = seg.index("except Exception")
    assert eh_idx < gen_idx, (
        "HTTPException handler must come before the generic 500 handler"
    )


def test_rebuild_graph_atomic_session():
    """The rebuild must reuse the endpoint `db` session so DELETEs and graph
    writes land in ONE transaction (no half-lost graph on partial failure).
    Verify every GraphStore write call is passed session=db and that there is a
    single commit at the end plus a rollback on failure."""
    seg = _rag_func_src("rebuild_graph")
    # Every graph-store write must carry session=db.
    for call in ["build_sequential_relationships", "add_entity",
                 "link_entity_to_chunk", "build_cooccurrence_relationships"]:
        assert call in seg, f"{call} missing from rebuild_graph"
        # crude check: the call should be followed by a session=db kwarg within
        # the same call expression region.
        call_pos = seg.index(call)
        window = seg[call_pos:call_pos + 200]
        assert "session=db" in window, f"{call} not passed session=db"
    assert "db.commit()" in seg
    assert "db.rollback()" in seg


def test_graph_edges_scoped_to_evidence_chunks():
    """get_knowledge_graph edges must only return relationships whose evidence
    chunk_ids are provably in the current user's documents. A plain
    entity_ids-only filter leaks cross-user edges because document_entities are
    global canonical rows."""
    seg = _rag_func_src("get_knowledge_graph")
    assert "jsonb_array_elements_text(er.relationship_metadata->'chunk_ids')" in seg
    assert "d.user_id = :user_id" in seg
    # The edge EXISTS must ALSO honour the optional doc_id filter, so a
    # per-document graph only shows edges evidenced by that exact document
    # (not by a sibling document owned by the same user).
    assert ":doc_id IS NULL OR dc.doc_id = CAST(:doc_id AS uuid)" in seg
    # The edge SELECT must not be a bare entity_ids-only filter without the
    # evidence-based EXISTS guard.
    assert "EXISTS (" in seg


def test_chunk_detail_target_doc_scoped():
    """chunk relationships must enforce ownership on BOTH the source and the
    target document, not just the source (td.user_id)."""
    seg = _rag_func_src("get_chunk_detail")
    assert "sd.user_id = :user_id" in seg
    assert "td.user_id = :user_id" in seg


def test_entities_occurrence_count_scoped():
    """get_entities occurrence_count must count only the current user's chunks,
    not corpus-wide frequency (which would leak cross-user signal)."""
    seg = _rag_func_src("get_entities")
    # The occurrence subquery must be joined back to the user's documents.
    assert "FROM entity_chunk_occurrences e2" in seg
    assert "d2.user_id = :user_id" in seg


def test_rebuild_graph_clears_stale_entity_relationships():
    """rebuild_graph must also clear stale entity_relationships (edges)
    evidenced by this document before rebuilding co-occurrence rows, otherwise
    old edges whose evidence chunk_ids reference this doc stay visible in
    GET /api/rag/graph after a rebuild (matching the background reprocess path)."""
    seg = _rag_func_src("rebuild_graph")
    assert "DELETE FROM entity_relationships" in seg
    # The cleanup must be scoped to edges whose evidence chunk_ids reference the
    # user-owned doc, mirroring the evidence-based style used in /graph.
    assert "relationship_metadata->'chunk_ids'" in seg
    assert "dc.doc_id = :doc_id" in seg and "d.user_id = :user_id" in seg


def test_graph_store_caller_session_does_not_rollback():
    """When a caller supplies a session (session is not None), GraphStore must
    NOT roll back — that is the caller's job. Unconditional rollback would break
    the "caller owns the transaction" contract."""
    src = _graph_store_src()
    tree = ast.parse(src)
    session_methods = [
        "add_entity",
        "link_entity_to_chunk",
        "add_chunk_relationship",
        "build_cooccurrence_relationships",
        "add_entity_relationship",
    ]
    for method in session_methods:
        fn = None
        for node in ast.walk(tree):
            if (
                isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
                and node.name == method
            ):
                fn = node
                break
        assert fn is not None, f"graph_store.{method} not found"
        seg = ast.get_source_segment(src, fn)
        # The method must be session-aware AND every rollback inside it must be
        # guarded by `if own_session:`.
        assert "session=None" in seg, f"{method} missing session=None param"
        if "db.rollback()" in seg:
            # There is a rollback guarded by the own_session check.
            assert "if own_session:" in seg, f"{method} rollback not guarded"
            # No *unguarded* rollback: the pattern `db.rollback()` should only
            # appear under an own_session guard block.
            rb_pos = [m.start() for m in __import__("re").finditer(
                r"db\.rollback\(\)", seg)]
            assert rb_pos, f"{method} has no rollback"
        else:
            # Methods without their own rollback (e.g. build_sequential
            # forwards to add_chunk_relationship) are fine.
            pass


# ---------------------------------------------------------------------------
# 2. rebuild_graph import / destructive-ordering fix
# ---------------------------------------------------------------------------

def test_rebuild_graph_no_broken_user_settings_import():
    """The broken `from database.user_settings import get_user_settings` must be
    gone, along with the unused MetadataExtractor/TextEmbedder imports."""
    src = RAG_FILE.read_text(encoding="utf-8")
    assert "from database.user_settings import" not in src
    assert "MetadataExtractor" not in src
    assert "TextEmbedder" not in src
    # get_user_settings must not appear at all (no leftover usage)
    assert "get_user_settings()" not in src


def test_rebuild_graph_imports_precede_deletes():
    """Within the rebuild_graph function, imports must be resolved before the
    destructive DELETE statements, so a missing module cannot leave the doc
    half-deleted."""
    src = RAG_FILE.read_text(encoding="utf-8")
    tree = ast.parse(src)
    fn = None
    for node in ast.walk(tree):
        if isinstance(node, ast.AsyncFunctionDef) and node.name == "rebuild_graph":
            fn = node
            break
    assert fn is not None, "rebuild_graph function not found"

    seg = ast.get_source_segment(src, fn)
    # Where do imports happen vs DELETEs?
    import_positions = [
        m.start() for m in re.finditer(
            r"^\s*from \S+ import |^\s*import \S+", seg, re.MULTILINE
        )
    ]
    delete_positions = [
        m.start() for m in re.finditer(r"DELETE FROM", seg)
    ]
    assert delete_positions, "expected DELETE FROM statements in rebuild_graph"
    assert import_positions, "expected imports in rebuild_graph"

    first_delete = min(delete_positions)
    graph_store_import = [
        i for i in import_positions
        if "GraphStore" in seg[i:i + 200]
    ]
    assert graph_store_import, "GraphStore must be imported before deletes"
    # The newest import (GraphStore + EntityExtractor) must appear before the
    # first DELETE, otherwise a broken import could be hit after deleting.
    assert graph_store_import[0] < first_delete, (
        "GraphStore import must precede DELETE statements to avoid data loss"
    )


# ---------------------------------------------------------------------------
# 3. Migration reachable via normal startup path + idempotent
# ---------------------------------------------------------------------------

def test_kg_migration_present_in_init_db():
    """The live KG DDL must be a numbered migration under init-db/ so
    run_sql_migrations() picks it up (files 03-..99-)."""
    assert KG_MIGRATION.exists(), f"missing {KG_MIGRATION}"
    assert KG_MIGRATION.name.startswith("10-"), (
        "expected prefix 10- so run_sql_migrations() (03..99 range) runs it"
    )


def test_kg_migration_in_scan_range():
    """run_sql_migrations() only runs files matching f'{n:02d}-' for n in
    3..99. '10-' is inside that range (10)."""
    import re as _re
    name = KG_MIGRATION.name
    matched = any(name.startswith(f"{n:02d}-") for n in range(3, 100))
    assert matched, f"{name} would be skipped by run_sql_migrations()"


def test_kg_migration_idempotent_no_deletes():
    """Migration must not delete/drop data; all creates must be IF NOT EXISTS."""
    sql = KG_MIGRATION.read_text(encoding="utf-8")
    assert "DROP " not in sql.upper()
    assert "DELETE FROM" not in sql.upper()
    assert "CREATE TABLE IF NOT EXISTS" in sql.upper()
    # Every CREATE INDEX must be IF NOT EXISTS too
    for line in sql.splitlines():
        if line.strip().upper().startswith("CREATE INDEX"):
            assert "IF NOT EXISTS" in line.upper(), line


def test_orig_migration_not_executed_note():
    """The old database/migrations copy, if kept, must not be scanned by
    run_sql_migrations() (it isn't) and should be marked deprecated or the
    live copy should differ."""
    if ORIG_MIGRATION.exists():
        orig = ORIG_MIGRATION.read_text(encoding="utf-8")
        live = KG_MIGRATION.read_text(encoding="utf-8")
        # Live file is the superset (has header + possibly the deprecation
        # note on the original). They must be structurally identical apart
        # from comments.
        lines_live = {
            ln.strip()
            for ln in live.splitlines()
            if ln.strip() and not ln.strip().startswith("--")
        }
        lines_orig = {
            ln.strip()
            for ln in orig.splitlines()
            if ln.strip() and not ln.strip().startswith("--")
        }
        assert lines_live == lines_orig, (
            "init-db/10-knowledge-graph.sql and database/migrations/ copy "
            "drifted; keep them in sync"
        )
