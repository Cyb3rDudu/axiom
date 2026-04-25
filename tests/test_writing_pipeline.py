"""Integration tests for run_response_pipeline + the flag matrix.

Exercises the post-agent pipeline end-to-end with a stubbed SQLAlchemy
session and pre-fabricated models.Reference rows. Each test asserts on
the final response text (post-processed), the persisted assistant
message, and the WebSocket payload contents.

Stubs (rather than a real Postgres) are deliberate: the pipeline is
pure post-processing, so stubbing the DB lets us isolate stage
behaviour without spinning up a container in CI.
"""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Dict, List, Optional

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.writing_flags import WritingFlags  # noqa: E402
from services.writing_pipeline import (  # noqa: E402
    PipelineContext,
    PipelineResult,
    run_response_pipeline,
)


# ---------------------------------------------------------------------------
# Stub fixtures
# ---------------------------------------------------------------------------


class _StubReference:
    """Stand-in for models.Reference rows the registry returns."""

    def __init__(
        self,
        *,
        id: str,
        entry_key: str,
        authors: Optional[List[Dict[str, str]]] = None,
        year: Optional[int] = None,
        publisher: Optional[str] = None,
        container_title: Optional[str] = None,
        title: Optional[str] = None,
        url: Optional[str] = None,
        web_url: Optional[str] = None,
        pages: Optional[str] = None,
        doi: Optional[str] = None,
        accessed_at: Any = None,
        reference_type: str = "web",
    ):
        self.id = id
        self.entry_key = entry_key
        self.authors = authors or []
        self.year = year
        self.publisher = publisher
        self.container_title = container_title
        self.title = title
        self.url = url
        self.web_url = web_url
        self.pages = pages
        self.doi = doi
        self.accessed_at = accessed_at
        self.reference_type = reference_type


class _StubQuery:
    """Returns a fixed result regardless of filter calls."""

    def __init__(self, rows: List[Any]):
        self._rows = rows

    def filter(self, *_args, **_kwargs):
        return self

    def all(self):
        return self._rows

    def first(self):
        return self._rows[0] if self._rows else None

    def order_by(self, *_args, **_kwargs):
        return self

    def delete(self, *_args, **_kwargs):
        return None


class _StubSession:
    """Captures add/commit calls; query returns canned rows by model."""

    def __init__(self, rows_by_model: Optional[Dict[str, List[Any]]] = None):
        self.added: List[Any] = []
        self.commits = 0
        self._rows = rows_by_model or {}

    def add(self, obj: Any) -> None:
        self.added.append(obj)

    def commit(self) -> None:
        self.commits += 1

    def refresh(self, _obj: Any) -> None:
        return None

    def query(self, model):
        name = getattr(model, "__name__", str(model))
        return _StubQuery(self._rows.get(name, []))


def _stub_draft(draft_id: str = "draft-1") -> SimpleNamespace:
    return SimpleNamespace(id=draft_id)


def _stub_citation_profile() -> SimpleNamespace:
    return SimpleNamespace(id="kmu_apa7", citation_mode="author_year")


def _make_context(
    *,
    db: _StubSession,
    flags: WritingFlags,
    figure_resolution: Optional[Dict[str, Any]] = None,
) -> PipelineContext:
    return PipelineContext(
        db=db,
        draft=_stub_draft(),
        chat_id="chat-1",
        session_id="session-1",
        user_id=42,
        task_id="task-1",
        flags=flags,
        citation_profile=_stub_citation_profile(),
        figure_resolution=figure_resolution,
    )


def _run_pipeline(*, raw: str, context: PipelineContext) -> PipelineResult:
    return asyncio.run(
        run_response_pipeline(
            raw_response=raw,
            sources=[],
            context=context,
        )
    )


# ---------------------------------------------------------------------------
# 1. All-off baseline
# ---------------------------------------------------------------------------


class TestAllFlagsOff:
    """No structured biblio, no completeness passes — pipeline is a passthrough."""

    def test_response_passes_through_unchanged(self):
        db = _StubSession()
        ctx = _make_context(db=db, flags=WritingFlags.all_off())
        raw = "Plain prose with no special blocks."

        result = _run_pipeline(raw=raw, context=ctx)

        assert result.final_response_text == raw
        assert result.structured_refs_summary is None
        assert result.citation_sync_dict is None
        assert result.completeness_telemetry == {}
        # Message persisted to DB
        assert len(db.added) == 1
        assert db.added[0].content == raw
        # WebSocket payload reflects passthrough
        assert result.websocket_payload["message"] == raw
        assert result.websocket_payload["completeness"] is None


# ---------------------------------------------------------------------------
# 2. Completeness flags on, no planner
# ---------------------------------------------------------------------------


