"""FastAPI application for the processor contract v1 (contract §5).

Binds to ``127.0.0.1`` by default. Processing is asynchronous: ``POST
/v1/process`` accepts 202, enqueues compute in a background thread, and the
client polls ``GET /v1/jobs/{id}``. Long Marker/model operations never hold
the request connection open.
"""

from __future__ import annotations

import asyncio
import logging
import shutil
import threading
import time
import urllib.request
import uuid
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from fastapi import FastAPI, HTTPException, Request, Response

from . import (
    CONTRACT_VERSION,
    DENSE_EMBEDDING_DIM,
    DENSE_EMBEDDING_MODEL,
    RERANKER_MODEL,
    __version__,
    query_service,
)
from .compute_core import pdf_health  # type: ignore[reportMissingImports]
from .config import settings
from .job_store import Job, JobIdCollision, JobStore
from .models import (
    AckPayload,
    AckResponse,
    Capabilities,
    EmbedRequest,
    EmbedResponse,
    JobStatus,
    PreflightReport,
    ProcessAccept,
    ProcessRequest,
    RerankRequest,
    RerankResponse,
)
from .validation import (
    SourceError,
    ensure_hash_matches,
    ensure_regular_readable,
    validate_content_type,
    validate_source,
)

log = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Startup: #216 preload the query models (background) so the first real
    embed/rerank request is warm instead of paying the ~90s cold load. No-op
    in reference mode or when AXIOM_PROCESSOR_WARMUP=0."""
    query_service.start_warmup()
    yield
    # No shutdown teardown: the warm singletons are process-lifetime by design.


app = FastAPI(title="Axiom Processor (contract v1)", version=__version__, lifespan=lifespan)

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
    warm = query_service.warmup_status()
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
            "reranking": True,
            "pdf_preflight": True,  # #175: /v1/pdf/preflight reads PDF bytes → quality gate
        },
        models={
            "dense_embedding": {"name": DENSE_EMBEDDING_MODEL, "dimensions": DENSE_EMBEDDING_DIM},
            "entity_extraction": {"name": "reference-gliner"},
            "relationship_extraction": {"name": "reference-mrebel"},
            "query_embedding": {"name": DENSE_EMBEDDING_MODEL, "dimensions": DENSE_EMBEDDING_DIM},
            "reranking": {"name": RERANKER_MODEL},
        },
        limits={
            "max_concurrent_jobs": s.max_concurrent_jobs,
            "max_source_bytes": 2_147_483_648,
            "max_query_texts": s.max_query_texts,
            "rerank_max_texts": s.rerank_max_texts,
        },
        # #216 honest readiness: models_warmed is the ACTUAL load state, not
        # a declared capability. The RAG's runner-roles probe reads it to
        # distinguish a genuinely-warm runner from one still preloading.
        warmup_enabled=warm["warmup_enabled"],
        models_warmed=warm["models_warmed"],
    )


def _iso() -> str:
    import datetime

    # Deliberately timezone.utc (not datetime.UTC): the runner targets
    # Python 3.11, where datetime.UTC does not exist yet.
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _status_payload(job: Job) -> dict[str, Any]:
    return {
        "contract_version": CONTRACT_VERSION,
        "job_id": job.job_id,
        "status": job.status,
        "stage": job.stage,
        "progress": job.progress or {"completed_units": 0, "total_units": 0, "unit": ""},
        # #225 early-commit visibility: operators and the E2E can see that
        # a partial result is already committed (chunks/embeddings safe)
        # even while a late stage is still running or has failed.
        "partial_result_available": bool(job.partial and job.result),
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

    HTTP-ONLY precedence (owner ruling, precision-wave post-mortem):
      1. source_url set — stream-download into a temp dir under the work
         root and run the SAME integrity gates on the downloaded bytes.
         Remote delivery is the contract path; a Mac storage path is never
         filesystem-probed inside a carrier container.
      2. else local_path under ALLOWED_SOURCE_ROOTS (fixture/co-located
         deliveries only), readable, hash ok — local mode.
      3. else SOURCE_URL_MISSING — the loud policy death: remote delivery
         is not configured for this runner, and the local path is outside
         its allowed roots. Never the confusing SOURCE_NOT_FOUND shape
         that cost the precision wave 130 jobs in minutes.
    """
    s = settings.get()
    cf = req.attachment.content_type
    validate_content_type(cf, ("application/pdf", "application/epub+zip"))
    if req.attachment.source_url:
        return _download_source(req)
    try:
        return validate_source(
            req.attachment.local_path,
            cf,
            req.attachment.content_hash,
            s.allowed_source_roots,
            ("application/pdf", "application/epub+zip"),
        )
    except SourceError:
        raise SourceError(
            "SOURCE_URL_MISSING",
            f"remote delivery not configured: source_url is empty and local "
            f"path {req.attachment.local_path!r} is not under this runner's "
            f"allowed roots — configure AXIOM_PROCESSOR_SOURCE_BASE_URL/"
            f"SOURCE_SECRET on the dispatcher (runbook §2.2)",
        ) from None


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

            def _progress(completed: int, total: int, unit: str) -> None:
                # #225: §9 progress for long-running stage loops — throttled
                # to every 10th unit (or the last) so the disk write per
                # batch stays cheap even for 400+ chunk books.
                if completed % 10 == 0 or completed >= total:
                    _store_impl().set_progress(job, completed, total, unit)

            def _commit(result: dict) -> None:
                # #225 early-commit: chunks/embeddings/entities persist
                # BEFORE the relationships stage runs; a late-stage abort
                # leaves this snapshot retrievable.
                _store_impl().set_partial(job, result)

            _store_impl().set_status(job, "running", stage="validate_source")
            result = compute(job.request, work_dir, set_stage=_advance,
                             commit=_commit, set_progress=_progress)
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
def health():
    """Liveness + honest warmup readiness (#216). While a real-model preload
    is still in flight the runner reports 503 warming so a #207 health probe
    treats it as not-yet-available; once the preload finishes (ok or fail) it
    goes green — a merely-warming runner is never skipped forever. Reference
    mode (nothing to warm) is always green."""
    st = query_service.warmup_status()
    if st["warmup_enabled"] and not st["warmup_finished"] and not st["models_warmed"]:
        # 503 while the real-model preload is in flight. Plain Response (no
        # extra import): a JSON body with the warming state.
        return Response(
            content="""{"status":"warming","models_warmed":false}""",
            status_code=503,
            media_type="application/json",
        )
    return {"status": "ok", "models_warmed": st["models_warmed"]}


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


