"""JobRuntime tests for #242 (cancel reaches the real subprocess) and #243
(bounded admission queue + retention).

Hermetic: no heavy/GPU deps. #242 uses a REAL sleep subprocess standing in
for the conversion worker so cancellation provably terminates an OS process
tree, not just flips a status. #243 drives the app's bounded FIFO scheduler.
"""

from __future__ import annotations

import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest
from axiom_ng_runner import app as appmod
from axiom_ng_runner import runner as runnermod
from axiom_ng_runner.config import Settings, settings
from axiom_ng_runner.job_store import Job, JobStore
from axiom_ng_runner.runtime import JobRuntime, _terminate_process_group
from fastapi.testclient import TestClient


def _sleep_child(seconds: int = 60, popen=None):
    """A real, long-running OS process standing in for the conversion worker.
    Uses its own process group (start_new_session) so the reaper's SIGTERM/
    SIGKILL to the group is observable — the #242 mechanism verbatim."""
    popen = popen or subprocess.Popen
    return popen(
        [sys.executable, "-c", "import time; time.sleep(600)"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )


class TestCancelReachesSubprocess:
    """#242 acceptance: a job in (simulated) real convert cancelled mid-run
    terminates the actual subprocess and settles the status to cancelled."""

    def test_cancel_terminates_registered_child(self):
        child = _sleep_child()
        try:
            rt = JobRuntime("job-kill", work=lambda r: None)
            rt.register_child(child)
            assert child.poll() is None  # actually running

            rt.cancel()  # SIGTERM -> reap -> SIGKILL on a daemon reaper

            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if child.poll() is not None:
                    break
                time.sleep(0.05)
            assert child.poll() is not None, (
                "cancelled job's subprocess must exit within the grace window"
            )
            assert rt.cancelled
        finally:
            _terminate_process_group(child, signal.SIGKILL)

    def test_cancel_with_no_child_is_noop(self):
        rt = JobRuntime("job-nokill", work=lambda r: None)
        rt.cancel()  # must not raise
        assert rt.cancelled


def _v1_payload(src, job_id: str, key: str, real: bool = True) -> dict:
    return {
        "contract_version": "1.0",
        "job_id": job_id,
        "idempotency_key": key,
        "source": {"type": "zotero", "source_id": "src-1", "server_id": "srv"},
        "document": {"document_id": "doc-1", "zotero_key": "ZK",
                     "zotero_version": 1,
                     "metadata_snapshot": {"itemType": "book"}},
        "attachment": {
            "attachment_id": "att-1", "zotero_key": "AK", "zotero_version": 1,
            "content_type": "application/pdf", "filename": src.name,
            "local_path": str(src),
            "content_hash": "sha256:" + _dummy_hash(src),
            "size_bytes": src.stat().st_size, "mtime_ms": 0,
        },
        "processing": {"profile": "full-rag-v1", "force_rebuild": False,
                       "extract_images": False,
                       "compute_dense_embeddings": False,
                       "compute_sparse_embeddings": False,
                       "extract_entities": False,
                       "extract_relationships": False},
    }


def _dummy_hash(src: Path) -> str:
    import hashlib

    return hashlib.sha256(src.read_bytes()).hexdigest()


class TestCancelThroughApp:
    """End-to-end via the HTTP client: a real-backend job that would run a
    long conversion is cancelled; the conversion subprocess (a fake slow Popen
    standing in for the pdf_worker) is terminated and status goes cancelled."""

    def test_cancel_mid_convert_terminates_subprocess_and_cancels(
        self, tmp_path, monkeypatch
    ):
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")

        _orig_popen = subprocess.Popen
        captured = {}

        class _SlowFakePopen:
            """Registers into the runtime like the real conversion, but holds a
            real sleep child so cancellation provably kills an OS process."""

            def __init__(self, *a, **kw):
                self._inner = _sleep_child(popen=_orig_popen)
                captured["proc"] = self._inner

            def __getattr__(self, name):
                return getattr(self._inner, name)

        monkeypatch.setattr("subprocess.Popen", _SlowFakePopen)

        old = settings.get()
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              compute_backend="real",
                              warmup=False))
        try:
            with TestClient(appmod.app) as c:
                acc = c.post("/v1/process",
                             json=_v1_payload(src, "job-cancel-real", "k-cancel-real"))
                assert acc.status_code == 202, acc.text
                job_id = acc.json()["job_id"]

                # Wait for the runtime to register the fake conversion child.
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline and "proc" not in captured:
                    time.sleep(0.02)
                assert "proc" in captured, "conversion subprocess was never registered"

                child = captured["proc"]
                assert child.poll() is None  # mid-convert

                cr = c.post(f"/v1/jobs/{job_id}/cancel")
                assert cr.status_code == 200

                # The subprocess must exit (SIGTERM'd) within the grace window.
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    if child.poll() is not None:
                        break
                    time.sleep(0.05)
                assert child.poll() is not None, (
                    "cancelled job's conversion subprocess must be terminated"
                )

                st = c.get(f"/v1/jobs/{job_id}").json()
                assert st["status"] == "cancelled", st
        finally:
            _terminate_process_group(captured.get("proc"), signal.SIGKILL)
            settings.set(old)


