"""#271 dedup id-adoption tests: JobStore.rekey / Scheduler.rekey / guards.

Each test pins exactly one guard line — removing that line in job_store.py,
runtime.py or app.py turns the matching test red (mutation-probed):

- ``_by_idempotency`` update in ``JobStore.rekey``
- occupied-id refusal in ``JobStore.rekey`` (JobIdCollision)
- directory-aliasing guard in ``JobStore.get_or_create``
- result-echo rewrite in ``JobStore.set_result``
- ``Scheduler.rekey`` body (tracked runtime moves)
- ``_scheduler().rekey(...)`` call site in ``_adopt_requested_job_id``

Hermetic: store/scheduler on tmp_path, no DB, no heavy compute — the
app-level test blocks compute on a threading.Event so the job is still
tracked by the scheduler when the second POST arrives (the wave-1 orphan
window from #270/#271).
"""

from __future__ import annotations

import hashlib
import json
import threading

import pytest
from axiom_ng_runner import app as appmod
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.job_store import Job, JobIdCollision, JobStore
from axiom_ng_runner.runtime import JobRuntime, Scheduler
from fastapi.testclient import TestClient


def _store_job(store: JobStore, job_id: str, key: str) -> Job:
    """A store candidate exactly like app.process builds it: path derived
    from the requested id, request echoing id + key."""
    return Job(
        job_id=job_id,
        idempotency_key=key,
        request={"job_id": job_id, "idempotency_key": key},
        path=store.work_root / job_id,
    )


def _manifest(store: JobStore, job: Job) -> dict:
    return json.loads((job.path / "manifest.json").read_text(encoding="utf-8"))


class TestStoreRekey:
    def test_rekey_moves_identity_and_idempotency_index(self, tmp_path):
        store = JobStore(tmp_path / "work")
        job = _store_job(store, "job-a", "key-1")
        created, deduplicated = store.get_or_create(job)
        assert created is job and deduplicated is False

        store.rekey(job, "job-b")

        # The key — not the id — identifies the work: the index must resolve
        # to the adopted id, and the old id must stop resolving.
        found = store.find_by_idempotency("key-1")
        assert found is not None, "idempotency index lost its entry on rekey"
        assert found.job_id == "job-b"
        assert store.get("job-a") is None
        assert store.get("job-b") is job
        # The directory deliberately stays; the manifest is authoritative.
        assert job.path == store.work_root / "job-a"
        assert _manifest(store, job)["job_id"] == "job-b"
        assert _manifest(store, job)["request"]["job_id"] == "job-b"

    def test_rekey_onto_occupied_id_raises(self, tmp_path):
        store = JobStore(tmp_path / "work")
        job_a = _store_job(store, "job-a", "key-1")
        store.get_or_create(job_a)
        job_b = _store_job(store, "job-b", "key-2")
        store.get_or_create(job_b)

        with pytest.raises(JobIdCollision):
            store.rekey(job_a, "job-b")

        # A refused adoption leaves both identities untouched.
        assert job_a.job_id == "job-a"
        assert store.get("job-a") is job_a
        assert store.get("job-b") is job_b

    def test_new_job_cannot_reuse_a_rekeyed_directory(self, tmp_path):
        store = JobStore(tmp_path / "work")
        job = _store_job(store, "job-a", "key-1")
        store.get_or_create(job)
        store.rekey(job, "job-b")  # dir work_root/job-a now belongs to job-b

        impostor = _store_job(store, "job-a", "key-2")
        with pytest.raises(JobIdCollision):
            store.get_or_create(impostor)

        # The rekeyed entry's manifest was not clobbered by the impostor.
        assert _manifest(store, job)["job_id"] == "job-b"
        assert store.get("job-a") is None

    def test_set_result_rewrites_stale_job_id_echo(self, tmp_path):
        store = JobStore(tmp_path / "work")
        job = _store_job(store, "job-b", "key-1")
        store.get_or_create(job)

        # A compute thread that started before the adoption echoes the OLD id.
        store.set_result(job, {"status": "completed", "job_id": "job-a", "pages": 3})

        result = job.result
        assert result is not None
        assert result["job_id"] == "job-b"
        assert _manifest(store, job)["result"]["job_id"] == "job-b"


class TestSchedulerRekey:
    def test_rekey_moves_the_tracked_runtime(self, tmp_path):
        release = threading.Event()
        started = threading.Event()

        def _blocked(rt: JobRuntime) -> None:
            started.set()
            release.wait(10)

        scheduler = Scheduler(max_concurrent=1, queue_capacity=1, work=_blocked)
        scheduler.start()
        rt = JobRuntime("job-old", work=_blocked)
        assert scheduler.submit(rt)
        assert started.wait(5), "compute never started — job not tracked"

        scheduler.rekey("job-old", "job-new")

        assert scheduler.get("job-new") is rt
        assert scheduler.is_relevant("job-new")
        assert scheduler.get("job-old") is None
        assert not scheduler.is_relevant("job-old")
        assert rt.job_id == "job-new"

        release.set()
        rt.join(5)


class TestDedupAdoptionCallSite:
    """The app-level call site: while the job is STILL tracked by the app's
    scheduler, a second POST under a new id must move the runtime. Without
    the scheduler rekey, the old id dangles tracked and _relaunch_if_needed
    double-spawns compute under the new id."""

    def test_second_post_rekeys_tracked_runtime(self, tmp_path, monkeypatch):
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")
        payload = {
            "contract_version": "1.0",
            "job_id": "",
            "idempotency_key": "k-live",
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

        release = threading.Event()
        started = threading.Event()

        def _blocked(rt: JobRuntime) -> None:
            started.set()
            release.wait(10)

        monkeypatch.setattr(appmod, "_run_compute", _blocked)
        old = settings.get()
        # Fresh (max_concurrent, capacity) pair so _scheduler() builds a new
        # Scheduler bound to the patched work fn for this test only.
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="reference", warmup=False,
                              max_concurrent_jobs=1, admission_queue_capacity=9))
        try:
            with TestClient(appmod.app) as client:
                first = dict(payload, job_id="job-live-old")
                r1 = client.post("/v1/process", json=first)
                assert r1.status_code == 202, r1.text
                assert started.wait(5), "compute never started — job not tracked"

                second = dict(payload, job_id="job-live-new")
                r2 = client.post("/v1/process", json=second)
                assert r2.status_code == 202, r2.text
                body = r2.json()
                assert body["job_id"] == "job-live-new"
                assert body["deduplicated"] is True
                assert body["deduplicated_job_id"] == "job-live-old"

                assert appmod._scheduler().is_relevant("job-live-new")
                assert not appmod._scheduler().is_relevant("job-live-old")
        finally:
            release.set()
            settings.set(old)
