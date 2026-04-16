"""Per-import mREBEL subprocess.

Runs relation extraction in a fresh subprocess spawned by the
doc-processor for each import (see issue #13). When the subprocess
exits, mREBEL's ~2.4 GB of model weights, the CUDA context, and all
allocator residue are released back to the OS.

Invoked as:
    python -m ai_researcher.relation_worker <chunks_json> <triples_json_out>
"""
