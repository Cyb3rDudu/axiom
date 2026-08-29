"""Black-box contract tests for the processor runner (contract §19).

These run against the HTTP API (via FastAPI TestClient) plus a couple of
process/store-level checks. They need only stdlib + fastapi + pymupdf; no
database, OpenSearch, graph or Zotero access. Heavy/GPU ML is not required
because the suite runs against the ``reference`` compute backend.
"""

from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

import pytest
from axiom_ng_runner import CONTRACT_VERSION, PIPELINE_STAGES
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.job_store import Job, JobStore
from axiom_ng_runner.validation import compute_sha256
from fastapi.testclient import TestClient

REPO_ROOT = Path(__file__).resolve().parent.parent.parent


def _process_payload(src, job_id: str, key: str, opts=None) -> dict:
    o = {
        "contract_version": CONTRACT_VERSION,
        "job_id": job_id,
        "idempotency_key": key,
        "source": {"type": "zotero", "source_id": "src-1", "server_id": "srv"},
        "document": {
            "document_id": "doc-1",
            "zotero_key": "ZK",
            "zotero_version": 1,
            "metadata_snapshot": {"itemType": "book", "title": "Smoke", "date": "2024"},
        },
        "attachment": {
            "attachment_id": "att-1",
            "zotero_key": "AK",
            "zotero_version": 1,
            "content_type": "application/pdf",
            "filename": src.name,
            "local_path": str(src),
            "content_hash": compute_sha256(src),
            "size_bytes": src.stat().st_size,
            "mtime_ms": 0,
        },
        "processing": {
            "profile": "full-rag-v1",
            "force_rebuild": False,
            "extract_images": False,
            "compute_dense_embeddings": True,
            "compute_sparse_embeddings": True,
            "extract_entities": True,
            "extract_relationships": True,
            **({} if opts is None else opts),
        },
    }
    return o


@pytest.fixture(scope="function")
def client(fixture_dirs):
    """A TestClient wired to the runner app with an isolated work root."""
    work = fixture_dirs["work"] / "client"
    work.mkdir(parents=True, exist_ok=True)
    old = settings.get()
    settings.set(
        Settings(work_root=work, allowed_source_roots=(str(fixture_dirs["sources"]),))
    )
    try:
        with TestClient(app) as c:
            yield c
    finally:
        settings.set(old)


