"""Axiom document processor runner (process contract v1).

A loopback HTTP service implementing PROCESSOR_CONTRACT.md. It owns only
computation and temporary job output; all durable state lives in axiom-ng.
"""

import os

__version__ = "0.1.0"
CONTRACT_VERSION = "1.0"

# Contract §9 stage vocabulary, in progression order. app.py owns
# validate_source (before compute starts); compute() advances the rest.
# Single source of truth: runner.py stages, the contract doc and the stage
# tests all derive from this tuple (issue #122 review M1).
PIPELINE_STAGES = (
    "validate_source",
    "convert",
    "chunk",
    "embed",
    "entities",
    "relationships",
    "assemble",
)

# Single source of truth for the reference dense-embedding model: the
# capability (GET /v1/capabilities) and every chunk result must agree
# (contract §6/§10). The real backend uses BGE-M3 (1024-dim); the reference
# backend uses a small 3-dim stub for hermetic tests.
if os.getenv("AXIOM_PROCESSOR_COMPUTE", "reference") == "real":
    DENSE_EMBEDDING_DIM = 1024
    DENSE_EMBEDDING_MODEL = "BAAI/bge-m3"
else:
    DENSE_EMBEDDING_DIM = 3
    DENSE_EMBEDDING_MODEL = "reference-bge-m3"