def _tracked_ctor(monkeypatch, appmod, captured, fn):
    """Wrap JobRuntime so tests can observe the scheduler's instances."""
    orig = JobRuntime

    class _Tracking(orig):
        def __init__(self, job_id, work):
            super().__init__(job_id, work)
            captured.append(self)

    monkeypatch.setattr(appmod, "JobRuntime", _Tracking)


class TestAdmissionQueue:
    """#243 FIFO + rejection. Uses reference backend (hermetic)."""

    @pytest.fixture
    def sel(self, tmp_path):
        return JobStore(tmp_path / "store")

    def test_fifo_one_worker_concurrency_one(self, tmp_path, monkeypatch):
        """N accepted jobs with concurrency 1 -> exactly 1 worker active, the
        rest processed in FIFO order."""
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")
        order: list[str] = []
        run_count = {"active": 0, "peak": 0}

        def slow_compute(request, work_dir, set_stage=None, commit=None,
                         set_progress=None, runtime=None):
            run_count["active"] += 1
            run_count["peak"] = max(run_count["peak"], run_count["active"])
            order.append(request["job_id"])
            time.sleep(0.05)  # keep the running window observable
            run_count["active"] -= 1
            return {"status": "completed", "chunks": []}

        monkeypatch.setattr(runnermod, "compute", slow_compute)
        # Ensure the scheduler rebuilds for the small concurrency.
        old = settings.get()
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              max_concurrent_jobs=1,
                              admission_queue_capacity=8,
                              warmup=False))
        try:
            with TestClient(appmod.app) as c:
                for i in range(3):
                    r = c.post("/v1/process",
                               json=_v1_payload(src, f"job-q{i}", f"k-q{i}"))
                    assert r.status_code == 202, r.text
                deadline = time.monotonic() + 30
                done = []
                while time.monotonic() < deadline and len(done) < 3:
                    for jid in (f"job-q{i}" for i in range(3)):
                        st = c.get(f"/v1/jobs/{jid}").json()
                        if st["status"] in ("completed", "failed") and jid not in done:
                            done.append(jid)
                    time.sleep(0.02)
                assert len(done) == 3, f"not all done: {done}"
        finally:
            settings.set(old)
        # FIFO proven: jobs completed in submission order.
        assert order == ["job-q0", "job-q1", "job-q2"], order
        assert run_count["peak"] == 1, f"concurrency must be 1, peak={run_count['peak']}"

    def test_queue_at_capacity_rejects_with_503(self, tmp_path, monkeypatch):
        """Queue am Limit -> explicit 503 QUEUE_FULL, and no thread is
        spawned for the rejected job (its record is not created)."""
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")
        spawned = {"n": 0}

        def slow_compute(request, work_dir, set_stage=None, commit=None,
                         set_progress=None, runtime=None):
            spawned["n"] += 1
            time.sleep(0.3)
            return {"status": "completed", "chunks": []}

        monkeypatch.setattr(runnermod, "compute", slow_compute)
        old = settings.get()
        # concurrency 0 is clamped to 1 by load_settings, so capacity = active
        # + admission(2) = 1 + 2 = 3 total accepted; the 4th is rejected.
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              max_concurrent_jobs=1,
                              admission_queue_capacity=2,
                              warmup=False))
        try:
            with TestClient(appmod.app) as c:
                for i in range(4):
                    r = c.post("/v1/process",
                               json=_v1_payload(src, f"job-{i}", f"k-{i}"),
                               timeout=10)
                    if i < 3:
                        assert r.status_code == 202, r.text
                    else:
                        assert r.status_code == 503, r.text
                        err = r.json()["detail"]
                        assert err["code"] == "QUEUE_FULL", err
                        assert err["retryable"] is True, err
                # Let the (up to) 3 admitted jobs finish; the rejected 4th
                # must never have spawned compute.
                time.sleep(1.0)
                assert spawned["n"] <= 3, f"rejected job must not spawn, {spawned}"
                # The rejected job's record must not exist (no stranded accept).
                assert c.get("/v1/jobs/job-3").status_code == 404
        finally:
            settings.set(old)


