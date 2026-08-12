"""FastAPI application for the processor contract v1 (contract §5).

Binds to ``127.0.0.1`` by default. Processing is asynchronous: ``POST
/v1/process`` accepts 202, enqueues compute in a background thread, and the
client polls ``GET /v1/jobs/{id}``. Long Marker/model operations never hold
the request connection open.
"""

from __future__ import annotations

import logging
import threading
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException, Response

from . import CONTRACT_VERSION, DENSE_EMBEDDING_DIM, DENSE_EMBEDDING_MODEL, __version__
from .config import settings
from .job_store import Job, JobIdCollision, JobStore
from .models import (
    AckPayload,
    AckResponse,
    Capabilities,
    JobStatus,
    ProcessAccept,
    ProcessRequest,
)
from .validation import SourceError, validate_content_type, validate_source

log = logging.getLogger(__name__)

app = FastAPI(title="Axiom Processor (contract v1)", version=__version__)

# Dense-embedding dimension/model are the single source of truth in __init__
# (shared with the runner) so the capability and every chunk result agree
# (contract §6/§10).

# Registry created lazily so tests can override work_root via the settings bag.
_store: JobStore | None = None
_store_root: Path | None = None
_store_lock = threading.Lock()
_worker_sem: threading.BoundedSemaphore | None = None
_sem_concurrency: int | None = None
_running: dict[str, Any] = {}  # job_id -> {thread, process}


def _store_impl() -> JobStore:
    global _store, _store_root
    s = settings.get()
    with _store_lock:
        # Rebuild when the active work root changes (tests point each suite at
        # an isolated root; a real server loads config once on startup).
        if _store is None or _store_root != s.work_root:
            _store = JobStore(s.work_root)
            _store_root = s.work_root
            # W1 liveness: re-enqueue any non-terminal jobs recovered from a
            # previous (crashed) process so accepted work is not stranded.
            # Runs exactly once per freshly built store.
            for j in list(_store._jobs.values()):
                _relaunch_if_needed(j)
        return _store


def _semaphore() -> threading.BoundedSemaphore:
    global _worker_sem, _sem_concurrency
    s = settings.get()
    with _store_lock:
        if _worker_sem is None or _sem_concurrency != s.max_concurrent_jobs:
            _worker_sem = threading.BoundedSemaphore(s.max_concurrent_jobs)
            _sem_concurrency = s.max_concurrent_jobs
        return _worker_sem


def _capabilities() -> Capabilities:
    s = settings.get()
    return Capabilities(
        contract_versions=[CONTRACT_VERSION],
        processor={"name": "axiom-python-marker", "version": __version__},
        formats=["application/pdf", "application/epub+zip"],
        features={
            "markdown": True,
            "page_locators": True,
            "section_hierarchy": True,
            "images": False,
            "dense_embeddings": True,
            "sparse_embeddings": True,
            "entities": True,
            "entity_relationships": True,
        },
        models={
            "dense_embedding": {"name": DENSE_EMBEDDING_MODEL, "dimensions": DENSE_EMBEDDING_DIM},
            "entity_extraction": {"name": "reference-gliner"},
            "relationship_extraction": {"name": "reference-mrebel"},
        },
        limits={
            "max_concurrent_jobs": s.max_concurrent_jobs,
            "max_source_bytes": 2_147_483_648,
        },
    )


def _iso() -> str:
    import datetime

    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _status_payload(job: Job) -> dict[str, Any]:
    return {
        "contract_version": CONTRACT_VERSION,
        "job_id": job.job_id,
        "status": job.status,
        "stage": job.stage,
        "error": job.error,
        "started_at": _iso() if job.created_at else None,
        "updated_at": _iso() if job.updated_at else None,
        "completed_at": _iso() if job.status == "completed" else None,
    }


# --- shared helpers ------------------------------------------------------


def _raise_source(exc: SourceError) -> HTTPException:
    log.warning("source rejected: %s (%s)", exc.message, exc.code)
    return HTTPException(status_code=422, detail=exc.message)


