"""
Unified GPU model cache.

All GPU models (embedder, reranker, GLiNER) are served by the GPU worker
subprocess (see ai_researcher.gpu_worker). This module exposes the same
get_embedder / get_reranker / get_gliner / unload_* API that callers
have always used, but the returned objects are thin facades that route
every call through the worker via a Unix-domain-socket RPC.

Idle VRAM cleanup is handled by the worker's own idle monitor: when idle
past AXIOM_GPU_WORKER_IDLE_SEC and the activity detector reports the
system as idle, the worker exits — the main backend process stays alive.
"""

import logging
import threading

logger = logging.getLogger(__name__)


class ModelCache:
    """Thread-safe singleton cache returning GPU worker facades."""

    _instance = None
    _lock = threading.Lock()

    _embedder_facade = None
    _reranker_facade = None
    _gliner_facade = None

    def __new__(cls):
        if cls._instance is None:
            with cls._lock:
                if cls._instance is None:
                    cls._instance = super().__new__(cls)
        return cls._instance

    # ── Embedder ────────────────────────────────────────────────────────

    def get_embedder(self):
        """Return an EmbedderFacade backed by the GPU worker subprocess."""
        if self._embedder_facade is None:
            with self._lock:
                if self._embedder_facade is None:
                    from .gpu_worker_facades import EmbedderFacade
                    logger.info("Embedder routed through GPU worker subprocess")
                    self._embedder_facade = EmbedderFacade()
        return self._embedder_facade

    def unload_embedder(self):
        """No-op — the worker owns all models as a group."""
        logger.debug("unload_embedder: worker owns models as a group")

    # ── Reranker ────────────────────────────────────────────────────────

    def get_reranker(self):
        """Return a RerankerFacade backed by the GPU worker subprocess."""
        if self._reranker_facade is None:
            with self._lock:
                if self._reranker_facade is None:
                    from .gpu_worker_facades import RerankerFacade
                    logger.info("Reranker routed through GPU worker subprocess")
                    self._reranker_facade = RerankerFacade()
        return self._reranker_facade

    def unload_reranker(self):
        """No-op — the worker owns all models as a group."""
        logger.debug("unload_reranker: worker owns models as a group")

    # ── GLiNER ──────────────────────────────────────────────────────────

    def get_gliner(self):
        """Return a GlinerFacade backed by the GPU worker subprocess."""
        if self._gliner_facade is None:
            with self._lock:
                if self._gliner_facade is None:
                    from .gpu_worker_facades import GlinerFacade
                    logger.info("GLiNER routed through GPU worker subprocess")
                    self._gliner_facade = GlinerFacade()
        return self._gliner_facade

    def unload_gliner(self):
        """No-op — the worker owns all models as a group."""
        logger.debug("unload_gliner: worker owns models as a group")

    # ── Bulk operations ─────────────────────────────────────────────────

    def unload_all(self):
        """Terminate the GPU worker subprocess to release its CUDA context.

        The main backend / doc-processor process stays alive and the worker
        will respawn on the next get_embedder / get_reranker / get_gliner call.
        """
        from .gpu_worker_facades import shutdown_worker_if_running
        shutdown_worker_if_running()
        logger.info("GPU worker subprocess terminated — VRAM released")

    # Backward compat
    def clear_cache(self):
        """Alias for unload_all."""
        self.unload_all()

    def vram_usage(self) -> str:
        """Return a human-readable summary of loaded models from the worker."""
        try:
            from .gpu_worker_facades import worker_health
            h = worker_health()
            if not h.get("loaded"):
                return "worker: none"
            loaded = [name for name, is_loaded in h["loaded"].items() if is_loaded]
            vram = h.get("vram_mb")
            vram_str = f" ({vram} MB)" if vram else ""
            return f"worker: {', '.join(loaded) if loaded else 'none'}{vram_str}"
        except Exception as exc:
            return f"worker: error ({exc})"


# Global singleton instance
model_cache = ModelCache()
