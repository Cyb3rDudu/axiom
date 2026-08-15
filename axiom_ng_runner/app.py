"""FastAPI application for the processor contract v1 (contract §5).

Binds to ``127.0.0.1`` by default. Processing is asynchronous: ``POST
/v1/process`` accepts 202, enqueues compute in a background thread, and the
client polls ``GET /v1/jobs/{id}``. Long Marker/model operations never hold
the request connection open.
"""

from __future__ import annotations

import logging
import shutil
import threading
import time
import urllib.request
import uuid
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from fastapi import FastAPI, HTTPException, Response

from . import (
    CONTRACT_VERSION,
    DENSE_EMBEDDING_DIM,
    DENSE_EMBEDDING_MODEL,
    __version__,
    query_service,
)
from .config import settings
from .job_store import Job, JobIdCollision, JobStore
from .models import (
    AckPayload,
    AckResponse,
    Capabilities,
    EmbedRequest,
    EmbedResponse,
    JobStatus,
    ProcessAccept,
    ProcessRequest,
)
from .validation import (
    SourceError,
    ensure_hash_matches,
    ensure_regular_readable,
    validate_content_type,
    validate_source,
)

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
            "query_embedding": True,
        },
        models={
            "dense_embedding": {"name": DENSE_EMBEDDING_MODEL, "dimensions": DENSE_EMBEDDING_DIM},
            "entity_extraction": {"name": "reference-gliner"},
            "relationship_extraction": {"name": "reference-mrebel"},
            "query_embedding": {"name": DENSE_EMBEDDING_MODEL, "dimensions": DENSE_EMBEDDING_DIM},
        },
        limits={
            "max_concurrent_jobs": s.max_concurrent_jobs,
            "max_source_bytes": 2_147_483_648,
            "max_query_texts": s.max_query_texts,
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


def _query_reject(code: str, message: str) -> HTTPException:
    """Uniform 422 for query-endpoint guard violations (#131/#132)."""
    log.warning("query rejected: %s (%s)", message, code)
    return HTTPException(status_code=422, detail={"code": code, "message": message})


def _contract_guard(version: str) -> None:
    if version != CONTRACT_VERSION:
        raise _query_reject(
            "CONTRACT_VERSION_UNSUPPORTED",
            f"contract_version {version!r} is not supported "
            f"(expected {CONTRACT_VERSION!r})",
        )


def _validate_request(req: ProcessRequest) -> Path:
    """Validate source policy; returns the readable path or raises.

    Precedence (contract §3 remote delivery):
      1. local_path present, under ALLOWED_SOURCE_ROOTS, readable, hash ok —
         unchanged local mode.
      2. else source_url set — stream-download into a temp dir under the
         work root and run the SAME integrity gates on the downloaded bytes
         (regular file, readable, hash). The temp file is finalized into the
         job's work dir on accept (dies with remove_work on ACK) and dropped
         on dedup/collision.
      3. else SOURCE_NOT_FOUND as before.
    """
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
    except SourceError:
        if not req.attachment.source_url:
            raise
    return _download_source(req)


def _download_source(req: ProcessRequest) -> Path:
    """Stream source_url into work_root/.incoming/<uuid>/ and gate it."""
    s = settings.get()
    att = req.attachment
    if not att.source_url:
        raise SourceError("SOURCE_NOT_FOUND", "no source_url configured")
    scheme = urlparse(att.source_url).scheme.lower()
    if scheme not in ("http", "https"):
        # Hard stop before urlopen: file:// (and every other scheme) must
        # never reach the opener, regardless of the hash gate.
        raise SourceError(
            "SOURCE_NOT_FOUND", f"unsupported source_url scheme {scheme!r}"
        )
    incoming = s.work_root / ".incoming" / uuid.uuid4().hex
    incoming.mkdir(parents=True, exist_ok=True)
    suffix = Path(att.filename or "").suffix or ".bin"
    dest = incoming / f"source{suffix}"
    # Total budget (not per-socket-op) and a byte cap: the deadline loop below
    # enforces both against a slow-drip or oversized sender.
    budget = s.source_download_timeout
    cap = att.size_bytes if att.size_bytes > 0 else 2_147_483_648
    deadline = time.monotonic() + budget
    try:
        with urllib.request.urlopen(att.source_url, timeout=budget) as r:
            if r.status != 200:
                raise SourceError(
                    "SOURCE_NOT_FOUND",
                    f"source_url returned HTTP {r.status}",
                )
            with open(dest, "wb") as f:
                total = 0
                while True:
                    if time.monotonic() > deadline:
                        raise SourceError(
                            "SOURCE_NOT_FOUND",
                            f"source_url download exceeded {budget:.0f}s budget",
                        )
                    chunk = r.read(65536)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > cap:
                        raise SourceError(
                            "SOURCE_NOT_FOUND",
                            f"source_url exceeded size cap {cap}",
                        )
                    f.write(chunk)
    except SourceError:
        shutil.rmtree(incoming, ignore_errors=True)
        raise
    except Exception as err:  # urllib errors: timeout, conn refused, HTTPError
        shutil.rmtree(incoming, ignore_errors=True)
        raise SourceError(
            "SOURCE_NOT_FOUND", f"source_url download failed: {err}"
        ) from err
    # Same integrity gates as local sources — the hash gate makes no trust
    # assumption about transport.
    try:
        path = ensure_regular_readable(dest)
        ensure_hash_matches(path, att.content_hash)
    except SourceError as exc:
        shutil.rmtree(incoming, ignore_errors=True)
        if exc.code == "SOURCE_HASH_MISMATCH":
            # Deliberately generic: no actual-hash echo for remote pulls
            # (the detailed message stays a local-mode diagnostic).
            raise SourceError(
                "SOURCE_HASH_MISMATCH",
                "downloaded source failed the content hash gate",
            ) from exc
        raise
    return path


def _finalize_source(src: Path, job: Job, deduplicated: bool) -> None:
    """Move a downloaded temp source into the job's work dir (accept) or
    drop it (dedup/collision). Local sources pass through untouched. Raises
    OSError on staging failure (caller maps to 422; the accepted job then
    fails cleanly at compute on the missing source)."""
    # Anchored: downloads land exactly at work_root/.incoming/<uuid>/source*;
    # a local source can never live there, and a dir merely NAMED .incoming
    # deeper in a tree no longer matches.
    if src.parent.parent != settings.get().work_root / ".incoming":
        return  # local mode — nothing to finalize
    if deduplicated:
        shutil.rmtree(src.parent, ignore_errors=True)
        return
    work = job.path / "work"
    work.mkdir(parents=True, exist_ok=True)
    dest = work / src.name
    try:
        shutil.move(str(src), dest)
    finally:
        shutil.rmtree(src.parent, ignore_errors=True)
    # The compute pipeline reads local_path — point it at the pulled file and
    # persist immediately: a crash before the first set_status must not leave
    # an accepted job pointing at the original (invalid) path.
    job.request["attachment"]["local_path"] = str(dest)
    job.save()


# --- background compute --------------------------------------------------


def _run_compute(job: Job) -> None:
    from .runner import compute

    with _semaphore():
        try:
            work_dir = job.path / "work"
            work_dir.mkdir(parents=True, exist_ok=True)

            def _advance(stage: str) -> None:
                # Live stage for GET /v1/jobs (§9): compute reports progress,
                # the store keeps the job visible as running.
                _store_impl().set_status(job, "running", stage=stage)

            _store_impl().set_status(job, "running", stage="validate_source")
            result = compute(job.request, work_dir, set_stage=_advance)
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


def _artifacts_expired() -> HTTPException:
    """#126 replay-after-ack: terminal refusal for a resubmit that dedups onto
    an ACKed job — the stored result is still dedupable (§19.2), but its
    artifacts died with the ACK (§15/§19.10). Handing the result back would
    send the dispatcher into an artifact-404 retry wall."""
    return HTTPException(
        status_code=409,
        detail={
            "code": "ARTIFACTS_EXPIRED",
            "message": (
                "job was acknowledged; result artifacts are gone "
                "(contract §15/§19.10). Re-enqueue with a fresh "
                "idempotency key (force_rebuild) to recompute."
            ),
            "retryable": False,
        },
    )


@app.post("/v1/process", status_code=202)
def process(body: ProcessRequest) -> dict[str, Any]:
    # Plain def (not async): source validation may download tens of MB
    # synchronously — the event loop must never block on it.
    store = _store_impl()
    job_dir = store.work_root / body.job_id
    candidate = Job(
        job_id=body.job_id,
        idempotency_key=body.idempotency_key,
        request=body.model_dump(),
        path=job_dir,
    )

    # Dedup/collision short-circuit BEFORE any download: a retried submit
    # must not pull the source again, and a job_id collision must 409
    # before spending the bandwidth (contract §8/§9 idempotency).
    existing = store.find_by_idempotency(body.idempotency_key)
    prior = store.get(body.job_id)
    if existing is not None or prior is not None:
        if prior is not None and prior.idempotency_key != body.idempotency_key:
            raise HTTPException(
                status_code=409,
                detail=f"job {body.job_id} already exists with a different idempotency key",
            )
        job, _ = store.get_or_create(candidate)  # returns the existing job
        if job.acked:
            raise _artifacts_expired()
        _relaunch_if_needed(job)
        return ProcessAccept(
            contract_version=CONTRACT_VERSION,
            job_id=job.job_id,
            status=job.status,
            deduplicated=True,
        ).model_dump()

    # Validate the request and source *before* accepting. For source_url
    # delivery this downloads + hash-gates the bytes into a temp dir.
    try:
        src_path = _validate_request(body)
    except SourceError as exc:
        raise _raise_source(exc) from exc

    try:
        job, deduplicated = store.get_or_create(candidate)
    except JobIdCollision as exc:
        # job_id taken with a different idempotency key: refuse, do not clobber.
        # (Race window: created between the short-circuit above and here.)
        _finalize_source(src_path, candidate, deduplicated=True)  # drop temp
        raise HTTPException(status_code=409, detail=str(exc)) from exc

    # Downloaded source: stage into the job's work dir (dies with ACK's
    # remove_work) or drop it when this POST deduplicated onto an existing job.
    try:
        _finalize_source(src_path, job, deduplicated)
    except OSError as exc:
        raise HTTPException(
            status_code=422, detail=f"source staging failed: {exc}"
        ) from exc

    if deduplicated:
        if job.acked:
            # Same seam as the short-circuit above: this POST lost the
            # find-by-key race but still deduped onto an ACKed job.
            raise _artifacts_expired()
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


# --- query endpoints (epic #130, contract §7a additive v1) -----------------


@app.post("/v1/embed")
def embed_queries(body: EmbedRequest) -> EmbedResponse:
    """Query-embedding endpoint (#131): dense BGE-M3 vectors for query texts.

    Plain def (not async): the model call is compute-bound and runs in the
    threadpool, same rationale as POST /v1/process. The embedder is the
    process-wide warm singleton (query_service): lazy-load on first request,
    warm-keep afterwards — the whole point of the low-latency budget.
    """
    _contract_guard(body.contract_version)
    s = settings.get()
    cap = s.max_query_texts
    limit = cap if body.max_texts is None else body.max_texts
    if not 1 <= limit <= cap:
        raise _query_reject(
            "MAX_TEXTS_INVALID",
            f"max_texts must be between 1 and the server cap {cap}",
        )
    if not body.texts:
        raise _query_reject("QUERY_TEXTS_EMPTY", "texts must not be empty")
    if any(not t.strip() for t in body.texts):
        raise _query_reject("QUERY_TEXT_BLANK", "texts must not contain blank strings")
    if len(body.texts) > limit:
        raise _query_reject(
            "QUERY_TEXTS_TOO_MANY",
            f"{len(body.texts)} texts exceed the limit {limit}",
        )

    vectors = query_service.get_query_embedder().embed_queries_dense(body.texts)
    # Model output must agree with the declared capability (contract §6):
    # drift surfaces loudly here instead of poisoning the vector space the
    # OS index lives in (silent zeros would break cosine search subtly).
    if len(vectors) != len(body.texts) or any(
        len(v) != DENSE_EMBEDDING_DIM for v in vectors
    ):
        raise HTTPException(
            status_code=500,
            detail={
                "code": "EMBEDDING_SHAPE_MISMATCH",
                "message": (
                    "embedding model returned a shape that disagrees with "
                    f"the declared capability (expected {DENSE_EMBEDDING_DIM} dims)"
                ),
            },
        )
    return EmbedResponse(
        contract_version=CONTRACT_VERSION,
        model=DENSE_EMBEDDING_MODEL,
        dimensions=DENSE_EMBEDDING_DIM,
        embeddings=vectors,
    )
