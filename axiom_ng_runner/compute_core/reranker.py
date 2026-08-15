"""Cross-encoder reranker for POST /v1/rerank (epic #130 R2, #132).

Vendored-slim from the proven archive implementation
(archive/old-axiom-python: axiom_backend/ai_researcher/core_rag/reranker.py):
BAAI/bge-reranker-v2-m3 via FlagReranker with sigmoid-normalized scores,
batch scoring, and a thread lock around model access. Dropped from the
archive version: Pydantic-Note/result-object adapters (the endpoint sends
plain strings), the broad except-fallback that masked compute failures
with 0.0 scores (an endpoint must fail loudly, not hand back fake
rankings), and manual batch chunking (FlagReranker.compute_score batches
internally).

Device policy mirrors the archive one: FP16 on CUDA, FP32 forced on CPU.
MPS also runs FP32 — half-op coverage on MPS is spotty and the #128 Mac
proofs ran fp32 — with PYTORCH_ENABLE_MPS_FALLBACK=1 as insurance for
unsupported kernels (set at module top: torch reads it at import time,
so setting it after torch is loaded would be dead code).
"""

import logging
import os
import threading

# MUST precede the devices import (which imports torch): torch captures this
# flag at import time. Harmless on non-MPS hosts — it only affects MPS.
os.environ.setdefault("PYTORCH_ENABLE_MPS_FALLBACK", "1")

from .devices import hardware_detector

logger = logging.getLogger(__name__)


class QueryReranker:
    """Scores (query, candidate) pairs with a cross-encoder."""

    def __init__(
        self,
        model_name: str = "BAAI/bge-reranker-v2-m3",
        batch_size: int = 32,  # archive default
        max_length: int = 512,
    ):
        self.device = hardware_detector.get_model_device("reranker")
        # PYTORCH_ENABLE_MPS_FALLBACK is set at module top (before torch import).
        use_fp16 = self.device.startswith("cuda")
        logger.info(
            "loading reranker %s on %s (fp16=%s, batch_size=%d)",
            model_name,
            self.device,
            use_fp16,
            batch_size,
        )
        from FlagEmbedding import FlagReranker

        self.model = FlagReranker(
            model_name,
            use_fp16=use_fp16,
            devices=[self.device],
            batch_size=batch_size,
            max_length=max_length,
        )
        self._lock = threading.Lock()

    def rerank(
        self, query: str, texts: list[str], top_n: int | None = None
    ) -> list[dict]:
        """Return [{"index": i, "score": s}, ...] sorted by score descending.

        Scores are sigmoid-normalized to (0, 1) like the archive call
        (compute_score(normalize=True)). Ties keep input order (stable sort).
        """
        pairs = [[query, t] for t in texts]
        with self._lock:
            scores = self.model.compute_score(pairs, normalize=True)
        if isinstance(scores, (int, float)):
            scores = [scores]  # defensive: single-pair scalar unwrap
        out = [{"index": i, "score": float(s)} for i, s in enumerate(scores)]
        out.sort(key=lambda e: (-e["score"], e["index"]))
        return out[:top_n] if top_n is not None else out
