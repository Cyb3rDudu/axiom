"""
Singleton cache for ML models with automatic GPU memory release.

Models are loaded on first use and automatically unloaded after
IDLE_TIMEOUT_SECONDS of inactivity to free GPU VRAM for other processes
(e.g., document processor running mREBEL).
"""

import logging
import threading
import time
from typing import Optional
from .embedder import TextEmbedder
from .reranker import TextReranker

logger = logging.getLogger(__name__)

IDLE_TIMEOUT_SECONDS = 120  # Unload models after 2 minutes of inactivity


class ModelCache:
    """Thread-safe singleton cache for ML models with idle timeout."""

    _instance = None
    _lock = threading.Lock()
    _embedder: Optional[TextEmbedder] = None
    _reranker: Optional[TextReranker] = None
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
        # Cancel existing timer
        if self._timer is not None:
            self._timer.cancel()
        # Start new timer
        self._timer = threading.Timer(IDLE_TIMEOUT_SECONDS, self._idle_unload)
        self._timer.daemon = True
        self._timer.start()

    def _idle_unload(self):
        """Called by timer when models have been idle too long."""
        elapsed = time.time() - self._last_access
        if elapsed >= IDLE_TIMEOUT_SECONDS - 1:  # small tolerance
            logger.info(f"Models idle for {elapsed:.0f}s, unloading to free GPU VRAM")
            self.clear_cache()

    def get_embedder(self) -> TextEmbedder:
        """Get or create the singleton embedder instance."""
        if self._embedder is None:
            with self._lock:
                if self._embedder is None:
                    logger.info("Initializing singleton TextEmbedder...")
                    self._embedder = TextEmbedder()
        self._touch()
        return self._embedder

    def get_reranker(self) -> TextReranker:
        """Get or create the singleton reranker instance."""
        if self._reranker is None:
            with self._lock:
                if self._reranker is None:
                    logger.info("Initializing singleton TextReranker...")
                    self._reranker = TextReranker()
        self._touch()
        return self._reranker

    def clear_cache(self):
        """Clear cached models and free GPU memory."""
        with self._lock:
            if self._timer is not None:
                self._timer.cancel()
                self._timer = None
            had_models = self._embedder is not None or self._reranker is not None
            self._embedder = None
            self._reranker = None
            if had_models:
                try:
                    import torch
                    import gc
                    gc.collect()
                    if torch.cuda.is_available():
                        torch.cuda.empty_cache()
                    logger.info("Model cache cleared, GPU memory freed")
                except Exception:
                    logger.info("Model cache cleared")


# Global singleton instance
model_cache = ModelCache()