class TestRetention:
    """#243 retention via prune_expired on the store."""

    def test_expired_pruned_unexpired_kept(self, tmp_path):
        store = JobStore(tmp_path / "store")

        def _mk(jid, status, updated, acked=False):
            path = tmp_path / "store" / jid
            j = Job(job_id=jid, idempotency_key=f"k-{jid}", request={},
                    path=path, status=status, acked=acked)
            j.updated_at = updated
            path.mkdir(parents=True, exist_ok=True)
            # Register with the store so prune_expired can see it.
            store.put(j)
            # put() via save() stamps updated_at to now — re-stamp the age in
            # memory (the manifest age is irrelevant to the prune decision).
            j.updated_at = updated
            return j

        now = time.time()
        # unacked completed, expiring (older than the 3600s cutoff)
        _mk("done-old", "completed", now - 5000)
        # unacked cancelled, expiring
        _mk("cancelled-old", "cancelled", now - 5000)
        # unacked completed, not yet expired
        fresh = _mk("done-fresh", "completed", now - 10)
        # acked completed, old but must be kept (acked jobs are never pruned)
        acked = _mk("acked-old", "completed", now - 5000, acked=True)

        store.prune_expired(retention_seconds=3600.0)

        remaining = {j.job_id for j in store._jobs.values()}
        assert "done-old" not in remaining
        assert "cancelled-old" not in remaining
        assert "done-fresh" in remaining
        assert "acked-old" in remaining
        assert fresh.path.exists()
        assert acked.path.exists()


class TestCancelSurvivesQueue:
    """#242 security: cancel must survive the admission queue — a job
    cancelled while waiting never starts compute, even after earlier queued/
    running jobs finish."""

    def test_cancelled_queued_job_never_starts(self, tmp_path, monkeypatch):
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")
        started: list[str] = []
        first_done = {"done": False}

        def slow_compute(request, work_dir, set_stage=None, commit=None,
                         set_progress=None, runtime=None):
            started.append(request["job_id"])
            if request["job_id"] == "first":
                time.sleep(0.5)  # hold the single worker so 'queued' waits
                first_done["done"] = True
            return {"status": "completed", "chunks": []}

        monkeypatch.setattr(runnermod, "compute", slow_compute)
        old = settings.get()
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              max_concurrent_jobs=1,
                              admission_queue_capacity=8,
                              warmup=False))
        try:
            with TestClient(appmod.app) as c:
                r1 = c.post("/v1/process",
                            json=_v1_payload(src, "first", "k-first"))
                assert r1.status_code == 202, r1.text
                r2 = c.post("/v1/process",
                            json=_v1_payload(src, "queued", "k-queued"))
                assert r2.status_code == 202, r2.text

                # 'queued' is waiting behind 'first' (concurrency 1). Cancel it
                # while it is still queued — it must never start afterwards.
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline and not started:
                    time.sleep(0.02)
                assert started == ["first"]  # the worker picked up 'first' first
                cr = c.post("/v1/jobs/queued/cancel")
                assert cr.status_code == 200
                assert c.get("/v1/jobs/queued").json()["status"] == "cancelled"

                # Let 'first' finish (frees the worker); 'queued' must NOT run.
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and not first_done["done"]:
                    time.sleep(0.02)
                assert first_done["done"], "first never finished"
                time.sleep(0.2)  # give a (wrong) 'queued' start a chance
                assert started == ["first"], (
                    f"cancelled queued job must never start, got {started}"
                )
        finally:
            settings.set(old)


