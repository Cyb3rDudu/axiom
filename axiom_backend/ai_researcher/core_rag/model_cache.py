"""
Unified GPU model cache with automatic idle unloading.

All GPU models go through this cache. Models load on demand and
unload after IDLE_TIMEOUT_SECONDS of inactivity. On a 16GB GPU
shared between backend (chat) and doc-processor (import), no model
should stay loaded when not actively needed.
"""

import logging
import threading
import time
import gc
from typing import Optional

logger = logging.getLogger(__name__)

IDLE_TIMEOUT_SECONDS = 120


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
    """Thread-safe singleton cache for all GPU models with idle timeout."""

    _instance = None
    _lock = threading.Lock()

    # Model instances
    _embedder = None
    _reranker = None
    _gliner = None

    # Idle timer
    _last_access: float = 0
    _timer: Optional[threading.Timer] = None

    def __new__(cls):
        if cls._instance is None:
            with cls._lock:
                if cls._instance is None:
                    cls._instance = super().__new__(cls)
        return cls._instance

    def _touch(self):
        """Record access time and reset idle timer."""
        self._last_access = time.time()
        if self._timer is not None:
            self._timer.cancel()
        self._timer = threading.Timer(IDLE_TIMEOUT_SECONDS, self._idle_unload)
        self._timer.daemon = True
        self._timer.start()

    def _idle_unload(self):
        """Called by timer when models have been idle."""
        elapsed = time.time() - self._last_access
        if elapsed >= IDLE_TIMEOUT_SECONDS - 1:
            logger.info(f"Models idle for {elapsed:.0f}s, unloading all GPU models")
            self.unload_all()

    # ── Embedder ────────────────────────────────────────────────────────

    def get_embedder(self):
        """Get or create the TextEmbedder."""
        if self._embedder is None:
            with self._lock:
                if self._embedder is None:
                    from .embedder import TextEmbedder
                    logger.info("Loading TextEmbedder...")
                    self._embedder = TextEmbedder()
        self._touch()
        return self._embedder

    def unload_embedder(self):
        """Unload embedder from GPU."""
        with self._lock:
            if self._embedder is not None:
                del self._embedder
                self._embedder = None
                _free_gpu()
                logger.info("TextEmbedder unloaded")

    # ── Reranker ────────────────────────────────────────────────────────

    def get_reranker(self):
        """Get or create the TextReranker."""
        if self._reranker is None:
            with self._lock:
                if self._reranker is None:
                    from .reranker import TextReranker
                    logger.info("Loading TextReranker...")
                    self._reranker = TextReranker()
        self._touch()
        return self._reranker

    def unload_reranker(self):
        """Unload reranker from GPU."""
        with self._lock:
            if self._reranker is not None:
                del self._reranker
                self._reranker = None
                _free_gpu()
                logger.info("TextReranker unloaded")

    # ── GLiNER ──────────────────────────────────────────────────────────

    def get_gliner(self):
        """Get or create the GLiNER model."""
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
        self._touch()
        return self._gliner

    def unload_gliner(self):
        """Unload GLiNER from GPU."""
        with self._lock:
            if self._gliner is not None:
                del self._gliner
                self._gliner = None
                _free_gpu()
                logger.info("GLiNER unloaded")

    # ── Bulk operations ─────────────────────────────────────────────────

    def unload_all(self):
        """Unload all models and free GPU memory."""
        with self._lock:
            if self._timer is not None:
                self._timer.cancel()
                self._timer = None
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
