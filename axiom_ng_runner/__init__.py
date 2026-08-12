"""Axiom document processor runner (process contract v1).

A loopback HTTP service implementing PROCESSOR_CONTRACT.md. It owns only
computation and temporary job output; all durable state lives in axiom-ng.
"""

__version__ = "0.1.0"
CONTRACT_VERSION = "1.0"

# Single source of truth for the reference dense-embedding model: the
# capability (GET /v1/capabilities) and every chunk result must agree
# (contract §6/§10). The real backend overrides these from the loaded model.
DENSE_EMBEDDING_DIM = 3
DENSE_EMBEDDING_MODEL = "reference-bge-m3"
