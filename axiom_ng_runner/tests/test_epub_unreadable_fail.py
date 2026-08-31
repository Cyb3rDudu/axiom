"""#237 — unreadable EPUB: structured FAIL instead of runner 500/retry storm.

Pins the acceptance behavior: a deliberately corrupt EPUB (truncated zip)
makes the convert stage raise SourceError("SOURCE_UNREADABLE") — the
structured terminal FAIL contract (error class, stage=convert,
retryable=false, evidence excerpt) that the dispatcher maps onto a repair
case. An infrastructure-shaped child failure (signal/OOM) keeps the
INTERNAL_ERROR retryable path.

Run: .venv/bin/python -m pytest tests/test_epub_unreadable_fail.py
"""
from __future__ import annotations

import hashlib
import time
import zipfile
from pathlib import Path

import pytest

# CI light stack: runner.py imports compute_core.relation_extractor (mREBEL)
# at module level, which needs torch — skip collection there.
pytest.importorskip("torch")

from axiom_ng_runner import runner as runnermod
from axiom_ng_runner.app import app
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.validation import SourceError
from fastapi.testclient import TestClient


def _epub_request(tmp_path: Path, epub_path: Path) -> dict:
    return {
        "job_id": "job-237",
        "idempotency_key": "idem-237",
        "attachment": {
            "attachment_id": "a1",
            "local_path": str(epub_path),
            "content_type": "application/epub+zip",
            "filename": "broken.epub",
        },
        "processing": {"profile": "full-rag-v1"},
    }


def _truncated_epub(tmp_path: Path) -> Path:
    """A valid EPUB prefix, cut mid-central-directory — a corrupt zip."""
    good = tmp_path / "good.epub"
    with zipfile.ZipFile(good, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("OEBPS/ch1.xhtml", "<html><body>hi</body></html>")
    raw = good.read_bytes()
    broken = tmp_path / "broken.epub"
    broken.write_bytes(raw[: len(raw) // 2])
    return broken


def test_corrupt_epub_raises_structured_source_unreadable(tmp_path):
    epub = _truncated_epub(tmp_path)
    with pytest.raises(SourceError) as exc_info:
        runnermod._real_pipeline(_epub_request(tmp_path, epub), tmp_path)
    assert exc_info.value.code == "SOURCE_UNREADABLE"
    # Evidence excerpt: the worker's JSON error line rides in the message.
    assert "epub_worker" in exc_info.value.message


def test_signal_failure_stays_infra_error(tmp_path, monkeypatch):
    """A SIGKILLed child (OOM) is infrastructure, not a document defect."""
    epub = _truncated_epub(tmp_path)

    class _Proc:
        returncode = -9
        stdout = ""
        stderr = ""

    monkeypatch.setattr("subprocess.run", lambda *a, **k: _Proc())
    with pytest.raises(RuntimeError) as exc_info:
        runnermod._real_pipeline(_epub_request(tmp_path, epub), tmp_path)
    assert "CHILD_OOM_SIGKILL" in str(exc_info.value)


# --- app level: the runner answers the structured FAIL contract -----------

def test_corrupt_epub_via_http_reports_structured_fail(tmp_path):
    """#237 acceptance: POST /v1/process with a truncated-zip EPUB is
    accepted, then GET /v1/jobs/{id} reports failed with error class
    SOURCE_UNREADABLE, retryable=false, stage=convert — never an unhandled
    500 and never a retryable infra error."""
    epub = _truncated_epub(tmp_path)
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work",
                          allowed_source_roots=(str(tmp_path),),
                          compute_backend="real",
                          warmup=False))
    payload = {
        "contract_version": "1.0",
        "job_id": "job-epub-237",
        "idempotency_key": "key-epub-237",
        "source": {"type": "zotero", "source_id": "src-1", "server_id": "srv"},
        "document": {"document_id": "doc-1", "zotero_key": "ZK",
                     "zotero_version": 1,
                     "metadata_snapshot": {"itemType": "book"}},
        "attachment": {
            "attachment_id": "att-1", "zotero_key": "AK", "zotero_version": 1,
            "content_type": "application/epub+zip", "filename": "broken.epub",
            "local_path": str(epub),
            "content_hash": hashlib.sha256(epub.read_bytes()).hexdigest(),
            "size_bytes": epub.stat().st_size, "mtime_ms": 0,
        },
        "processing": {"profile": "full-rag-v1", "force_rebuild": False,
                       "extract_entities": False,
                       "extract_relationships": False,
                       "compute_dense_embeddings": False,
                       "compute_sparse_embeddings": False},
    }
    try:
        with TestClient(app) as c:
            acc = c.post("/v1/process", json=payload)
            assert acc.status_code == 202, acc.text
            deadline = time.monotonic() + 30
            body = None
            while time.monotonic() < deadline:
                body = c.get("/v1/jobs/job-epub-237").json()
                if body["status"] == "failed":
                    break
                time.sleep(0.2)
            assert body and body["status"] == "failed", body
            err = body["error"]
            assert err["code"] == "SOURCE_UNREADABLE", err
            assert err["retryable"] is False, err
            assert err["stage"] == "convert", err
    finally:
        settings.set(old)