def _validate_request(req: ProcessRequest) -> Path:
    """Validate source policy; returns the readable path or raises."""
    s = settings.get()
    cf = req.attachment.content_type
    validate_content_type(cf, ("application/pdf", "application/epub+zip"))
    try:
        return validate_source(
            req.attachment.local_path,
            cf,
            req.attachment.content_hash,
            s.allowed_source_roots,
            ("application/pdf", "application/epub+zip"),
        )
    except SourceError as exc:
        raise HTTPException(status_code=422, detail=exc.message) from exc


# --- background compute --------------------------------------------------


def _run_compute(job: Job) -> None:
    from .runner import compute

    with _semaphore():
        try:
            work_dir = job.path / "work"
            work_dir.mkdir(parents=True, exist_ok=True)
            _store_impl().set_status(job, "running", stage="validate_source")
            result = compute(job.request, work_dir)
            _store_impl().set_result(job, result)
        except SourceError as exc:
            _store_impl().set_error(
                job, exc.code, exc.message, retryable=False, stage="convert"
            )
        except Exception as exc:
            log.exception("job %s failed during compute", job.job_id)
            _store_impl().set_error(
                job, "INTERNAL_ERROR", str(exc), retryable=True, stage="convert"
            )
        finally:
            _running.pop(job.job_id, None)


# --- endpoints -----------------------------------------------------------


@app.get("/v1/health")
def health() -> dict[str, Any]:
    return {"status": "ok"}


@app.get("/v1/capabilities")
def capabilities() -> Capabilities:
    return _capabilities()


@app.post("/v1/process", status_code=202)
async def process(body: ProcessRequest) -> dict[str, Any]:
    # Validate the request and source *before* accepting.
    _validate_request(body)

    store = _store_impl()
    job_dir = store.work_root / body.job_id
    candidate = Job(
        job_id=body.job_id,
        idempotency_key=body.idempotency_key,
        request=body.model_dump(),
        path=job_dir,
    )
    try:
        job, deduplicated = store.get_or_create(candidate)
    except JobIdCollision as exc:
        # job_id taken with a different idempotency key: refuse, do not clobber.
        raise HTTPException(status_code=409, detail=str(exc)) from exc

    if deduplicated:
        # Dedup (W1 liveness): if the matched job recovered to `accepted`
        # after a restart and has no live compute thread, relaunch it so a
        # restart does not permanently strand accepted work (invariant #10).
        _relaunch_if_needed(job)
        return ProcessAccept(
            contract_version=CONTRACT_VERSION,
            job_id=job.job_id,
            status=job.status,
            deduplicated=True,
        ).model_dump()

    _launch_compute(job)
    return ProcessAccept(
        contract_version=CONTRACT_VERSION,
        job_id=job.job_id,
        status="accepted",
        deduplicated=False,
    ).model_dump()


def _launch_compute(job: Job) -> None:
    """Start (or restart) the background compute thread for a non-terminal job."""
    t = threading.Thread(
        target=_run_compute, args=(job,), name=f"compute-{job.job_id}", daemon=True
    )
    _running[job.job_id] = {"thread": t}
    t.start()


def _relaunch_if_needed(job: Job) -> None:
    """Re-enqueue compute for a recovered non-terminal job with no live owner.

    A job recovered from disk after a restart is `accepted` with no running
    thread; without relaunch it would strand forever (W1). Dedup callers and
    startup both route through here so the work is picked up exactly once.
    """
    if job.status not in ("accepted", "running"):
        return
    entry = _running.get(job.job_id)
    if entry is not None and entry.get("thread") and entry["thread"].is_alive():
        return  # already owned by a live compute thread
    if job.status == "running":
        # A prior owner died mid-run; demote to accepted before relaunch so we
        # do not double-count and so set_status transitions stay valid.
        _store_impl().set_status(job, "accepted", stage="")
    _launch_compute(job)


