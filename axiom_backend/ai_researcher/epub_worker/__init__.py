"""Per-import EPUB conversion subprocess.

Runs EPUB -> markdown conversion (via pandoc) in a fresh subprocess
spawned by the doc-processor for each import. Mirrors
``ai_researcher.pdf_worker`` (issue #13): the same CLI shape, the same
JSON-on-stdout protocol, the same ``image_<N>.<ext>`` naming, so the
caller treats EPUB exactly like a PDF import.

Unlike pdf_worker there is no GPU/VRAM to isolate — pandoc is a plain
CPU binary — but the per-import subprocess is kept for architectural
parity (one worker package per format).

Invoked as:
    python -m ai_researcher.epub_worker <epub> <out_markdown> <out_images_dir>
"""