def _wait_completed(client, job_id: str, timeout: float = 30.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        r = client.get(f"/v1/jobs/{job_id}", timeout=timeout)
        assert r.status_code == 200, r.text
        body = r.json()
        if body["status"] in ("completed", "failed", "cancelled"):
            return body
        time.sleep(0.05)
    raise AssertionError(f"job {job_id} did not reach terminal state in {timeout}s")


# ---------------------------------------------------------------------------
# 1. Health and capabilities report contract v1
# ---------------------------------------------------------------------------
def test_health_and_capabilities(client):
    h = client.get("/v1/health", timeout=10)
    assert h.status_code == 200

    c = client.get("/v1/capabilities", timeout=10)
    assert c.status_code == 200
    body = c.json()
    assert CONTRACT_VERSION in body["contract_versions"]
    assert "application/pdf" in body["formats"]
    assert "application/epub+zip" in body["formats"]
    assert body["processor"]["name"]
    assert body["features"]["markdown"] is True
    assert body["limits"]["max_concurrent_jobs"] >= 1


# ---------------------------------------------------------------------------
# 2. Repeated idempotency keys do not start duplicate processing
# ---------------------------------------------------------------------------
def test_idempotency_dedup(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    r1 = client.post(
        "/v1/process",
        json=_process_payload(src, "job-dedup-1", "key-dedup"),
        timeout=10,
    )
    assert r1.status_code == 202, r1.text
    assert r1.json()["deduplicated"] is False

    r2 = client.post(
        "/v1/process",
        json=_process_payload(src, "job-dedup-1", "key-dedup"),
        timeout=10,
    )
    assert r2.status_code == 202, r2.text
    assert r2.json()["deduplicated"] is True
    assert r2.json()["job_id"] == r1.json()["job_id"]


# ---------------------------------------------------------------------------
# 3. A known PDF produces Markdown and at least one chunk
# ---------------------------------------------------------------------------
def test_pdf_produces_markdown_and_chunks(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    r = client.post(
        "/v1/process", json=_process_payload(src, "job-pdf", "key-pdf"), timeout=10
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]
    body = _wait_completed(client, job_id)
    # G2: the API surface (not just the compute callback) exposes the live
    # stage; a completed job carries the last compute stage.
    assert body["stage"] == "assemble", body.get("stage")

    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10)
    assert res.status_code == 200, res.text
    result = res.json()
    assert result["status"] == "completed"
    assert len(result["chunks"]) >= 1

    ms = client.get(f"/v1/jobs/{job_id}/artifacts/markdown", timeout=10)
    assert ms.status_code == 200, ms.text
    assert len(ms.content) > 0


# ---------------------------------------------------------------------------
# 4. Chunk text, page labels, physical pages, section hierarchy survive round trip
# ---------------------------------------------------------------------------
def test_pdf_provenance_round_trip(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    r = client.post(
        "/v1/process", json=_process_payload(src, "job-prov", "key-prov"), timeout=10
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]
    _wait_completed(client, job_id)

    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10).json()
    chunk = res["chunks"][0]
    assert chunk["text"], "chunk must carry text"
    # Structure fields are present and well-typed.
    assert "structure" in chunk
    struct = chunk["structure"]
    assert "section_titles" in struct and isinstance(struct["section_titles"], list)
    assert isinstance(struct["start_paragraph_index"], int)
    assert isinstance(struct["end_paragraph_index"], int)
    # Page locator present with BOTH logical label and physical index for PDFs
    # (contract §11 requires both for page-based formats), and they must be
    # mutually consistent via the manifest page_label_map.
    loc = chunk["locator"]
    assert loc["type"] == "page_span", "pdf chunk must carry a page_span locator"
    labels = [ch["locator"]["page_label_start"] for ch in res["chunks"]]
    assert any(labels), "pdf chunks should carry page labels"
    assert loc["physical_page_start"] is not None
    assert loc["physical_page_end"] is not None
    # physical indexes fall within the page-label map's range.
    plm = res["manifest"]["page_label_map"]
    if plm:
        max_phys = max(int(k) for k in plm)
        assert loc["physical_page_start"] <= max_phys
        assert loc["physical_page_end"] <= max_phys


def test_chunker_section_hierarchy_from_markdown_headings():
    """Unit proof that the chunker emits a real ordered section hierarchy when
    the source markdown has headings (the API round-trip can't exercise this
    because pymupdf text extraction yields no markdown headings — that's a
    Marker feature, exercised by the real backend)."""
    from axiom_ng_runner.chunking import chunk_markdown

    md = (
        "{0}----------\n\n"
        "# Part One\n\nIntro paragraph.\n\n"
        "## Subsection A\n\nBody text here with some words.\n\n"
        "{1}----------\n\n"
        "# Part Two\n\nMore body text for the second part."
    )
    chunks: list[dict[str, Any]] = chunk_markdown(md, {0: "i", 1: "1"})
    assert chunks, "markdown with headings must produce chunks"
    hierarchies = [c["structure"]["section_titles"] for c in chunks]
    assert any(h for h in hierarchies), "section titles must be populated"
    # The chunk under "## Subsection A" carries the ordered hierarchy
    # ["Part One", "Subsection A"] — proving nested headings accumulate.
    sub = next(
        c for c in chunks if "Subsection A" in c["structure"]["section_titles"]
    )
    assert sub["structure"]["section_titles"] == ["Part One", "Subsection A"]


def test_epub_no_fabricated_pages(client, fixture_dirs):
    """Contract §11: EPUB (no stable page markers) must NOT get a fabricated
    page_span locator with invented page numbers."""
    src = fixture_dirs["epub"]
    payload = _process_payload(src, "job-epub-prov", "key-epub-prov")
    payload["attachment"]["content_type"] = "application/epub+zip"
    r = client.post(
        "/v1/process", json=payload, timeout=10
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]
    _wait_completed(client, job_id)

    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10).json()
    assert res["status"] == "completed"
    assert res["chunks"], "epub should produce chunks"
    for ch in res["chunks"]:
        loc = ch["locator"]
        assert loc["type"] == "epub_cfi", f"epub chunk must not fabricate pages: {loc}"
        # No invented page numbers.
        assert "page_label_start" not in loc
        assert "physical_page_start" not in loc


# ---------------------------------------------------------------------------
# 5. Source hash mismatch fails before successful output
# ---------------------------------------------------------------------------
def test_hash_mismatch_fails(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    payload = _process_payload(src, "job-badhash", "key-badhash")
    payload["attachment"]["content_hash"] = (
        "sha256:0000000000000000000000000000000000000000000000000000000000000000"
    )
    # Correct fail-fast behaviour: the processor rejects a mismatched hash
    # synchronously (422) before any processing, so a successful result is
    # impossible. (Synchronous rejection is allowed by contract §7.)
    r = client.post("/v1/process", json=payload, timeout=10)
    assert r.status_code in (202, 422), r.text
    if r.status_code == 202:
        body = _wait_completed(client, r.json()["job_id"])
        assert body["status"] == "failed"
        assert body["error"]["code"] == "SOURCE_HASH_MISMATCH"


# ---------------------------------------------------------------------------
# 6. All chunk/entity/relationship refs resolve within the result
# ---------------------------------------------------------------------------
def test_all_refs_resolve(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    r = client.post(
        "/v1/process", json=_process_payload(src, "job-refs", "key-refs"), timeout=10
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]
    _wait_completed(client, job_id)
    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10).json()

    chunk_refs = {ch["ref"] for ch in res["chunks"]}
    entity_refs = {e["ref"] for e in res["entities"]}

    for ch in res["chunks"]:
        for ir in ch.get("image_refs", []):
            # every referenced artifact ref must exist in artifacts
            assert any(a["ref"] == ir for a in res["artifacts"]), (
                f"image ref {ir} unresolved"
            )
    for e in res["entities"]:
        for m in e.get("mentions", []):
            assert m["chunk_ref"] in chunk_refs, (
                f"entity mention chunk {m['chunk_ref']} unresolved"
            )
    for rel in res["entity_relationships"]:
        assert rel["source_entity_ref"] in entity_refs
        assert rel["target_entity_ref"] in entity_refs
        for ev in rel.get("evidence_chunk_refs", []):
            assert ev in chunk_refs
    # §19.6 / §12: every non-sequential chunk_relationship must resolve its
    # refs and carry evidence chunk references.
    for rel in res.get("chunk_relationships", []):
        assert rel["source_chunk_ref"] in chunk_refs, (
            f"chunk rel source {rel['source_chunk_ref']} unresolved"
        )
        assert rel["target_chunk_ref"] in chunk_refs, (
            f"chunk rel target {rel['target_chunk_ref']} unresolved"
        )
        if rel.get("type") != "sequential_next":
            assert rel.get("evidence_chunk_refs"), (
                "non-sequential chunk relationship must carry evidence"
            )
            assert all(ev in chunk_refs for ev in rel["evidence_chunk_refs"])


# ---------------------------------------------------------------------------
# 7. Dense and sparse embeddings match declared capabilities
# ---------------------------------------------------------------------------
def test_embeddings_match_capabilities(client, fixture_dirs):
    caps = client.get("/v1/capabilities", timeout=10).json()
    dense_model = caps["models"]["dense_embedding"]["name"]
    cap_dims = caps["models"]["dense_embedding"]["dimensions"]
    assert isinstance(cap_dims, int), "capability dimensions must be an int (contract §6)"

    src = fixture_dirs["pdf"]
    r = client.post(
        "/v1/process", json=_process_payload(src, "job-emb", "key-emb"), timeout=10
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]
    _wait_completed(client, job_id)
    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10).json()

    for ch in res["chunks"]:
        emb = ch.get("embeddings", {})
        if "dense" in emb:
            d = emb["dense"]
            assert d["model"] == dense_model
            assert isinstance(d["dimensions"], int)
            # §19.7: dense dimensions must match the declared capability.
            assert d["dimensions"] == cap_dims, (
                f"chunk dims {d['dimensions']} != capability dims {cap_dims}"
            )
            assert len(d["values"]) == d["dimensions"]
            assert all(isinstance(v, (int, float)) for v in d["values"])
        if "sparse" in emb:
            s = emb["sparse"]
            assert isinstance(s["values"], dict)
            assert all(
                isinstance(k, str) and isinstance(v, (int, float))
                for k, v in s["values"].items()
            )


# ---------------------------------------------------------------------------
# 8. No PostgreSQL or OpenSearch writes
# ---------------------------------------------------------------------------
def test_no_durable_store_access(fixture_dirs):
    """Contract §19.8: the reference compute path must not import any durable
    store — verified by a real import-trace (sys.modules), not a text grep.

    A text grep misses transitive imports (``from axiom_ng_runner.compute_core import
    ...`` -> sqlalchemy/pgvector) [historical: pre-vendor-move example]. This snapshot-run-assert test catches them:
    we record sys.modules, run the reference compute end-to-end, then assert
    none of the banned durable-store modules were loaded as a side-effect.
    """
    import sys

    banned_prefixes = (
        "sqlalchemy",
        "psycopg",
        "asyncpg",
        "opensearch",
        "pgvector",
        "zotero",
    )

    # Pre-import the app so baseline modules are already loaded; only the
    # compute path's NEW imports matter.
    import axiom_ng_runner.app  # noqa: F401

    before = set(sys.modules)

    # Run the reference compute on a tiny fixture.
    src = fixture_dirs["pdf"]
    work_dir = fixture_dirs["work"] / "no_store_work"
    work_dir.mkdir(parents=True, exist_ok=True)
    payload = _process_payload(src, "job-nostore", "key-nostore")
    from axiom_ng_runner.runner import compute

    result = compute(payload, work_dir)
    assert result["status"] == "completed"

    after = set(sys.modules)
    loaded = sorted(m for m in (after - before) if m.split(".")[0] in banned_prefixes)
    assert not loaded, (
        "reference compute loaded durable-store modules: " + ", ".join(loaded)
    )


# ---------------------------------------------------------------------------
# 9. Cancellation terminates a genuinely in-flight job (cooperative cancel)
# ---------------------------------------------------------------------------
def test_cancellation_terminates_inflight_job(client, fixture_dirs, monkeypatch):
    """§19.9 (strengthened): cancellation must terminate a genuinely in-flight
    job, not just be accepted on a near-instant reference compute.

    We monkeypatch the compute function with a cooperative long-running one
    that polls the job's status and aborts when it observes ``cancelled`` —
    exactly the cooperative-cancel contract (§17). The cancel endpoint marks
    the job cancelled; the in-flight compute observes it and stops without
    producing a result, so the job settles to ``cancelled`` (not completed).
    """
    import axiom_ng_runner.app as appmod
    import axiom_ng_runner.runner as runnermod

    cancel_seen: dict[str, bool] = {"seen": False}

    def slow_cooperative_compute(request, work_dir, set_stage=None, commit=None, set_progress=None):
        # Simulate a long compute that cooperatively polls for cancellation.
        # Read status through the app's store singleton so we observe the
        # cancel endpoint's in-memory transition (a fresh JobStore would cache
        # the job at construction and miss the live update).
        store = appmod._store_impl()
        jid = request["job_id"]
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            j = store.get(jid)
            if j is not None and j.status == "cancelled":
                cancel_seen["seen"] = True
                raise RuntimeError("cancelled mid-compute")
            time.sleep(0.02)
        # If we never got cancelled, produce a normal result.
        return {"status": "completed", "chunks": []}

    monkeypatch.setattr(runnermod, "compute", slow_cooperative_compute)

    src = fixture_dirs["pdf"]
    r = client.post(
        "/v1/process",
        json=_process_payload(src, "job-cancel-live", "key-cancel-live"),
        timeout=10,
    )
    assert r.status_code == 202, r.text
    job_id = r.json()["job_id"]

    # Give the compute thread a moment to enter its loop, then cancel.
    time.sleep(0.1)
    cr = client.post(f"/v1/jobs/{job_id}/cancel", timeout=10)
    assert cr.status_code == 200

    body = _wait_completed(client, job_id, timeout=30)
    assert body["status"] == "cancelled", (
        f"in-flight job must settle to cancelled, got {body['status']}"
    )
    # The cooperative compute thread sets cancel_seen asynchronously when it
    # observes the cancel; give it a bounded window (it runs in a daemon
    # thread and may be scheduled slightly after the cancel lands).
    for _ in range(100):
        if cancel_seen["seen"]:
            break
        time.sleep(0.02)
    assert cancel_seen["seen"], "cooperative compute must have observed the cancel"
    # A cancelled job must not serve a completed result.
    res = client.get(f"/v1/jobs/{job_id}/result", timeout=10)
    assert res.status_code != 200


def test_cancel_wins_over_late_result(fixture_dirs):
    """Store-level invariant: a late compute result/error must not overwrite a
    cancellation. Tested directly at the store because the reference backend
    computes too fast to race reliably.
    """
    import uuid

    work = fixture_dirs["work"] / "cancel_win_work"
    work.mkdir(parents=True, exist_ok=True)
    jid = f"job-cancelled-{uuid.uuid4().hex[:8]}"
    store = JobStore(work)
    job = Job(
        job_id=jid,
        idempotency_key="k-cancel-win",
        request={},
        path=work / jid,
        status="running",
    )
    store.put(job)

    store.set_status(job, "cancelled", stage="")
    assert job.status == "cancelled"

    # A late result / error arrives from the (now-obsolete) compute thread.
    store.set_result(job, {"status": "completed", "chunks": []})
    store.set_error(job, "INTERNAL_ERROR", "late", retryable=True)

    assert job.status == "cancelled", "cancellation must win over a late result"
    assert job.result is None
    assert job.error is None


# ---------------------------------------------------------------------------
# 10. Acknowledgement removes temporary output and is idempotent
# ---------------------------------------------------------------------------
def test_ack_removes_temp_output_and_is_idempotent(client, fixture_dirs):
    tmp_work = fixture_dirs["work"] / "ack_work"
    tmp_work.mkdir(parents=True, exist_ok=True)
    old = settings.get()
    settings.set(
        Settings(
            work_root=tmp_work, allowed_source_roots=(str(fixture_dirs["sources"]),)
        )
    )
    try:
        with TestClient(app) as c:
            src = fixture_dirs["pdf"]
            r = c.post(
                "/v1/process",
                json=_process_payload(src, "job-ack", "key-ack"),
                timeout=10,
            )
            assert r.status_code == 202, r.text
            job_id = r.json()["job_id"]
            _wait_completed(c, job_id)
            job_dir = tmp_work / job_id
            assert job_dir.exists(), "job temp dir should exist before ack"
            assert (job_dir / "manifest.json").exists()
            work_dir = job_dir / "work"
            assert work_dir.exists(), (
                "temp work dir with compute output should exist before ack"
            )
            assert (work_dir / "markdown.md").exists()

            ack = c.post(
                f"/v1/jobs/{job_id}/ack",
                json={"persisted": True, "snapshot_id": "snap-1"},
                timeout=10,
            )
            assert ack.status_code == 200, ack.text
            # Temporary compute output removed after ack (the durable manifest
            # tombstone may remain so a repeated ack stays idempotent).
            assert not work_dir.exists(), "temp work output should be removed after ack"
            # Idempotent.
            ack2 = c.post(
                f"/v1/jobs/{job_id}/ack",
                json={"persisted": True, "snapshot_id": "snap-1"},
                timeout=10,
            )
            assert ack2.status_code == 200
    finally:
        settings.set(old)


# ---------------------------------------------------------------------------
# 11. Processor restart does not silently convert an accepted job into success
# ---------------------------------------------------------------------------
def test_restart_recovers_without_fake_success(fixture_dirs):
    """§19.11 (strengthened): a restart must (a) NOT fabricate success for an
    accepted-but-uncommitted job, AND (b) re-launch that job so it actually
    reaches completion (liveness — invariant #10). A completed-but-unacked
    job must stay completed (no downgrade)."""
    import axiom_ng_runner.app as appmod

    work = fixture_dirs["work"] / "restart_work"
    work.mkdir(parents=True, exist_ok=True)
    src = fixture_dirs["pdf"]

    old = settings.get()
    settings.set(
        Settings(work_root=work, allowed_source_roots=(str(fixture_dirs["sources"]),))
    )
    try:
        # Phase 1: accept a job and let it complete normally.
        with TestClient(app) as c:
            r = c.post(
                "/v1/process",
                json=_process_payload(src, "job-restart-done", "key-restart-done"),
                timeout=10,
            )
            assert r.status_code == 202, r.text
            done_id = r.json()["job_id"]
            _wait_completed(c, done_id)

        # Phase 2: seed a SEPARATE accepted-but-unrun job manifest on disk, as a
        # crashed previous process would have left it (accepted, no result).
        seed_work = work / "job-restart-pending"
        seed_work.mkdir(parents=True, exist_ok=True)
        pending_payload = _process_payload(src, "job-restart-pending", "key-pending")
        (seed_work / "manifest.json").write_text(
            json.dumps(
                {
                    "job_id": "job-restart-pending",
                    "idempotency_key": "key-pending",
                    "request": pending_payload,
                    "status": "accepted",
                    "stage": "",
                    "result": None,
                    "error": None,
                    "acked": False,
                    "created_at": 0.0,
                    "updated_at": 0.0,
                }
            ),
            encoding="utf-8",
        )

        # Phase 3: simulate a fresh process start — force the app store to
        # rebuild over the same work root, which triggers relaunch of recovered
        # non-terminal jobs (the W1 fix).
        appmod._store = None  # force rebuild on next _store_impl()
        with TestClient(app) as c:
            # Reaching into the store rebuilds it and relaunches the pending job.
            store = appmod._store_impl()
            recovered_done = store.get(done_id)
            recovered_pending = store.get("job-restart-pending")
            assert recovered_done is not None and recovered_pending is not None
            # (a) No fabrication: completed job stays completed.
            assert recovered_done.status == "completed"
            # (b) Liveness: the accepted job is re-run to completion (invariant #10).
            final = _wait_completed(c, "job-restart-pending", timeout=30)
            assert final["status"] == "completed", (
                "recovered accepted job must be re-queued to completion after restart"
            )
    finally:
        appmod._store = None  # reset for other tests
        settings.set(old)


# ---------------------------------------------------------------------------
# 12. No durable copy of the original PDF/EPUB remains after acknowledgement
# ---------------------------------------------------------------------------
def test_no_durable_source_copy_after_ack(client, fixture_dirs):
    src = fixture_dirs["pdf"]
    src_bytes = src.read_bytes()

    tmp_work = fixture_dirs["work"] / "nocopy_work"
    tmp_work.mkdir(parents=True, exist_ok=True)
    old = settings.get()
    settings.set(
        Settings(
            work_root=tmp_work, allowed_source_roots=(str(fixture_dirs["sources"]),)
        )
    )
    try:
        with TestClient(app) as c:
            r = c.post(
                "/v1/process",
                json=_process_payload(src, "job-nocopy", "key-nocopy"),
                timeout=10,
            )
            assert r.status_code == 202, r.text
            job_id = r.json()["job_id"]
            _wait_completed(c, job_id)
            c.post(
                f"/v1/jobs/{job_id}/ack",
                json={"persisted": True, "snapshot_id": "snap-n"},
                timeout=10,
            )

            # Nothing durable anywhere under the work root may equal the source bytes.
            found = 0
            for f in tmp_work.rglob("*"):
                if (
                    f.is_file()
                    and f.suffix in (".pdf", ".epub")
                    and f.read_bytes() == src_bytes
                ):
                    found += 1
            assert found == 0, "durable copy of source remained after ack"
    finally:
        settings.set(old)


# ---------------------------------------------------------------------------
# 10. Stage progression: set_stage callback + manifest.stage_timings (§9)
# ---------------------------------------------------------------------------


def test_stage_progression_and_manifest_timings(fixture_dirs):
    """Issue #122: compute must advance the live job stage through the
    contract stages and persist per-stage UTC timestamps into the manifest
    so phase durations are reconstructable after job end."""
    from axiom_ng_runner.runner import compute

    src = fixture_dirs["pdf"]
    work_dir = fixture_dirs["work"] / "stage_work"
    work_dir.mkdir(parents=True, exist_ok=True)
    payload = _process_payload(src, "job-stage", "key-stage")

    seen: list[str] = []
    result = compute(payload, work_dir, set_stage=seen.append)

    # Live progression covers every contract stage in order, derived from
    # the single source of truth (PIPELINE_STAGES minus app.py's
    # validate_source, which fires before compute starts).
    assert seen == list(PIPELINE_STAGES)[1:]

    # Manifest carries parseable UTC timestamps for each stage.
    from datetime import datetime

    timings = result["manifest"]["stage_timings"]
    assert set(timings) == set(seen)
    stamps = [datetime.fromisoformat(timings[s]) for s in seen]
    assert all(st.tzinfo is not None for st in stamps), "timestamps must be tz-aware"
    assert stamps == sorted(stamps), "stage timestamps must be monotonically ordered"


def test_stage_progression_without_callback(fixture_dirs):
    """set_stage stays optional: compute must run unchanged (callers that
    don't observe stages, e.g. ad-hoc scripts, keep working)."""
    from axiom_ng_runner.runner import compute

    src = fixture_dirs["pdf"]
    work_dir = fixture_dirs["work"] / "stage_nocb"
    work_dir.mkdir(parents=True, exist_ok=True)
    payload = _process_payload(src, "job-stage2", "key-stage2")

    result = compute(payload, work_dir)
    assert result["status"] == "completed"
    assert "stage_timings" in result["manifest"]


# ---------------------------------------------------------------------------
# 11. EPUB image-path normalization (#124): no machine paths in chunk texts
# ---------------------------------------------------------------------------


def test_normalize_epub_image_paths_strips_machine_paths():
    """Issue #124: the epub worker's temp dir carries a random suffix; both
    markdown image links AND raw HTML <img src> must lose the machine path
    before chunking, or chunk texts differ on every re-run (TC2: Sonko)."""
    from axiom_ng_runner.runner import _normalize_epub_image_paths

    md = (
        '# Kapitel\n\n'
        '<img src="/tmp/epub_media_k97091zv/images/532180_1_En_1_Chapter/'
        '532180_1_En_1_Fig1_HTML.png" style="width:31.9em" aria-describedby="d65e473" />\n\n'
        'Text davor ![Fig 2](/tmp/epub_media_pg911a7r/images/ch2/fig2.png) Text danach\n\n'
        '<img src="/tmp/epub_media_qx7wv2mn/images/fig9.png" />\n'
    )
    out = _normalize_epub_image_paths(md)
    # Keine Maschinenpfade, keine Zufallssuffixe — in keiner Form.
    assert "/tmp/" not in out
    assert "epub_media" not in out
    # Beide Formen tragen den stabilen Basename…
    assert 'src="532180_1_En_1_Fig1_HTML.png"' in out
    assert "![Fig 2](fig2.png)" in out
    # …und JEDES <img>-Vorkommen (auch das zweite mit anderem Temp-Suffix).
    assert 'src="fig9.png"' in out
    # …und alle anderen Attribute/Inhalte bleiben unberührt.
    assert 'style="width:31.9em"' in out
    assert "aria-describedby" in out
    assert out.startswith("# Kapitel")


def test_real_pipeline_calls_image_path_normalization():
    """#124 shipping guard: the EPUB pipeline branch needs heavy deps (pandoc,
    marker) and never runs in this suite — when a stray mutation dropped the
    pipeline's normalization call, the whole suite stayed green (rsync-built
    images ship the working tree verbatim). Pin the call site by source
    inspection."""
    import axiom_ng_runner.runner as runner_mod

    src = Path(runner_mod.__file__).read_text(encoding="utf-8")
    assert "_normalize_epub_image_paths(markdown)" in src


# ---------------------------------------------------------------------------
# 12. Replay-after-ACK: resubmit of an acknowledged job is terminal (#126)
# ---------------------------------------------------------------------------


def test_resubmit_after_ack_returns_artifacts_expired(client, fixture_dirs):
    """Issue #126: the dedup resubmit of an ACKed job must NOT hand back the
    stored result (its artifacts died with the ACK, §19.12) — that sent the
    dispatcher into an artifact-404 retry wall in production. Terminal 409
    with a parseable code instead."""
    src = fixture_dirs["pdf"]
    payload = _process_payload(src, "job-ackexp", "key-ackexp")

    r1 = client.post("/v1/process", json=payload, timeout=10)
    assert r1.status_code == 202, r1.text
    job_id = r1.json()["job_id"]
    _wait_completed(client, job_id, timeout=30)

    ack = client.post(
        f"/v1/jobs/{job_id}/ack",
        json={"persisted": True, "snapshot_id": "snap-ackexp"},
        timeout=10,
    )
    assert ack.status_code == 200, ack.text

    r2 = client.post("/v1/process", json=payload, timeout=10)
    assert r2.status_code == 409, r2.text
    detail = r2.json()["detail"]
    assert detail["code"] == "ARTIFACTS_EXPIRED"
    assert detail["retryable"] is False


def test_resubmit_after_ack_survives_runner_restart(fixture_dirs):
    """#126 restart durability (the production-incident shape): the acked
    flag lives in the durable manifest, so a runner restart (store rebuilt
    from disk) must keep answering ARTIFACTS_EXPIRED — a restart must never
    resurrect a dedup path that hands back the dead result."""
    import axiom_ng_runner.app as appmod

    work = fixture_dirs["work"] / "ackexp_restart"
    work.mkdir(parents=True, exist_ok=True)
    src = fixture_dirs["pdf"]
    payload = _process_payload(src, "job-ackexp-restart", "key-ackexp-restart")

    old = settings.get()
    settings.set(
        Settings(work_root=work, allowed_source_roots=(str(fixture_dirs["sources"]),))
    )
    try:
        with TestClient(app) as c:
            r1 = c.post("/v1/process", json=payload, timeout=10)
            assert r1.status_code == 202, r1.text
            job_id = r1.json()["job_id"]
            _wait_completed(c, job_id, timeout=30)
            ack = c.post(
                f"/v1/jobs/{job_id}/ack",
                json={"persisted": True, "snapshot_id": "snap-ackexp-restart"},
                timeout=10,
            )
            assert ack.status_code == 200, ack.text

        # Simulate a fresh process start over the same work root (the
        # test_restart_recovers_without_fake_success pattern): the store
        # rebuilds from the durable manifests — the acked flag must survive.
        appmod._store = None
        with TestClient(app) as c:
            store = appmod._store_impl()  # rebuild from disk
            recovered = store.get(job_id)
            assert recovered is not None and recovered.acked, (
                "acked flag must survive the store rebuild from disk"
            )
            r2 = c.post("/v1/process", json=payload, timeout=10)
            assert r2.status_code == 409, r2.text
            detail = r2.json()["detail"]
            assert detail["code"] == "ARTIFACTS_EXPIRED"
            assert detail["retryable"] is False
    finally:
        appmod._store = None  # reset for other tests
        settings.set(old)


def test_resubmit_before_ack_still_deduplicates(client, fixture_dirs):
    """The un-acked dedup path is unchanged: 202 + deduplicated=true (§19.2)."""
    src = fixture_dirs["pdf"]
    payload = _process_payload(src, "job-acklive", "key-acklive")
    r1 = client.post("/v1/process", json=payload, timeout=10)
    assert r1.status_code == 202
    r2 = client.post("/v1/process", json=payload, timeout=10)
    assert r2.status_code == 202
    assert r2.json()["deduplicated"] is True
