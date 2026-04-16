"""PDF -> markdown conversion via Marker, as a short-lived subprocess.

Usage:
    python -m ai_researcher.pdf_worker <pdf_path> <out_markdown_path> <out_images_dir>

Writes:
    - markdown to ``out_markdown_path``
    - each extracted image to ``out_images_dir/image_<N>.<ext>``
    - a final JSON line to stdout:
        {"ok": true, "image_mapping": {original_filename: saved_filename, ...}}
      where ``image_mapping`` lets the caller rewrite image references in
      the markdown from Marker's original filenames to our on-disk ones.

Exits non-zero on any failure (with JSON error on stderr).

This entry point deliberately lives in its own package with minimal
imports at module-load time so Python startup is fast.
"""

from __future__ import annotations

import io
import json
import logging
import os
import sys
import traceback
from pathlib import Path
from typing import Any, Dict


def _stderr_err(payload: Dict[str, Any]) -> None:
    """Emit a single-line JSON error on stderr."""
    print(json.dumps(payload), file=sys.stderr, flush=True)


def _save_images(marker_images: Dict[str, Any], out_dir: Path) -> Dict[str, str]:
    """Persist each PIL image / bytes blob under a stable name.

    Returns a mapping ``{original_filename: new_filename}`` so the caller
    can fix up image references in the markdown.
    """
    from PIL import Image  # type: ignore[import-not-found]

    out_dir.mkdir(parents=True, exist_ok=True)
    mapping: Dict[str, str] = {}
    for idx, (orig_name, image_data) in enumerate(marker_images.items()):
        ext = Path(orig_name).suffix or ".png"
        new_name = f"image_{idx}{ext}"
        new_path = out_dir / new_name

        if isinstance(image_data, Image.Image):
            save_format = ext.lstrip(".").upper()
            if save_format == "JPG":
                save_format = "JPEG"
            buf = io.BytesIO()
            image_data.save(buf, format=save_format or "PNG")
            image_data = buf.getvalue()

        with open(new_path, "wb") as f:
            f.write(image_data)
        mapping[orig_name] = new_name
    return mapping


def main() -> int:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO"),
        format="%(asctime)s [pdf-worker] %(levelname)s %(name)s: %(message)s",
    )
    logger = logging.getLogger(__name__)

    if len(sys.argv) < 4:
        _stderr_err(
            {
                "ok": False,
                "error": (
                    "usage: python -m ai_researcher.pdf_worker "
                    "<pdf_path> <out_markdown> <out_images_dir>"
                ),
            }
        )
        return 2

    pdf_path = Path(sys.argv[1])
    out_md = Path(sys.argv[2])
    out_images_dir = Path(sys.argv[3])

    if not pdf_path.exists():
        _stderr_err({"ok": False, "error": f"PDF not found: {pdf_path}"})
        return 2

    # Ensure output dirs exist before we load heavy deps.
    out_md.parent.mkdir(parents=True, exist_ok=True)

    try:
        # Construct a minimal DocumentProcessor whose only job is Marker
        # conversion. It owns no embedder, no vector store, no DB — just
        # the Marker models. When this process exits, all that state is
        # freed to the OS.
        from ai_researcher.core_rag.processor import DocumentProcessor

        processor = DocumentProcessor(
            pdf_dir=pdf_path.parent,
            markdown_dir=out_md.parent,
            metadata_dir=out_md.parent,
            db_path=str(out_md.parent / ".unused.db"),
            embedder=None,
            vector_store=None,
            force_reembed=False,
            load_marker=True,
        )

        logger.info(f"Converting {pdf_path.name} via Marker...")
        markdown, marker_images = processor._convert_pdf_with_table_handling(pdf_path)
        if not markdown:
            _stderr_err({"ok": False, "error": "Marker returned empty markdown"})
            return 1

        out_md.write_text(markdown, encoding="utf-8")
        logger.info(f"Wrote markdown ({len(markdown)} chars) to {out_md}")

        mapping = _save_images(marker_images or {}, out_images_dir)
        logger.info(f"Wrote {len(mapping)} images to {out_images_dir}")

        # Final result — single JSON line on stdout.
        print(
            json.dumps(
                {
                    "ok": True,
                    "markdown_path": str(out_md),
                    "images_dir": str(out_images_dir),
                    "image_mapping": mapping,
                }
            ),
            flush=True,
        )
        return 0

    except TimeoutError as exc:
        _stderr_err({"ok": False, "error": f"Marker timed out: {exc}"})
        return 3

    except Exception as exc:
        _stderr_err(
            {
                "ok": False,
                "error": str(exc),
                "traceback": traceback.format_exc(),
            }
        )
        return 1


if __name__ == "__main__":
    sys.exit(main())