@app.post("/v1/embed", response_model_exclude_none=True)
def embed_queries(body: EmbedRequest) -> EmbedResponse:
    """Query-embedding endpoint (#131): dense BGE-M3 vectors for query texts.

    response_model_exclude_none keeps the response §7a-clean on the dense-only
    path: "sparse" is present ONLY when the request asked include_sparse
    (no "sparse": null).

    Plain def (not async): the model call is compute-bound and runs in the
    threadpool, same rationale as POST /v1/process. The embedder is the
    process-wide warm singleton (query_service): lazy-load on first request,
    warm-keep afterwards — the whole point of the low-latency budget.
    """
    _contract_guard(body.contract_version)
    # #216: wait out the startup warmup so the FIRST real request is served
    # warm (no ~90s cold load in-band). No-op when no warmup is in scope.
    query_service.await_warmup()
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

    sparse_maps = None
    if body.include_sparse:
        vectors, sparse_maps = query_service.get_query_embedder().embed_queries_with_sparse(
            body.texts
        )
        if len(sparse_maps) != len(body.texts):
            raise HTTPException(
                status_code=500,
                detail={
                    "code": "EMBEDDING_SHAPE_MISMATCH",
                    "message": "sparse count disagrees with the input text count",
                },
            )
    else:
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
        sparse=sparse_maps,
    )


