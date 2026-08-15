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

from . import DENSE_EMBEDDING_DIM, DENSE_EMBEDDING_MODEL
from .config import settings

log = logging.getLogger(__name__)

_lock = threading.Lock()
_embedder: Any | None = None
_embedder_loads = 0


class QueryEmbedder(Protocol):
    def embed_queries_dense(self, texts: list[str]) -> list[list[float]]: ...


class _ReferenceQueryEmbedder:
    """Deterministic stub: the same hash-based dense vectors the ingest
    reference backend produces (runner._dense_embedding), so query and
    chunk reference vectors live in one (meaningless but consistent) space.
    """

    model_name = DENSE_EMBEDDING_MODEL

    def embed_queries_dense(self, texts: list[str]) -> list[list[float]]:
        from .runner import _dense_embedding

        return [_dense_embedding({"text": t})["values"] for t in texts]


def _build_embedder() -> QueryEmbedder:
    if settings.get().compute_backend == "real":
        from .compute_core.embedder import TextEmbedder

        return TextEmbedder()  # raises on load failure — no silent fallback
    return _ReferenceQueryEmbedder()


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


def stats() -> dict[str, Any]:
    """Load counters + warm flag: warm-keep evidence for tests and ops."""
    with _lock:
        return {
            "embedder_loads": _embedder_loads,
            "embedder_warm": _embedder is not None,
        }


def reset() -> None:
    """Drop singletons. Tests only — in production a reset would just
    trigger one more lazy-load (see warm-keep tests)."""
    global _embedder, _embedder_loads
    with _lock:
        _embedder = None
        _embedder_loads = 0