@app.get("/v1/jobs/{job_id}", response_model=JobStatus)
def job_status(job_id: str) -> dict[str, Any]:
    job = _store_impl().get(job_id)
    if job is None:
        raise HTTPException(status_code=404, detail=f"unknown job {job_id}")
    return _status_payload(job)


@app.get("/v1/jobs/{job_id}/result")
def job_result(job_id: str) -> Response:
    job = _store_impl().get(job_id)
    if job is None:
        raise HTTPException(status_code=404, detail=f"unknown job {job_id}")
    if job.status != "completed" or not job.result:
        raise HTTPException(status_code=409, detail=f"job {job_id} not completed")
    return Response(
        content=__import__("json").dumps(job.result),
        media_type="application/vnd.axiom.processor-result+json",
    )


@app.get("/v1/jobs/{job_id}/artifacts/{artifact_ref}")
def job_artifact(job_id: str, artifact_ref: str) -> Response:
    job = _store_impl().get(job_id)
    if job is None:
        raise HTTPException(status_code=404, detail=f"unknown job {job_id}")
    if job.status != "completed" or not job.result:
        raise HTTPException(status_code=409, detail=f"job {job_id} not completed")

    artifacts = job.result.get("artifacts", []) or []
    match = next((a for a in artifacts if a.get("ref") == artifact_ref), None)
    if match is None:
        raise HTTPException(status_code=404, detail=f"unknown artifact {artifact_ref}")

    if artifact_ref == "markdown":
        md = job.path / "work" / "markdown.md"
        if not md.exists():
            raise HTTPException(status_code=404, detail="artifact file missing")
        data = md.read_bytes()
    else:
        art = job.path / "work" / "artifacts" / artifact_ref
        if not art.exists():
            raise HTTPException(status_code=404, detail="artifact file missing")
        data = art.read_bytes()

    return Response(
        content=data,
        media_type=match.get("media_type", "application/octet-stream"),
    )


@app.post("/v1/jobs/{job_id}/cancel")
def job_cancel(job_id: str) -> dict[str, Any]:
    store = _store_impl()
    job = store.get(job_id)
    if job is None:
        raise HTTPException(status_code=404, detail=f"unknown job {job_id}")

    # Cancel is cooperative and idempotent (contract §17). It must NOT
    # overwrite an already-terminal job: a late cancel on a completed/failed
    # job would discard a valid result (mirrors the cancelled-guard in
    # set_result/set_error). Return the current status unchanged.
    if job.status in ("completed", "failed", "cancelled"):
        return {
            "contract_version": CONTRACT_VERSION,
            "job_id": job_id,
            "status": job.status,
        }

    # Terminate an owned compute subprocess if we have one.
    from contextlib import suppress

    entry = _running.get(job_id)
    proc = (entry or {}).get("process")
    if proc is not None and proc.poll() is None:
        with suppress(OSError):
            proc.terminate()

    store.set_status(job, "cancelled", stage="")
    return {
        "contract_version": CONTRACT_VERSION,
        "job_id": job_id,
        "status": "cancelled",
    }


@app.post("/v1/jobs/{job_id}/ack")
def job_ack(job_id: str, body: AckPayload) -> AckResponse:
    store = _store_impl()
    job = store.get(job_id)
    # Idempotent: a repeated ack after cleanup is still a successful ack.
    tomb = store.get_acked(job_id)
    if job is None and tomb is None:
        raise HTTPException(status_code=404, detail=f"unknown job {job_id}")
    # Contract §15: ack authorizes removal of temp output ONLY after the
    # result is committed. Refuse to touch a running/accepted job's work dir
    # (W3): an early ack on an in-flight job would rmtree the live compute
    # output. Return acked-but-noop; the caller may re-ack after completion.
    if (
        job is not None
        and body.persisted
        and not job.acked
        and job.status == "completed"
    ):
        store.mark_acked(job)
        store.remove_work(job_id)  # temp compute output + artifacts, not the record
    return AckResponse(contract_version=CONTRACT_VERSION, job_id=job_id, status="acked")
