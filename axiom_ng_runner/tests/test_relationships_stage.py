"""#225 — relationships stage: early-commit, progress, stage budget.

Pins the three acceptance behaviors without the heavy models:
- the batch loop reports done/total per batch and honors the deadline
  (STAGE_BUDGET_EXCEEDED → honest partial completion, never 0/0 blindness)
- set_partial/set_progress keep the job running while the partial result
  is committed (no dispatcher race)
- an app-level run with a monkeypatched compute proves the early-commit
  contract: after a relationships-stage abort the chunks are retrievable
  via GET /v1/jobs/{id}/result and the status names the reason.

Run: .venv/bin/python -m pytest tests/test_relationships_stage.py
"""
from __future__ import annotations

import time

import pytest
from axiom_ng_runner import runner as runnermod
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.job_store import Job, JobStore
from fastapi.testclient import TestClient


# --- batch loop: progress + budget ----------------------------------------


def _fake_extractor(calls, triples, delay=0.0):
    def fn(chunks):
        calls.append([c.get("metadata", {}).get("chunk_id") for c in chunks])
        if delay:
            time.sleep(delay)
        return list(triples)
    return fn


def test_batch_progress_reported(monkeypatch):
    calls: list = []
    monkeypatch.setattr(
        "axiom_ng_runner.compute_core.relation_extractor.extract_relations_from_chunks",
        _fake_extractor(calls, [
            {"head": "A", "head_type": "c", "tail": "B", "tail_type": "c",
             "relation": "r", "chunk_id": "chunk-0001"},
        ]),
    )
    seen: list[tuple[int, int]] = []
    chunks = [{"text": "x" * 60, "metadata": {"chunk_id": f"chunk-{i:04d}"}}
              for i in range(45)]
    rels, exceeded = runnermod._extract_real_relationships(
        [], chunks, {}, on_progress=lambda d, t: seen.append((d, t)))
    assert not exceeded
    assert seen == [(20, 45), (40, 45), (45, 45)]  # per-batch done/total
    assert len(calls) == 3  # batches of 20/20/5
    assert rels and rels[0]["evidence_chunk_refs"] == ["chunk-0001"]


def test_budget_deadline_stops_between_batches(monkeypatch):
    calls: list = []
    monkeypatch.setattr(
        "axiom_ng_runner.compute_core.relation_extractor.extract_relations_from_chunks",
        _fake_extractor(calls, [], delay=0.1),
    )
    chunks = [{"text": "x" * 60, "metadata": {"chunk_id": f"chunk-{i:04d}"}}
              for i in range(60)]
    seen: list[tuple[int, int]] = []
    rels, exceeded = runnermod._extract_real_relationships(
        [], chunks, {}, deadline=time.monotonic() + 0.15,
        on_progress=lambda d, t: seen.append((d, t)))
    assert exceeded is True
    assert len(calls) < 3  # stopped between batches, not after all 60
    assert len(seen) == len(calls)  # progress consistent with batches run


def test_budget_zero_disables_deadline(monkeypatch):
    calls: list = []
    monkeypatch.setattr(
        "axiom_ng_runner.compute_core.relation_extractor.extract_relations_from_chunks",
        _fake_extractor(calls, []),
    )
    rels, exceeded = runnermod._extract_real_relationships(
        [], [{"text": "x" * 60, "metadata": {"chunk_id": "c"}}] * 45,
        {}, deadline=None)
    assert exceeded is False and len(calls) == 3


# --- store: partial + progress --------------------------------------------


def test_set_partial_keeps_job_running(tmp_path):
    store = JobStore(tmp_path)
    job = Job(job_id="j1", idempotency_key="k1", request={}, path=tmp_path / "j1")
    store.get_or_create(job)
    store.set_status(job, "running", stage="relationships")
    store.set_partial(job, {"status": "completed", "chunks": [{"ref": "chunk-0000"}]})
    assert job.status == "running"  # no terminal flip → no dispatcher race
    assert job.partial is True and job.result["chunks"]
    # progress only lands while running
    store.set_progress(job, 20, 45, "chunks")
    assert job.progress == {"completed_units": 20, "total_units": 45, "unit": "chunks"}
    store.set_result(job, {"status": "completed", "chunks": [{"ref": "chunk-0000"}],
                           "manifest": {}})
    store.set_progress(job, 30, 45, "chunks")
    assert job.progress["completed_units"] == 20  # terminal: frozen
    # disk round-trip preserves partial + progress
    reloaded = Job.load(job.path)
    assert reloaded.partial is True and reloaded.progress["total_units"] == 45


# --- app level: early-commit contract -------------------------------------


