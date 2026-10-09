"""#369 cancelled-entry relaunch: a dedup match on a CANCELLED job entry
relaunches instead of answering terminal.

The dispatcher's no-progress watchdog cancels a stalled runner job before it
walks away (orphan compute stop); the claim scan then evicts and the reclaim
resubmits under the SAME idempotency key. Without the relaunch, that resubmit
would dedup onto the dead cancelled entry and the dispatcher's poll loop
would see a terminal "cancelled" — the retry the eviction policy promised
would silently degrade to a cancel.

Hermetic like test_dedup_rekey: compute is a blocked thread, no heavy models.
"""

from __future__ import annotations

import hashlib
import threading

from fastapi.testclient import TestClient

from axiom_compute_worker import app as appmod
from axiom_compute_worker.config import Settings, settings
from axiom_compute_worker.runtime import JobRuntime


def _payload(src, key: str) -> dict:
    return {
        "contract_version": "1.0",
        "job_id": "job-evicted",
        "idempotency_key": key,
        "source": {"type": "zotero", "source_id": "src-1", "server_id": "srv"},
        "document": {"document_id": "doc-1", "zotero_key": "ZK",
                     "zotero_version": 1,
                     "metadata_snapshot": {"itemType": "book"}},
        "attachment": {
            "attachment_id": "att-1", "zotero_key": "AK", "zotero_version": 1,
            "content_type": "application/pdf", "filename": src.name,
            "local_path": str(src),
            "content_hash": "sha256:" + hashlib.sha256(src.read_bytes()).hexdigest(),
            "size_bytes": src.stat().st_size, "mtime_ms": 0,
        },
        "processing": {"profile": "full-rag-v1", "force_rebuild": False,
                       "extract_images": False,
                       "compute_dense_embeddings": False,
                       "compute_sparse_embeddings": False,
                       "extract_entities": False,
                       "extract_relationships": False},
    }


def test_resubmit_after_watchdog_cancel_relaunches(tmp_path, monkeypatch):
    src = tmp_path / "doc.pdf"
    src.write_bytes(b"%PDF-1.4 smoke")

    started = threading.Event()
    release = threading.Event()
    runs = []

    def _blocked(rt: JobRuntime) -> None:
        runs.append(rt.job_id)
        started.set()
        release.wait(10)

    monkeypatch.setattr(appmod, "_run_compute", _blocked)
    old = settings.get()
    settings.set(Settings(work_root=tmp_path / "work",
                          allowed_source_roots=(str(tmp_path),),
                          compute_backend="reference", warmup=False,
                          max_concurrent_jobs=1, admission_queue_capacity=9))
    try:
        with TestClient(appmod.app) as client:
            # 1. Original claim: accepted, compute starts (blocked).
            r1 = client.post("/v1/process", json=_payload(src, "k-evict"))
            assert r1.status_code == 202, r1.text
            assert started.wait(5), "compute never started"

            # 2. The watchdog's orphan-compute stop lands: entry cancelled.
            r_cancel = client.post("/v1/jobs/job-evicted/cancel")
            assert r_cancel.status_code == 200, r_cancel.text
            assert r_cancel.json()["status"] == "cancelled"
            release.set()  # the (already-doomed) compute thread winds down
            # In production the reclaim lands >= a lease window after the
            # cancel; mirror that: wait until the cancelling runtime is no
            # longer tracked, so the relaunch decision sees a settled entry.
            deadline = __import__("time").monotonic() + 5
            while appmod._scheduler().is_relevant("job-evicted"):
                assert __import__("time").monotonic() < deadline, (
                    "cancelled compute never left the scheduler"
                )
                __import__("time").sleep(0.02)

            # 3. The reclaim resubmits under the SAME idempotency key: the
            # cancelled entry must RELAUNCH (the eviction retry), not answer
            # terminal cancelled.
            started.clear()
            r2 = client.post("/v1/process", json=_payload(src, "k-evict"))
            assert r2.status_code == 202, r2.text
            body = r2.json()
            assert body["deduplicated"] is True
            assert body["status"] in ("accepted", "running"), (
                f"cancelled entry must relaunch on resubmit, got {body['status']!r}"
            )
            assert started.wait(5), "relaunched compute never started"
            assert len(runs) == 2, f"expected 2 compute runs, got {runs}"
    finally:
        release.set()
        settings.set(old)