class TestCompletenessAllOn:
    def _flags(self) -> WritingFlags:
        return WritingFlags(
            structured_bibliography=True,
            wordcount_fix=True,
            sources_always=True,
            transparent_continuation=False,
            rag_figures=False,
        )

    def test_synthesises_sources_block_from_registry(self):
        registry = [
            _StubReference(
                id="ref-1",
                entry_key="destatis-2024",
                authors=[{"family": "Destatis", "given": ""}],
                year=2024,
                title="Außenhandel",
                container_title="Statistisches Bundesamt",
                url="https://www.destatis.de/x",
                reference_type="web",
            )
        ]
        db = _StubSession(rows_by_model={"Reference": registry})
        ctx = _make_context(db=db, flags=self._flags())

        raw = (
            "Some prose body.\n\n"
            "```content-block:document\n# 1. Section\nHello\n```\n"
        )
        result = _run_pipeline(raw=raw, context=ctx)

        # sources_always synthesised a refs block
        assert "content-block:references" in result.final_response_text
        assert "destatis-2024" in result.final_response_text
        # telemetry recorded
        assert "sources_always" in result.completeness_telemetry

    def test_recomputes_wortbilanz(self):
        db = _StubSession(rows_by_model={"Reference": []})
        ctx = _make_context(db=db, flags=self._flags())
        # Document body has 8 words; declared count is way off
        raw = (
            "Wortbilanz: 9999 insgesamt\n\n"
            "```content-block:document\n"
            "# 1. Einleitung\nHier sind acht ganze deutsche Worte zum testen.\n"
            "```\n"
        )
        result = _run_pipeline(raw=raw, context=ctx)

        wc_tele = result.completeness_telemetry.get("wordcount_fix")
        assert wc_tele is not None
        assert wc_tele["actual"] < 100
        assert wc_tele["declared"] == 9999

    def test_audit_warning_surfaces(self):
        db = _StubSession()
        ctx = _make_context(
            db=db,
            flags=WritingFlags(structured_bibliography=False),
        )
        # Unbalanced fence and a URL-in-parens citation
        raw = (
            "Some text (https://example.com/page) more text.\n"
            "```content-block:document\nUnbalanced — never closes\n"
        )
        result = _run_pipeline(raw=raw, context=ctx)

        assert result.audit_dict is not None
        assert result.audit_dict["unbalanced_fences"] is True
        assert result.audit_dict["url_in_parens_count"] >= 1


# ---------------------------------------------------------------------------
# 3. Figure validation path
# ---------------------------------------------------------------------------


class TestFigureValidation:
    def test_invalid_figure_url_replaced_with_about_blank(self):
        db = _StubSession()
        ctx = _make_context(
            db=db,
            flags=WritingFlags(rag_figures=True),
            figure_resolution={
                "valid_image_urls": {"/api/documents/images/doc-1/real.png"},
            },
        )
        raw = (
            "![Chart](placeholder-fig1.png)\n\n"
            "![Real](/api/documents/images/doc-1/real.png)\n"
        )
        result = _run_pipeline(raw=raw, context=ctx)

        assert "about:blank#figure-not-resolved" in result.final_response_text
        # Real URL preserved
        assert "/api/documents/images/doc-1/real.png" in result.final_response_text
        # Telemetry counted both
        fig_tele = result.completeness_telemetry["figures_validated"]
        assert fig_tele["figures_total"] == 2
        assert fig_tele["figures_resolved"] == 1


# ---------------------------------------------------------------------------
# 4. Sources-always with empty registry
# ---------------------------------------------------------------------------


class TestSourcesAlwaysEmptyRegistry:
    def test_no_block_emitted_when_registry_empty(self):
        db = _StubSession(rows_by_model={"Reference": []})
        flags = WritingFlags(sources_always=True)
        ctx = _make_context(db=db, flags=flags)

        raw = "Plain response without a refs block."
        result = _run_pipeline(raw=raw, context=ctx)

        # No registry → synthesizer no-ops (telemetry says no_registry)
        sources_tele = result.completeness_telemetry.get("sources_always")
        assert sources_tele is None or sources_tele.get("action") == "no_registry"
        assert "content-block:references" not in result.final_response_text


# ---------------------------------------------------------------------------
# 5. WebSocket payload shape
# ---------------------------------------------------------------------------


class TestWebSocketPayload:
    def test_payload_contains_required_fields(self):
        db = _StubSession()
        ctx = _make_context(db=db, flags=WritingFlags.all_off())

        result = _run_pipeline(raw="response text", context=ctx)

        payload = result.websocket_payload
        # Contract: these fields are always present
        assert set(payload.keys()) >= {
            "message",
            "sources",
            "task_id",
            "audit",
            "structured_references",
            "citation_sync",
            "completeness",
        }
        assert payload["task_id"] == "task-1"
        assert payload["sources"] == []


# ---------------------------------------------------------------------------
# 6. Persistence contract
# ---------------------------------------------------------------------------


class TestPersistence:
    def test_post_processed_text_is_what_lands_in_db(self):
        # The text the user sees in the chat must equal what's in the DB
        # (regression test for the bug where post-processing ran AFTER
        # persist and the DB ended up with raw LLM output).
        registry = [
            _StubReference(
                id="ref-1",
                entry_key="destatis-2024",
                authors=[{"family": "Destatis", "given": ""}],
                year=2024,
                title="Außenhandel",
                container_title="Statistisches Bundesamt",
                url="https://destatis.de/x",
                reference_type="web",
            )
        ]
        db = _StubSession(rows_by_model={"Reference": registry})
        ctx = _make_context(
            db=db,
            flags=WritingFlags(sources_always=True),
        )
        raw = "Some prose body."

        result = _run_pipeline(raw=raw, context=ctx)

        # DB row must match the post-processed (sources-synthesised) text
        assert len(db.added) == 1
        persisted = db.added[0]
        assert persisted.content == result.final_response_text
        assert "content-block:references" in persisted.content
