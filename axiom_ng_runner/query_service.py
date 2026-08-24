"""Warm query-side compute service (epic #130, R1 #131 / R2 #132).

The ingest pipeline builds heavy models per job and lets them go; queries
cannot afford that. This module owns process-wide singletons for the query
endpoints: lazy-load on first request, warm-keep for the process lifetime
(#131 core requirement — Low-Latency is the point of /v1/embed).

In ``reference`` mode the singletons are deterministic stubs mirroring the
ingest reference semantics, so the hermetic contract suite needs no torch.
In ``real`` mode a load failure raises — never a silent fallback to the
stub: a reference vector would poison the shared embedding space.
"""

from __future__ import annotations

import logging
import threading
from typing import Any, Protocol

from . import DENSE_EMBEDDING_DIM, DENSE_EMBEDDING_MODEL, RERANKER_MODEL
from .config import settings

log = logging.getLogger(__name__)

_lock = threading.Lock()
_embedder: Any | None = None
_embedder_loads = 0
_reranker: Any | None = None
_reranker_loads = 0

# #216 cold-start warmup: preload both query models at startup so the first
# real embed/rerank request is already warm. state one per process:
_warmup_event = threading.Event()   # set once the warmup load finishes (ok or fail)
_warmup_planned = False             # a background warmup thread is running/in scope
_warmup_failed = False              # the warmup load raised (endpoints fall back to lazy-load)


class QueryEmbedder(Protocol):
    def embed_queries_dense(self, texts: list[str]) -> list[list[float]]: ...

    def embed_queries_with_sparse(
        self, texts: list[str]
    ) -> tuple[list[list[float]], list[dict[str, float]]]: ...


class QueryRerankerLike(Protocol):
    def rerank(
        self, query: str, texts: list[str], top_n: int | None = None
    ) -> list[dict]: ...


class _ReferenceQueryEmbedder:
    """Deterministic stub: the same hash-based dense vectors the ingest
    reference backend produces (runner._dense_embedding), so query and
    chunk reference vectors live in one (meaningless but consistent) space.
    """

    model_name = DENSE_EMBEDDING_MODEL

    def embed_queries_dense(self, texts: list[str]) -> list[list[float]]:
        from .runner import _dense_embedding

        return [_dense_embedding({"text": t})["values"] for t in texts]

    def embed_queries_with_sparse(
        self, texts: list[str]
    ) -> tuple[list[list[float]], list[dict[str, float]]]:
        # Deterministic stub sparse: the ingest reference sparse
        # (_sparse_embedding buckets text tokens) — query and chunk stub
        # share the same meaningless-but-consistent space.
        from .runner import _sparse_embedding

        dense = self.embed_queries_dense(texts)
        sparse = [_sparse_embedding(t)["values"] for t in texts]
        return dense, sparse


def _build_embedder() -> QueryEmbedder:
    if settings.get().compute_backend == "real":
        from .compute_core.embedder import TextEmbedder

        # Explicit model: capability report and loaded model share one
        # source of truth (DENSE_EMBEDDING_MODEL) — no default-value drift.
        return TextEmbedder(model_name=DENSE_EMBEDDING_MODEL)  # raises on load failure — no silent fallback
    return _ReferenceQueryEmbedder()


class _ReferenceQueryReranker:
    """Deterministic stub: Jaccard token overlap between query and text.
    Ranks lexical-overlap candidates above distractors, which is exactly
    what the hermetic ordering tests need (the real cross-encoder quality
    smoke runs against real models in the IT suite)."""

    model_name = RERANKER_MODEL

    def rerank(
        self, query: str, texts: list[str], top_n: int | None = None
    ) -> list[dict]:
        q = set(query.lower().split())
        scored: list[dict] = []
        for i, t in enumerate(texts):
            toks = set(t.lower().split())
            union = q | toks
            j = len(q & toks) / len(union) if union else 0.0
            scored.append({"index": i, "score": round(j, 6)})
        scored.sort(key=lambda e: (-e["score"], e["index"]))
        return scored[:top_n] if top_n is not None else scored


def _build_reranker() -> QueryRerankerLike:
    if settings.get().compute_backend == "real":
        from .compute_core.reranker import QueryReranker

        # Explicit model: same rationale as _build_embedder.
        return QueryReranker(model_name=RERANKER_MODEL)  # raises on load failure — no silent fallback
    return _ReferenceQueryReranker()


