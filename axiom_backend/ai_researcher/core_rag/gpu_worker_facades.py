"""Facade classes that present the TextEmbedder / TextReranker / GLiNER
public API but route calls through the GPU worker subprocess (see
``ai_researcher/gpu_worker/client.py``).

These are returned by ``model_cache`` when ``AXIOM_USE_GPU_WORKER=true``.
Call sites in ``retriever.py``, ``entity_extractor.py``,
``simplified_writing_agent.py``, ``api/missions.py`` etc. need no changes —
the facade instances quack like the original model objects.

Design notes
------------
- **Lazy client**. The GPU worker subprocess is only spawned on the first
  RPC call, not when a facade is constructed. This keeps ``model_cache``
  import cheap and lets the backend boot without a GPU.

- **Pydantic-safe reranker**. The worker can't serialize arbitrary
  objects (Notes, custom classes), so the ``RerankerFacade`` extracts
  each item's text on the client side, sends lightweight ``{"text": ...}``
  dicts to the worker, and then maps the worker's returned indices back
  to the caller's original ``results`` list. The caller keeps its
  original Pydantic objects — only the text crosses the socket.

- **No partial unload**. The worker owns all models together. Calls to
  ``unload_embedder`` / ``unload_reranker`` / ``unload_gliner`` become
  no-ops in worker mode (the idle monitor will free everything at once
  by killing the subprocess). ``unload_all`` is wired to
  ``GpuWorkerClient.shutdown_worker``.
"""

from __future__ import annotations

import asyncio
import logging
import os
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger(__name__)


def _client():
    """Deferred import so ``model_cache`` can be loaded without msgpack present."""
    from ai_researcher.gpu_worker.client import get_client
    return get_client()


# ── Embedder ────────────────────────────────────────────────────────────

class EmbedderFacade:
    """Mirrors ``TextEmbedder`` (embed_query, embed_chunks, async variants)."""

    def embed_query(self, query_text: str) -> Optional[Dict[str, Any]]:
        if not query_text:
            return None
        try:
            return _client().embed_query(query_text)
        except Exception as exc:
            logger.error(f"EmbedderFacade.embed_query failed: {exc}", exc_info=True)
            return None

    def embed_chunks(self, chunks: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
        if not chunks:
            return []
        # Worker returns the chunks with embeddings attached (msgpack payload).
        return _client().embed_chunks(chunks)

    async def embed_query_async(self, query_text: str) -> Optional[Dict[str, Any]]:
        if not query_text:
            return None
        return await asyncio.to_thread(self.embed_query, query_text)

    async def embed_chunks_async(self, chunks: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
        if not chunks:
            return []
        return await asyncio.to_thread(self.embed_chunks, chunks)


# ── Reranker ────────────────────────────────────────────────────────────

class RerankerFacade:
    """Mirrors ``TextReranker.rerank``.

    Accepts the same heterogeneous ``results`` list (Pydantic Notes with
    ``.content``, dicts with ``text``, raw strings, etc.). The items
    themselves never cross the socket — only extracted text does — so the
    caller gets back the exact same objects it passed in.
    """

    @staticmethod
    def _extract_text(item: Any) -> str:
        if hasattr(item, "model_fields") and hasattr(item, "content"):
            return item.content or ""
        if isinstance(item, dict):
            return item.get("text") or item.get("content") or str(item)
        return str(item)

    def rerank(
        self,
        query: str,
        results: List[Any],
        top_n: Optional[int] = None,
    ) -> List[Tuple[float, Any]]:
        if not results:
            return []
        if not query:
            return [(0.0, r) for r in results]

        payload = [{"text": self._extract_text(r)} for r in results]
        try:
            scored = _client().rerank(query, payload, top_n=top_n, timeout=120)
        except Exception as exc:
            logger.error(f"RerankerFacade.rerank failed: {exc}", exc_info=True)
            return [(0.0, r) for r in results]

        # Worker returns a list of [score, original_index] pairs already
        # sorted descending. Map indices back to the caller's objects so
        # Pydantic models etc. are preserved.
        out: List[Tuple[float, Any]] = []
        for pair in scored:
            try:
                score, idx = float(pair[0]), int(pair[1])
            except (TypeError, ValueError, IndexError):
                continue
            if 0 <= idx < len(results):
                out.append((score, results[idx]))
        return out


# ── GLiNER ──────────────────────────────────────────────────────────────

class GlinerFacade:
    """Mirrors GLiNER's ``predict_entities(text, labels, threshold, multi_label)``.

    Returns the raw list of entity dicts (``text``, ``label``, ``score``,
    ``start``, ``end``) that ``entity_extractor._extract_with_gliner``
    expects.
    """

    def predict_entities(
        self,
        text: str,
        labels: List[str],
        threshold: float = 0.45,
        multi_label: bool = True,
    ) -> List[Dict[str, Any]]:
        if not text or not labels:
            return []
        try:
            return _client().extract_entities(
                text=text,
                labels=labels,
                threshold=threshold,
                multi_label=multi_label,
                timeout=120,
            )
        except Exception as exc:
            logger.error(f"GlinerFacade.predict_entities failed: {exc}", exc_info=True)
            return []


# ── Worker lifecycle passthroughs ───────────────────────────────────────

def shutdown_worker_if_running() -> None:
    """Ask the worker to shut down (best-effort).

    Two paths:
    - **Owner** (backend): SIGTERM the Popen via ``shutdown_worker`` — the
      socket file and child process are cleaned up in-process.
    - **Client** (doc-processor): can't kill another container's subprocess
      directly, so send a ``shutdown`` RPC. The worker's signal-handler-like
      ``_shutdown`` event breaks its accept loop and it exits cleanly.

    Either path frees VRAM so callers like ``model_cache.unload_all`` can
    rely on this being effective regardless of which container invokes it.
    """
    c = _client()
    try:
        if c._client_mode:
            if os.path.exists(c._socket_path):
                try:
                    c._call("shutdown", timeout=5)
                except Exception as exc:
                    # Worker may have closed the socket before responding —
                    # that's the desired end state, so ignore.
                    logger.debug(f"shutdown RPC: {exc}")
        else:
            c.shutdown_worker()
    except Exception as exc:
        logger.warning(f"shutdown_worker_if_running: {exc}")


def worker_health() -> Dict[str, Any]:
    """Return ``GpuWorkerClient.health`` (for /api/system/gpu status).

    Returns a stub response when the worker isn't running so a status
    check never causes a spawn.
    """
    try:
        c = _client()
        if not c._worker_alive() and not c._client_mode:
            return {
                "alive": False,
                "loaded": {"embedder": False, "reranker": False, "gliner": False},
                "vram_mb": 0,
            }
        return {"alive": True, **c.health(timeout=5)}
    except Exception as exc:
        return {"alive": False, "error": str(exc)}
