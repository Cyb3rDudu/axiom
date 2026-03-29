"""
Singleton cache for ML models to avoid repeated initialization.
This prevents unnecessary GPU memory allocation and model loading.
"""

import logging
import threading
from typing import Optional
from .embedder import TextEmbedder
from .reranker import TextReranker

logger = logging.getLogger(__name__)

class ModelCache:
    """Thread-safe singleton cache for ML models."""
    
    _instance = None
    _lock = threading.Lock()
    _embedder: Optional[TextEmbedder] = None
    _reranker: Optional[TextReranker] = None
    
    def __new__(cls):
        if cls._instance is None:
            with cls._lock:
                if cls._instance is None:
                    cls._instance = super().__new__(cls)
        return cls._instance
    
    def get_embedder(self) -> TextEmbedder:
        """Get or create the singleton embedder instance."""
        if self._embedder is None:
            with self._lock:
                if self._embedder is None:
                    logger.info("Initializing singleton TextEmbedder...")
                    self._embedder = TextEmbedder()
        return self._embedder
    
    def get_reranker(self) -> TextReranker:
        """Get or create the singleton reranker instance."""
        if self._reranker is None:
            with self._lock:
                if self._reranker is None:
                    logger.info("Initializing singleton TextReranker...")
                    self._reranker = TextReranker()
        return self._reranker
    
    def clear_cache(self):
        """Clear cached models (useful for testing or memory management)."""
        with self._lock:
            self._embedder = None
            self._reranker = None
            logger.info("Model cache cleared")

# Global singleton instance
model_cache = ModelCache()