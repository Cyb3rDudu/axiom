"""
Unified GPU model cache.

All GPU models go through this cache. Models load on demand and stay
loaded for the lifetime of the process. Explicit unload methods are
available for the doc-processor pipeline to free VRAM between steps.

Optional idle monitor (enabled via AXIOM_AUTO_UNLOAD=true env var) unloads
models after AXIOM_IDLE_UNLOAD_SEC (default 900s/15min) of complete
inactivity across all signals (missions, docs, API). Safe because the
activity detector covers mid-mission LLM waits.
"""

import logging
import os
import threading
import time
import gc
from typing import Optional

logger = logging.getLogger(__name__)

IDLE_CHECK_INTERVAL_SEC = 60
IDLE_UNLOAD_THRESHOLD_SEC = int(os.getenv("AXIOM_IDLE_UNLOAD_SEC", "900"))
AUTO_UNLOAD_ENABLED = os.getenv("AXIOM_AUTO_UNLOAD", "false").lower() == "true"


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

    # Idle monitor
    _idle_thread_started: bool = False

    def __new__(cls):
        if cls._instance is None:
            with cls._lock:
                if cls._instance is None:
                    cls._instance = super().__new__(cls)
        return cls._instance

    def _start_idle_monitor_if_enabled(self):
        """Start the idle monitor thread (once, opt-in via env var)."""
        if self._idle_thread_started or not AUTO_UNLOAD_ENABLED:
            return
        with self._lock:
            if self._idle_thread_started:
                return
            self._idle_thread_started = True

        def monitor():
            from services.activity_detector import is_system_in_use
            logger.info(
                f"Idle monitor started (check every {IDLE_CHECK_INTERVAL_SEC}s, "
                f"unload after {IDLE_UNLOAD_THRESHOLD_SEC}s inactivity)"
            )
            while True:
                time.sleep(IDLE_CHECK_INTERVAL_SEC)
                try:
                    if self._embedder is None and self._reranker is None and self._gliner is None:
                        continue
                    in_use, reason = is_system_in_use(
                        max_request_idle_sec=IDLE_UNLOAD_THRESHOLD_SEC
                    )
                    if not in_use:
                        logger.info(f"Auto-unload triggered: {reason}")
                        self.unload_all()
                except Exception as e:
                    logger.error(f"Idle monitor error: {e}")

        t = threading.Thread(target=monitor, daemon=True, name="axiom-idle-monitor")
        t.start()

    # ── Embedder ────────────────────────────────────────────────────────

    def get_embedder(self):
        """Get or create the TextEmbedder."""
        if self._embedder is None:
            with self._lock:
                if self._embedder is None:
                    from .embedder import TextEmbedder
                    logger.info("Loading TextEmbedder...")
                    self._embedder = TextEmbedder()
        self._start_idle_monitor_if_enabled()
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
        self._start_idle_monitor_if_enabled()
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
        self._start_idle_monitor_if_enabled()
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
