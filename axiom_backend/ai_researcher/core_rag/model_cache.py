"""
Unified GPU model cache.

All GPU models go through this cache. Models load on demand and stay
loaded for the lifetime of the process. Explicit unload methods are
available for the doc-processor pipeline to free VRAM between steps.

Idle VRAM cleanup is now handled by the GPU worker subprocess (see
ai_researcher.gpu_worker.client). When the worker is idle past
AXIOM_GPU_WORKER_IDLE_SEC and the activity detector reports the system
as idle, the worker exits on its own — the main backend process stays
alive. The legacy "SIGTERM PID 1 to force a container restart" path
(issue #10) has been removed.
"""

import logging
import os
import threading
import gc
from typing import Optional

logger = logging.getLogger(__name__)

# When true, GPU models run in a child subprocess managed by
# ai_researcher.gpu_worker.client — the main backend process stays alive
# forever and only the worker subprocess dies on idle. See issue #6/#7.
USE_GPU_WORKER = os.getenv("AXIOM_USE_GPU_WORKER", "false").lower() == "true"


def _free_gpu():
    """Force GPU memory cleanup."""
    try:
        import torch
        gc.collect()
        if torch.cuda.is_available():
            torch.cuda.empty_cache()
    except Exception:
        pass


class ModelCache:
    """Thread-safe singleton cache for all GPU models."""

    _instance = None
    _lock = threading.Lock()

    # Model instances
    _embedder = None
    _reranker = None
    _gliner = None

    # Facades returned when USE_GPU_WORKER=true. Constructed lazily — the
    # worker subprocess itself is only spawned on the first RPC call.
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
        """Get or create the TextEmbedder (or facade in worker mode)."""
        if USE_GPU_WORKER:
            if self._embedder_facade is None:
                with self._lock:
                    if self._embedder_facade is None:
                        from .gpu_worker_facades import EmbedderFacade
                        logger.info("Embedder routed through GPU worker subprocess")
                        self._embedder_facade = EmbedderFacade()
            return self._embedder_facade

        if self._embedder is None:
            with self._lock:
                if self._embedder is None:
                    from .embedder import TextEmbedder
                    logger.info("Loading TextEmbedder...")
                    self._embedder = TextEmbedder()
        return self._embedder

    def unload_embedder(self):
        """Unload embedder from GPU. No-op in worker mode (models unload together)."""
        if USE_GPU_WORKER:
            logger.debug("unload_embedder skipped — worker owns models as a group")
            return
        with self._lock:
            if self._embedder is not None:
                del self._embedder
                self._embedder = None
                _free_gpu()
                logger.info("TextEmbedder unloaded")

    # ── Reranker ────────────────────────────────────────────────────────

    def get_reranker(self):
        """Get or create the TextReranker (or facade in worker mode)."""
        if USE_GPU_WORKER:
            if self._reranker_facade is None:
                with self._lock:
                    if self._reranker_facade is None:
                        from .gpu_worker_facades import RerankerFacade
                        logger.info("Reranker routed through GPU worker subprocess")
                        self._reranker_facade = RerankerFacade()
            return self._reranker_facade

        if self._reranker is None:
            with self._lock:
                if self._reranker is None:
                    from .reranker import TextReranker
                    logger.info("Loading TextReranker...")
                    self._reranker = TextReranker()
        return self._reranker

    def unload_reranker(self):
        """Unload reranker from GPU. No-op in worker mode (models unload together)."""
        if USE_GPU_WORKER:
            logger.debug("unload_reranker skipped — worker owns models as a group")
            return
        with self._lock:
            if self._reranker is not None:
                del self._reranker
                self._reranker = None
                _free_gpu()
                logger.info("TextReranker unloaded")

    # ── GLiNER ──────────────────────────────────────────────────────────

    def get_gliner(self):
        """Get or create the GLiNER model (or facade in worker mode)."""
        if USE_GPU_WORKER:
            if self._gliner_facade is None:
                with self._lock:
                    if self._gliner_facade is None:
                        from .gpu_worker_facades import GlinerFacade
                        logger.info("GLiNER routed through GPU worker subprocess")
                        self._gliner_facade = GlinerFacade()
            return self._gliner_facade

        if self._gliner is None:
            with self._lock:
                if self._gliner is None:
                    try:
                        import os
                        from gliner import GLiNER
                        from hardware_detection import hardware_detector
                        cache_dir = os.getenv("HF_HOME", "/root/.cache/huggingface/hub")
                        device = hardware_detector.get_model_device("gliner")
                        logger.info(f"Loading GLiNER (urchade/gliner_multi-v2.1) on {device}...")
                        self._gliner = GLiNER.from_pretrained(
                            "urchade/gliner_multi-v2.1",
                            cache_dir=cache_dir,
                        ).to(device)
                        logger.info("GLiNER loaded")
                    except ImportError:
                        logger.warning("GLiNER not available")
                        return None
        return self._gliner

    def unload_gliner(self):
        """Unload GLiNER from GPU. No-op in worker mode (models unload together)."""
        if USE_GPU_WORKER:
            logger.debug("unload_gliner skipped — worker owns models as a group")
            return
        with self._lock:
            if self._gliner is not None:
                del self._gliner
                self._gliner = None
                _free_gpu()
                logger.info("GLiNER unloaded")

    # ── Bulk operations ─────────────────────────────────────────────────

    def unload_all(self):
        """Unload all models and free GPU memory.

        In worker mode this terminates the GPU worker subprocess, which is
        the clean way to release its CUDA context. The main backend
        process stays alive and the worker will respawn on the next call.
        """
        if USE_GPU_WORKER:
            from .gpu_worker_facades import shutdown_worker_if_running
            shutdown_worker_if_running()
            logger.info("GPU worker subprocess terminated — VRAM released")
            return

        with self._lock:
            had = self._embedder or self._reranker or self._gliner
            self._embedder = None
            self._reranker = None
            self._gliner = None
            if had:
                _free_gpu()
                logger.info("All models unloaded, GPU memory freed")

    def unload_except(self, *keep):
        """Unload all models except the named ones.

        Args:
            *keep: Names to keep loaded ('embedder', 'reranker', 'gliner')
        """
        with self._lock:
            if 'embedder' not in keep and self._embedder is not None:
                del self._embedder
                self._embedder = None
            if 'reranker' not in keep and self._reranker is not None:
                del self._reranker
                self._reranker = None
            if 'gliner' not in keep and self._gliner is not None:
                del self._gliner
                self._gliner = None
            _free_gpu()
            logger.info(f"Unloaded models except {keep}")

    # Backward compat
    def clear_cache(self):
        """Alias for unload_all."""
        self.unload_all()

    def vram_usage(self) -> str:
        """Return a human-readable summary of loaded models."""
        if USE_GPU_WORKER:
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

        loaded = []
        if self._embedder:
            loaded.append("embedder")
        if self._reranker:
            loaded.append("reranker")
        if self._gliner:
            loaded.append("gliner")
        return ", ".join(loaded) if loaded else "none"


# Global singleton instance
model_cache = ModelCache()
