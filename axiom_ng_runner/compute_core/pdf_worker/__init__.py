"""Per-import Marker subprocess.

Runs PDF -> markdown conversion in a fresh subprocess spawned by the
doc-processor for each import (see issue #13). When the subprocess
exits, Marker's ~2.5 GB of model weights, the CUDA context, and all
allocator residue are released back to the OS — unlike loading Marker
in the long-lived doc-processor process, where the pages stay resident
for the container's lifetime.

Invoked as:
    python -m axiom_ng_runner.compute_core.pdf_worker <pdf> <out_markdown> <out_images_dir>
"""