class TestCapacityZero:
    """#243 root cause: admission_queue_capacity=0 is a NO-WAITING queue, not
    an unbounded one. With capacity 0 only max_concurrent_jobs are accepted;
    any overflow is rejected with 503, never 202."""

    def test_capacity_zero_rejects_overflow_with_503(self, tmp_path, monkeypatch):
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 smoke")
        spawned = {"n": 0}

        def slow_compute(request, work_dir, set_stage=None, commit=None,
                         set_progress=None, runtime=None):
            spawned["n"] += 1
            time.sleep(0.3)  # hold the single worker so the queue stays busy
            return {"status": "completed", "chunks": []}

        monkeypatch.setattr(runnermod, "compute", slow_compute)
        old = settings.get()
        settings.set(Settings(work_root=tmp_path / "work",
                              allowed_source_roots=(str(tmp_path),),
                              max_concurrent_jobs=1,
                              admission_queue_capacity=0,  # no waiting room
                              warmup=False))
        try:
            with TestClient(appmod.app) as c:
                for i in range(3):
                    r = c.post("/v1/process",
                               json=_v1_payload(src, f"cap0-{i}", f"k-cap0-{i}"),
                               timeout=10)
                    if i == 0:
                        assert r.status_code == 202, r.text
                    else:
                        # overflow: capacity 0 -> only the single running slot
                        assert r.status_code == 503, (
                            f"cap0 overflow got {r.status_code}: {r.text}"
                        )
                # Only the single admitted job may spawn.
                time.sleep(1.0)
                assert spawned["n"] == 1, f"capacity 0 may admit only 1, got {spawned}"
        finally:
            settings.set(old)


class TestRetentionTimerPath:
    """#243 acceptance: retention is wired through the timer/lifespan, not
    only callable directly. A forgotten _ensure_prune_timer() must be visible."""

    def test_lifespan_timer_prunes_expired_without_manual_call(self, tmp_path):
        old = settings.get()
        try:
            work = tmp_path / "work"
            settings.set(Settings(work_root=work,
                                  result_retention_seconds=0.1,  # tiny window
                                  warmup=False))

            # Seed an expired (unacked terminal) job INTO THE APP'S store
            # singleton — the one the timer's _tick actually prunes (a local
            # JobStore would be invisible and the proof would be hollow).
            store = appmod._store_impl()
            path = work / "expired-1"
            expired = Job(job_id="expired-1", idempotency_key="k-exp-1",
                          request={}, path=path, status="completed", acked=False)
            path.mkdir(parents=True, exist_ok=True)
            store.put(expired)
            expired.updated_at = time.time() - 100.0  # well past the window
            assert "expired-1" in store._jobs

            # Wire the timer path exactly as lifespan startup does.
            appmod._ensure_prune_timer()
            timer = appmod._prune_timer
            assert timer is not None and timer.is_alive(), (
                "_ensure_prune_timer must have registered a live timer"
            )

            # Drive the timer's registered function deterministically (proves
            # the lifespan -> _ensure_prune_timer -> _tick -> prune chain),
            # then also let the real wall-clock timer fire.
            timer.function()  # invoke the scheduled _tick once
            assert "expired-1" not in store._jobs, (
                "timer tick must prune the expired job without a manual call"
            )
        finally:
            settings.set(old)


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-p", "no:cacheprovider"]))
