"""Temporary job state for the processor runner (contract §9.1).

Each accepted job owns an isolated directory under the configured work root
keyed by its job id. A small JSON manifest persists the request and the
accepted/running/terminal state so the service can recover from a restart
without losing accepted work.

This is operational, temporary state — never application truth. axiom-ng owns
the durable record.
"""

from __future__ import annotations

import json
import logging
import shutil
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)


class JobIdCollision(Exception):
    """Raised by get_or_create when job_id is taken with a different key."""

    def __init__(self, job_id: str) -> None:
        super().__init__(f"job {job_id} already exists with a different idempotency key")
        self.job_id = job_id


@dataclass
class Job:
    job_id: str
    idempotency_key: str
    request: dict[str, Any]  # the (dict-able) process request
    path: Path  # this job's isolated directory on disk
    status: str = "accepted"  # accepted|running|completed|failed|cancelled
    stage: str = ""
    result: dict[str, Any] | None = None  # processor result once complete
    error: dict[str, Any] | None = None  # contract-shaped error
    # #225 early-commit: chunks/embeddings are committed BEFORE the
    # relationships stage runs; a late-stage abort leaves the partial
    # result retrievable (status stays running until the final verdict).
    partial: bool = False
    progress: dict[str, Any] | None = None  # §9 progress {completed,total,unit}
    acked: bool = False
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)

    # --- manifest persistence ------------------------------------------
    def manifest(self) -> dict[str, Any]:
        return {
            "job_id": self.job_id,
            "idempotency_key": self.idempotency_key,
            "request": self.request,
            "status": self.status,
            "stage": self.stage,
            "result": self.result,
            "error": self.error,
            "partial": self.partial,
            "progress": self.progress,
            "acked": self.acked,
            "created_at": self.created_at,
            "updated_at": self.updated_at,
        }

    def save(self) -> None:
        self.updated_at = time.time()
        self.path.mkdir(parents=True, exist_ok=True)
        _atomic_write(self.path / "manifest.json", self.manifest())

    @classmethod
    def load(cls, path: Path) -> Job | None:
        mf = path / "manifest.json"
        if not mf.exists():
            return None
        try:
            data = json.loads(mf.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None
        return cls(
            job_id=data["job_id"],
            idempotency_key=data["idempotency_key"],
            request=data["request"],
            path=path,
            status=data.get("status", "accepted"),
            stage=data.get("stage", ""),
            result=data.get("result"),
            error=data.get("error"),
            partial=data.get("partial", False),
            progress=data.get("progress"),
            acked=data.get("acked", False),
            created_at=data.get("created_at", 0.0),
            updated_at=data.get("updated_at", 0.0),
        )


def _atomic_write(path: Path, payload: dict[str, Any]) -> None:
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(payload, sort_keys=True), encoding="utf-8")
    tmp.replace(path)