def warmup_status() -> dict[str, Any]:
    """Process-wide warmup/readiness state for /v1/health and
    /v1/capabilities. models_warmed is honest: both query back-ends loaded
    right now, not merely declared. warmup_finished is True once the startup
    preload ran (success or failure) — health stays "warming" only until then,
    so a merely-warming runner is never skipped forever by the #207 monitor."""
    # Lock-free on purpose: the warmup worker (and a first-request lazy
    # load) holds _lock for the WHOLE real-model build (~90s). Taking _lock
    # here would hang /v1/health and /v1/capabilities for the entire
    # preload — the #207 probe (5s timeout) would read a merely-warming
    # runner as DOWN and the 503 "warming" below could never be served.
    # Plain reads are atomic under the GIL; the singletons are assigned
    # before the event is set, so the pair can only lag, never lie.
    return {
        "warmup_enabled": settings.get().warmup,
        "warmup_finished": _warmup_event.is_set(),
        "models_warmed": _embedder is not None and _reranker is not None,
        "warmup_failed": _warmup_failed,
    }


def start_warmup() -> None:
    """#216: preload the query models in the background at server startup.

    Only meaningful for the real backend (reference-mode stubs load
    instantly and lazily, so there is nothing to warm — keeping the hermetic
    suite and its warm-keep counters unchanged). A daemon thread loads both
    singletons exactly once via the same _build_* builders the endpoints use,
    so the first real embed/rerank request blocks on warmup (await_warmup)
    and is served warm instead of paying the ~90s model load in-band.
    """
    global _warmup_planned
    if not settings.get().warmup:
        _warmup_event.set()  # not planned: nothing to await
        return
    if settings.get().compute_backend != "real":
        # Nothing to warm (instant reference stubs); mark done so
        # await_warmup never blocks. Endpoints keep lazy-loading.
        _warmup_event.set()
        return
    with _lock:
        if _warmup_planned:
            return
        _warmup_planned = True
    threading.Thread(target=_warmup_worker, daemon=True, name="warmup").start()


def _warmup_worker() -> None:
    global _warmup_failed
    try:
        get_query_embedder()
        get_query_reranker()
        log.info("query model warmup complete (embedder + reranker resident)")
    except Exception:  # one failed load must not take the server down
        _warmup_failed = True
        log.exception("query model warmup failed — first request will lazy-load")
    finally:
        _warmup_event.set()


def await_warmup() -> None:
    """Block until the startup warmup finishes (only when one is planned).

    Called at the top of /v1/embed and /v1/rerank so the FIRST real request
    waits out the background preload instead of triggering its own ~90s cold
    load. Returns immediately when no warmup is in scope (reference mode) or
    after it is done. The warmup thread always sets _warmup_event in a finally,
    so this can never deadlock on a raised load.
    """
    if not _warmup_planned or _warmup_event.is_set():
        return
    _warmup_event.wait()


def get_query_embedder() -> QueryEmbedder:
    """Return the process-wide warm embedder, loading it on first use."""
    global _embedder, _embedder_loads
    if _embedder is not None:
        return _embedder
    with _lock:
        if _embedder is None:
            _embedder = _build_embedder()
            _embedder_loads += 1
            log.info(
                "query embedder warm-loaded (model=%s, loads=%d, dim=%d)",
                DENSE_EMBEDDING_MODEL,
                _embedder_loads,
                DENSE_EMBEDDING_DIM,
            )
    return _embedder


def get_query_reranker() -> QueryRerankerLike:
    """Return the process-wide warm reranker, loading it on first use."""
    global _reranker, _reranker_loads
    if _reranker is not None:
        return _reranker
    with _lock:
        if _reranker is None:
            _reranker = _build_reranker()
            _reranker_loads += 1
            log.info(
                "query reranker warm-loaded (model=%s, loads=%d)",
                RERANKER_MODEL,
                _reranker_loads,
            )
    return _reranker


def stats() -> dict[str, Any]:
    """Load counters + warm flags: warm-keep evidence for tests and ops."""
    with _lock:
        return {
            "embedder_loads": _embedder_loads,
            "embedder_warm": _embedder is not None,
            "reranker_loads": _reranker_loads,
            "reranker_warm": _reranker is not None,
        }


def reset() -> None:
    """Drop singletons + warmup state. Tests only — in production a reset
    would just trigger one more lazy-load (see warm-keep tests)."""
    global _embedder, _embedder_loads, _reranker, _reranker_loads
    global _warmup_event, _warmup_planned, _warmup_failed
    with _lock:
        _embedder = None
        _embedder_loads = 0
        _reranker = None
        _reranker_loads = 0
        _warmup_event = threading.Event()
        _warmup_planned = False
        _warmup_failed = False