@pytest.fixture()
def client(tmp_path):
    src_root = tmp_path / "sources"
    src_root.mkdir()
    (src_root / "book.pdf").write_bytes(b"%PDF-fake")
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work",
                          allowed_source_roots=(str(src_root),)))
    try:
        with TestClient(app) as c:
            c.sources_root = str(src_root / "book.pdf")
            yield c
    finally:
        settings.set(old)


def _sha256_of(path: str) -> str:
    import hashlib
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


def _payload(src: str, job_id: str) -> dict:
    return {
        "contract_version": "1.0",
        "job_id": job_id,
        "idempotency_key": f"key-{job_id}",
        "source": {"type": "zotero", "source_id": "src-1", "server_id": "srv"},
        "document": {
            "document_id": "doc-1",
            "zotero_key": "ZK",
            "zotero_version": 1,
            "metadata_snapshot": {"itemType": "book", "title": "S", "date": "2024"},
        },
        "attachment": {
            "attachment_id": "att-1",
            "zotero_key": "AK",
            "zotero_version": 1,
            "content_type": "application/pdf",
            "filename": "book.pdf",
            "local_path": src,
            "content_hash": _sha256_of(src),
            "size_bytes": 1,
            "mtime_ms": 0,
        },
        "processing": {
            "profile": "full-rag-v1",
            "force_rebuild": False,
            "extract_entities": False,
            "extract_relationships": True,
            "compute_dense_embeddings": False,
            "compute_sparse_embeddings": False,
        },
    }


def test_early_commit_survives_relationships_abort(client, monkeypatch):
    """#225 DoD: chunks committed BEFORE relationships; a stage abort
    leaves them retrievable with the named reason — never a total loss."""

    def fake_compute(request, work_dir, set_stage=None, commit=None, set_progress=None):
        set_stage("chunk")
        result = {
            "contract_version": "1.0",
            "status": "completed",
            "job_id": request["job_id"],
            "chunks": [{"ref": "chunk-0000", "index": 0, "text": "Kapiteltext"}],
            "manifest": {"stage_completion": {"relationships": False}},
        }
        commit(result)  # the early-commit under test
        set_stage("relationships")
        set_progress(20, 45, "chunks")
        time.sleep(0.3)  # keep the running window observable
        # budget exceeded → honest partial completion (the #225 path)
        result["manifest"]["stage_completion"]["relationships_reason"] = "STAGE_BUDGET_EXCEEDED"
        return result

    monkeypatch.setattr(runnermod, "compute", fake_compute)
    src = client.sources_root
    r = client.post("/v1/process", json=_payload(src, "job-225-early"))
    assert r.status_code == 202, r.text
    jid = r.json()["job_id"]

    # while running: progress visible, partial flagged (poll until the
    # compute thread reaches the relationships window)
    st = None
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        st = client.get(f"/v1/jobs/{jid}").json()
        if st.get("partial_result_available"):
            break
        time.sleep(0.02)
    assert st["progress"]["completed_units"] == 20
    assert st["progress"]["total_units"] == 45
    assert st["progress"]["unit"] == "chunks"
    assert st["partial_result_available"] is True

    # settle: completed with the partial result — chunks retrievable
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        st = client.get(f"/v1/jobs/{jid}").json()
        if st["status"] == "completed":
            break
        time.sleep(0.05)
    assert st["status"] == "completed"
    res = client.get(f"/v1/jobs/{jid}/result")
    assert res.status_code == 200
    body = res.json()
    assert body["chunks"][0]["ref"] == "chunk-0000"
    assert body["manifest"]["stage_completion"]["relationships_reason"] == "STAGE_BUDGET_EXCEEDED"


def test_crash_after_commit_leaves_partial_visible(client, monkeypatch):
    """Hard failure after the early commit: the job fails (contract §9 —
    result endpoint stays completed-only) but the status names the partial
    result so operators/E2E know the work is not lost."""

    def fake_compute(request, work_dir, set_stage=None, commit=None, set_progress=None):
        commit({"status": "completed", "chunks": [{"ref": "chunk-0000"}]})
        raise RuntimeError("mREBEL worker died")

    monkeypatch.setattr(runnermod, "compute", fake_compute)
    src = client.sources_root
    r = client.post("/v1/process", json=_payload(src, "job-225-crash"))
    jid = r.json()["job_id"]
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        st = client.get(f"/v1/jobs/{jid}").json()
        if st["status"] == "failed":
            break
        time.sleep(0.05)
    assert st["status"] == "failed"
    assert st["partial_result_available"] is True
    assert st["error"]["code"] == "INTERNAL_ERROR"