class JobStore:
    """In-memory + disk job registry.

    ``_lock`` serializes reads/writes because the HTTP server and the compute
    worker are different threads. ``by_idempotency`` enables dedup; both maps
    are rebuilt from disk on construction so a restart recovers accepted jobs.
    """

    def __init__(self, work_root: Path) -> None:
        self.work_root = Path(work_root)
        self.work_root.mkdir(parents=True, exist_ok=True)
        # .incoming only ever holds downloads of in-flight requests; any
        # residue belongs to a dead process — sweep it at startup.
        try:
            shutil.rmtree(self.work_root / ".incoming", ignore_errors=True)
        except OSError:
            log.warning("failed to sweep .incoming under %s", self.work_root)
        self._lock = threading.RLock()
        self._jobs: dict[str, Job] = {}
        self._by_idempotency: dict[str, str] = {}
        self._recover()

    # --- recovery across restart ----------------------------------------
    def _recover(self) -> None:
        for entry in self.work_root.iterdir():
            if not entry.is_dir():
                continue
            job = Job.load(entry)
            if job is None:
                continue
            # A job from a previous process must not silently become success.
            # Recover it to a recoverable-but-not-completed state unless it
            # was already acknowledgeable (completed + still tracked).
            if job.status in ("running", "accepted"):
                job.status = "accepted"
                job.stage = ""
            self._jobs[job.job_id] = job
            self._by_idempotency[job.idempotency_key] = job.job_id

    # --- lookup ---------------------------------------------------------
    def get(self, job_id: str) -> Job | None:
        with self._lock:
            return self._jobs.get(job_id)

    def find_by_idempotency(self, key: str) -> Job | None:
        with self._lock:
            jid = self._by_idempotency.get(key)
            return self._jobs.get(jid) if jid else None

    def get_or_create(self, job: Job) -> tuple[Job, bool]:
        """Atomically (W2) insert or dedup by idempotency key.

        Returns (the job to use, deduplicated). On collision:
        - same idempotency key, different job_id: return the existing job
          (deduplicated=True); the caller MUST discard its candidate.
        - same job_id, different key: raise to signal the collision (409).
        - job_id already taken with the same key: return it deduplicated.
        Holds ``_lock`` across the whole check-then-insert so two concurrent
        identical POSTs cannot both create a job and both spawn compute.
        """
        with self._lock:
            existing_jid = self._by_idempotency.get(job.idempotency_key)
            if existing_jid is not None and existing_jid != job.job_id:
                return self._jobs[existing_jid], True
            existing_same_jid = self._jobs.get(job.job_id)
            if existing_same_jid is not None:
                if existing_same_jid.idempotency_key != job.idempotency_key:
                    raise JobIdCollision(job.job_id)
                return existing_same_jid, True
            self._jobs[job.job_id] = job
            self._by_idempotency[job.idempotency_key] = job.job_id
            job.save()
            return job, False

    # --- mutations ------------------------------------------------------
    def put(self, job: Job) -> None:
        with self._lock:
            self._jobs[job.job_id] = job
            self._by_idempotency[job.idempotency_key] = job.job_id
            job.save()

    def set_status(
        self,
        job: Job,
        status: str,
        stage: str = "",
    ) -> None:
        with self._lock:
            # A cancelled job is terminal: a late compute thread must not
            # transition it back to running (cancellation wins). Allow
            # re-asserting cancelled (idempotent) but block resurrection.
            if job.status == "cancelled" and status != "cancelled":
                return
            job.status = status
            job.stage = stage
            job.save()

    def set_result(self, job: Job, result: dict[str, Any]) -> None:
        with self._lock:
            # A cancelled (or otherwise settled) job must not be resurrected to
            # completed by a late-arriving compute result (cancellation wins).
            if job.status == "cancelled":
                return
            job.result = result
            job.status = result.get("status", "completed")
            job.save()

    def set_partial(self, job: Job, result: dict[str, Any]) -> None:
        """#225 early-commit: store the so-far-complete result while the job
        stays running. The dispatcher never sees a terminal state, so no
        race with the live compute; a later set_result overwrites, and an
        abort leaves this snapshot retrievable for forensics/E2E."""
        with self._lock:
            if job.status == "cancelled":
                return
            job.result = result
            job.partial = True
            job.save()

    def set_progress(self, job: Job, completed: int, total: int, unit: str) -> None:
        """#225: §9 progress for the live job status. Throttled by the
        caller; only meaningful while running."""
        with self._lock:
            if job.status != "running":
                return
            job.progress = {
                "completed_units": completed,
                "total_units": total,
                "unit": unit,
            }
            job.save()

    def set_error(
        self,
        job: Job,
        code: str,
        message: str,
        retryable: bool = False,
        stage: str = "",
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock:
            if job.status == "cancelled":
                return
            job.error = {
                "code": code,
                "message": message,
                "retryable": retryable,
                "stage": stage,
                "details": details or {},
            }
            job.status = "failed"
            job.save()

    def mark_acked(self, job: Job) -> None:
        with self._lock:
            job.acked = True
            job.save()

    def get_acked(self, job_id: str) -> Job | None:
        """Return the job if it is tracked and marked as acknowledged."""
        with self._lock:
            job = self._jobs.get(job_id)
            if job is not None and job.acked:
                return job
            return None

    def remove_work(self, job_id: str) -> None:
        """Remove a job's temporary compute output, keeping the job record so
        a repeated ack stays idempotent."""
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return
            work_dir = job.path / "work"
            try:
                if work_dir.exists():
                    shutil.rmtree(work_dir, ignore_errors=True)
            except OSError:
                log.warning("failed to remove work dir %s", work_dir)

    def prune_expired(self, retention_seconds: float) -> None:
        """Remove unacknowledged jobs older than the retention window."""
        cutoff = time.time() - retention_seconds
        with self._lock:
            for jid in list(self._jobs):
                job = self._jobs[jid]
                if (
                    job.status == "completed"
                    and not job.acked
                    and job.updated_at < cutoff
                    or job.status in ("failed", "cancelled")
                    and job.updated_at < cutoff
                ):
                    self._drop(jid)

    def remove(self, job_id: str) -> None:
        with self._lock:
            self._drop(job_id)

    def _drop(self, job_id: str) -> None:
        job = self._jobs.pop(job_id, None)
        if job:
            self._by_idempotency.pop(job.idempotency_key, None)
            try:
                shutil.rmtree(job.path, ignore_errors=True)
            except OSError:
                log.warning("failed to remove job dir %s", job.path)