@app.post("/v1/rerank")
def rerank_texts(body: RerankRequest) -> RerankResponse:
    """Cross-encoder rerank endpoint (#132): sigmoid scores for
    (query, candidate) pairs, sorted descending, truncated to top_n.

    Same warm-singleton discipline as /v1/embed (query_service). Plain def:
    the model call is compute-bound and runs in the threadpool.
    """
    _contract_guard(body.contract_version)
    # #216: same as /v1/embed — block on the background preload so the first
    # real rerank is warm.
    query_service.await_warmup()
    s = settings.get()
    if not body.query.strip():
        raise _query_reject("RERANK_QUERY_EMPTY", "query must not be blank")
    if not body.texts:
        raise _query_reject("RERANK_TEXTS_EMPTY", "texts must not be empty")
    if any(not t.strip() for t in body.texts):
        raise _query_reject("RERANK_TEXT_BLANK", "texts must not contain blank strings")
    if len(body.texts) > s.rerank_max_texts:
        raise _query_reject(
            "RERANK_TEXTS_TOO_MANY",
            f"{len(body.texts)} texts exceed the server cap {s.rerank_max_texts}",
        )
    if body.top_n < 1:
        raise _query_reject("RERANK_TOP_N_INVALID", "top_n must be >= 1")

    # Archive semantics: top_n slices the ranking (results[:top_n]) — a
    # top_n above len(texts) returns everything instead of 422ing the
    # default top_n=10 for small candidate sets.
    ranked = query_service.get_query_reranker().rerank(
        body.query, body.texts, top_n=min(body.top_n, len(body.texts))
    )
    # Self-check mirrors /v1/embed: the model layer must return one score per
    # input text in the wrapper's contract shape, or the endpoint fails loudly.
    if (
        len(ranked) != min(body.top_n, len(body.texts))
        or len({e["index"] for e in ranked}) != len(ranked)
        or any(e["index"] < 0 or e["index"] >= len(body.texts) for e in ranked)
    ):
        raise HTTPException(
            status_code=500,
            detail={
                "code": "RERANK_SHAPE_MISMATCH",
                "message": "reranker returned an invalid ranking shape",
            },
        )
    return RerankResponse(
        contract_version=CONTRACT_VERSION,
        model=RERANKER_MODEL,
        scores=ranked,
    )


@app.post("/v1/pdf/preflight", response_model=PreflightReport)
async def pdf_preflight(request: Request):
    """#175 quality gate BEFORE chunking: takes a PDF as raw bytes
    (Content-Type: application/pdf), runs the read-only pdf_health.analyze_pdf
    metrics (text layer, page density, blank/image-only series, label/folio
    anomalies) and returns the structured report. NO repair, no upstream
    mutation — diagnosis only. `ok=false` flags a job for the repair/skip
    policy. Body = the PDF bytes (raw), not a wrapper/envelope.
    """
    data = await request.body()
    if not data:
        raise _query_reject("PREFLIGHT_EMPTY", "no document bytes in request body")

    s = settings.get()
    work = s.work_root / "preflight"
    work.mkdir(parents=True, exist_ok=True)
    # #220: the gate serves BOTH formats — PDF bytes (application/pdf) run
    # pdf_health, EPUB bytes (application/epub+zip) run epub_health incl.
    # the external epubcheck conformance stage. Same Rot→Skip→Repair
    # policy, same report shape (details mirrors the respective analyzer).
    is_epub = "epub" in (request.headers.get("content-type") or "").lower()
    suffix = ".epub" if is_epub else ".pdf"
    tmp = work / f"preflight-{uuid.uuid4().hex}{suffix}"
    try:
        tmp.write_bytes(data)
        if is_epub:
            from axiom_ng_runner.compute_core.epub_health import analyze_epub

            d = await asyncio.get_running_loop().run_in_executor(
                None, analyze_epub, str(tmp), s.epubcheck_cmd
            )
        else:
            d = await asyncio.get_running_loop().run_in_executor(
                None, pdf_health.analyze_pdf, str(tmp)
            )
    except Exception as exc:  # noqa: BLE001 — a broken PDF is evidence, not a traceback
        # A parse failure (pymupdf.FileDataError / FileNotFoundError) is a
        # legitimate diagnostic: the caller's policy treats it as un-assessable
        # and falls back, but never as silent ok. Raise a structured 500.
        raise HTTPException(
            status_code=500,
            detail={
                "code": "PREFLIGHT_PARSE",
                "message": f"Dokument konnte nicht analysiert werden: {type(exc).__name__}: {exc}",
            },
        ) from None
    finally:
        try:
            tmp.unlink(missing_ok=True)
        except OSError:
            pass

    v = d.get("verdacht", "")
    # #175 blocker fix (review): ok must ALSO require a text layer — a
    # textless scan with sane labels would otherwise report green and the
    # dispatcher would chunk a PDF that yields junk chunks. The label/folio
    # verdict alone (🟢/🟡) is not enough; yellow-with-text and red verdicts
    # behave exactly as before, only textless now fails the gate.
    ok = (v.startswith(("🟢", "🟡"))) and bool(d.get("text_layer"))
    return PreflightReport(
        contract_version=CONTRACT_VERSION,
        source_name="inline",
        ok=ok,
        verdacht=v,
        grund=d.get("label_befund", ""),
        details=d,
    )
