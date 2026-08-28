"""EPUB -> markdown conversion via pandoc, as a short-lived subprocess.

Usage:
    python -m axiom_ng_runner.compute_core.epub_worker <epub_path> <out_markdown_path> <out_images_dir>

Writes:
    - markdown to ``out_markdown_path``
    - each extracted image to ``out_images_dir/image_<N>.<ext>``
    - a final JSON line to stdout:
        {"ok": true, "image_mapping": {original_basename: saved_filename, ...}}
      where ``image_mapping`` lets the caller rewrite image references in
      the markdown from pandoc's extracted filenames to our on-disk ones.

Exits non-zero on any failure (with JSON error on stderr).

This is the EPUB counterpart of ``axiom_ng_runner.compute_core.pdf_worker`` — same CLI
contract, same stdout/stderr JSON protocol, same image-naming scheme.
The only difference is the engine: pandoc (CPU) instead of Marker (GPU).
Requires the ``pandoc`` binary on PATH (installed in the Dockerfile).

Deliberately minimal imports at module-load time so startup is fast.
"""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import subprocess
import sys
import tempfile
import traceback
from pathlib import Path
from typing import Any, Dict


def _stderr_err(payload: Dict[str, Any]) -> None:
    """Emit a single-line JSON error on stderr."""
    print(json.dumps(payload), file=sys.stderr, flush=True)


def _result(payload: Dict[str, Any]) -> None:
    """Emit the single-line JSON result on stdout (last line wins)."""
    print(json.dumps(payload), flush=True)


# Image extensions pandoc may extract from an EPUB. Matched
# case-insensitively when walking the --extract-media dir.
_IMAGE_EXTS = {".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp"}

# Purely-stylistic inline/block HTML tags pandoc leaves in GFM output when
# converting EPUB XHTML. EPUBs wrap text in <span class="bold">,
# <div class="img_container">, etc.; these carry no semantic value (no
# href/src/data-* attrs) and only clutter the markdown, so we strip them
# and keep the inner text. Markdown image refs ![](...) and links []()
# are untouched because they're not HTML tags.
_STYLING_TAG_RE = re.compile(r"</?(span|div)\b[^>]*>", re.IGNORECASE)


def _strip_styling_html(markdown: str) -> str:
    """Remove stylistic ``<span>``/``<div>`` wrappers pandoc leaks into GFM.

    Without this, headings come out as
    ``# <span class="bold">Title</span>`` instead of ``# Title``. Stripping
    yields clean markdown comparable to Marker's PDF output. Verified safe on
    real textbooks: the matched tags never carry href/src/data attributes.
    """
    cleaned = _STYLING_TAG_RE.sub("", markdown)
    # Block <div> removal can open up runs of blank lines; collapse 3+ down to
    # a single blank paragraph break so the chunker's splitting stays tight.
    cleaned = re.sub(r"\n{3,}", "\n\n", cleaned)
    return cleaned


# EPUB 3 pagebreak landmarks surface from pandoc as
# ``\[P\]<span id="..._page_P"></span>`` where P is the printed page number.
# Must run BEFORE _strip_styling_html (which would delete the landmark spans).
_PAGEBREAK_RE = re.compile(
    r'(?:\\\[(\d+)\\\])?\s*<span id="[^"]*?_page_(\d+)"></span>'
)


# #226 F1: canonicalize ALL four print-page dialects into the landmark form
# pandoc passes through (and _PAGEBREAK_RE above understands). The old worker
# only caught native Apress ``…_page_P`` ids by luck; the #222-injected shape
# (epub:type pagebreak id="PBn" role="doc-pagebreak"), Jossé class="page" and
# Bieger id="page_N" never became {N} markers. One source of truth: the
# dialect GRAMMAR mirrors compute_core.epub_pagelist (same number precedence
# title > aria-label > id > text).
def _canonicalize_page_anchors(raw: str) -> str:
    """Rewrite every dialect anchor into ``<span id="axiom_page_N"></span>``."""
    import re as _re

    def _num_from(attrs: str, text: str = "") -> str | None:
        for key in ("title", "aria-label", "id"):
            m = _re.search(rf'\b{key}="([^"]*)"', attrs)
            if m:
                d = _re.search(r"\d{1,4}", m.group(1))
                if d:
                    return d.group(0)
        d = _re.search(r"\d{1,4}", text)
        return d.group(0) if d else None

    def _span(n: str) -> str:
        return f'<span id="axiom_page_{n}"></span>'

    # 1) epub:type="pagebreak" on any element (native Apress + #222-injected)
    def _re_pagebreak(m: _re.Match[str]) -> str:
        n = _num_from(m.group(2))
        return _span(n) if n else m.group(0)

    raw = _re.sub(
        r"<(\w+)((?:[^>\"']|\"[^\"]*\"|'[^']*')*"
        r"\bepub:type=([\"'])[^\"']*\bpagebreak\b[^\"']*\3"
        r"(?:[^>\"']|\"[^\"]*\"|'[^']*')*)\s*/?>",
        _re_pagebreak, raw,
    )

    # 2) Jossé/dtv: <a class="page" id="page_N">N</a> (inline, number in text).
    # W1: "page" must be a WHITESPACE TOKEN of the class list — mirroring
    # epub_pagelist._is_candidate's class split — so class="page-num" or
    # class="page-break" does NOT match.
    def _re_class_page(m: _re.Match[str]) -> str:
        n = _num_from(m.group(1), m.group(3))
        return _span(n) if n else m.group(0)

    raw = _re.sub(
        r"<a\b([^>]*\bclass=([\"'])(?:[^\"']*\s)?page(?:\s[^\"']*)?\2[^>]*)>"
        r"\s*(\[?\d{1,4}\]?)\s*</a>",
        _re_class_page, raw,
    )

    # 3) Bieger/Springer: <a id="page_N"/> — self-closing or paired-empty —
    # followed by an optional [N] echo
    def _re_id_page(m: _re.Match[str]) -> str:
        return _span(m.group(2))

    raw = _re.sub(
        r"<a\b[^>]*\bid=([\"'])page[_-]?(\d{1,4})\1[^>]*(?:/>|>\s*</a>)"
        r"\s*(?:\[\d{1,4}\])?",
        _re_id_page, raw,
    )
    return raw


def _marker_canonicalized_copy(epub_path: Path, out_dir: Path) -> Path:
    """Copy with all page anchors canonicalized (fast-path: none found)."""
    import zipfile

    out = out_dir / ("anchors_" + epub_path.name)
    changed = False
    with zipfile.ZipFile(epub_path) as z, \
            zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in z.infolist():
            data = z.read(item.filename)
            if item.filename.lower().endswith((".xhtml", ".html")):
                raw = data.decode("utf-8", "replace")
                fixed = _canonicalize_page_anchors(raw)
                if fixed != raw:
                    changed = True
                    data = fixed.encode("utf-8")
            zout.writestr(item, data)
    return out if changed else epub_path


def _inject_page_markers(markdown: str) -> str:
    """Turn EPUB pagebreak landmarks into Marker-style ``{N}----`` markers.

    Each landmark carries the printed page P (in the ``_page_P`` anchor id,
    optionally echoed as a visible ``\\[P\\]``). We replace it with a
    ``{P-1}----`` paragraph so the chunker — which maps marker index N to
    ``str(N + 1)`` — assigns the surrounding text to printed page P. The
    visible ``\\[P\\]`` is dropped so the page number doesn't echo in the
    body, mirroring how Marker strips page numbers from PDF output. This
    gives EPUBs accurate printed page numbers (when the EPUB ships a
    page-list); EPUBs without landmarks are unaffected (chunks stay "1").
    """
    def repl(match: re.Match) -> str:
        page = int(match.group(2))
        return f"\n\n{{{page - 1}}}" + "-" * 48 + "\n\n"

    result = _PAGEBREAK_RE.sub(repl, markdown)
    # Strip standalone page-number echoes (\\[N\\]) that pandoc also emits at
    # page breaks but which weren't adjacent to a landmark span. The {N}
    # markers already carry the page info, so these echoes are redundant body
    # noise (mirrors Marker stripping page numbers from PDF body text).
    # Only matches escaped brackets with digits — leaves markdown links like
    # [97](#..._page_97) (index entries) and footnotes [^1] untouched.
    result = re.sub(r"\\\[\d+\\\]\s*", "", result)
    return result


def _save_extracted_images(media_dir: Path, out_dir: Path) -> Dict[str, str]:
    """Move every image pandoc extracted into ``out_dir`` under a stable name.

    Walks ``media_dir`` recursively (pandoc nests under ``media/``), copies
    each image to ``out_dir/image_<N>.<ext>``, and returns a mapping
    ``{extracted_basename: new_filename}``.

    Keys are basenames because the caller's rewriter
    (``DocumentProcessor._update_markdown_image_paths``) matches markdown
    references by ``Path(ref).name`` — so whatever path pandoc wrote into
    the markdown (e.g. ``media/image1.png``), only the basename matters.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    mapping: Dict[str, str] = {}

    extracted = [
        p for p in media_dir.rglob("*")
        if p.is_file() and p.suffix.lower() in _IMAGE_EXTS
    ]
    for idx, src in enumerate(extracted):
        ext = src.suffix.lower() or ".png"
        new_name = f"image_{idx}{ext}"
        shutil.copy2(src, out_dir / new_name)
        mapping[src.name] = new_name
    return mapping


# #220 Stage 2 promotion: the Z3 normalization lives in the shared repair
# toolbelt now (compute_core.epub_repair) so the fixer side can import it
# without the heavy worker. The local name stays for the W9 callers/tests.
from axiom_ng_runner.compute_core.epub_repair import (
    normalize_entry_paths as _normalized_epub_copy,
)

def _convert_via_pandoc(epub_path: Path, out_md: Path, media_dir: Path) -> None:
    """Shell out to the ``pandoc`` binary: EPUB -> GFM, images into media_dir.

    Raises FileNotFoundError if pandoc isn't on PATH, or RuntimeError on a
    non-zero pandoc exit. ``--wrap=none`` keeps paragraphs on one line so
    the chunker sees coherent text; ``--extract-media`` pulls images out
    so we can rename + serve them like PDF figures.
    """
    pandoc = shutil.which("pandoc")
    if not pandoc:
        raise FileNotFoundError(
            "pandoc binary not found on PATH — install pandoc to convert EPUB"
        )

    media_dir.mkdir(parents=True, exist_ok=True)
    cmd = [
        pandoc,
        "--from", "epub",
        "--to", "gfm",
        "--wrap", "none",
        f"--extract-media={media_dir}",
        "--output", str(out_md),
        str(epub_path),
    ]
    proc = subprocess.run(
        cmd, capture_output=True, text=True, env=os.environ.copy()
    )
    if proc.returncode != 0:
        detail = (proc.stderr or "").strip().splitlines()
        detail_str = detail[-1] if detail else f"exit code {proc.returncode}"
        raise RuntimeError(f"pandoc failed: {detail_str}")


def main() -> int:
    logging.basicConfig(
        level=os.getenv("LOG_LEVEL", "INFO"),
        format="%(asctime)s [epub-worker] %(levelname)s %(name)s: %(message)s",
    )
    logger = logging.getLogger(__name__)

    if len(sys.argv) < 4:
        _stderr_err(
            {
                "ok": False,
                "error": (
                    "usage: python -m axiom_ng_runner.compute_core.epub_worker "
                    "<epub_path> <out_markdown> <out_images_dir>"
                ),
            }
        )
        return 2

    epub_path = Path(sys.argv[1])
    out_md = Path(sys.argv[2])
    out_images_dir = Path(sys.argv[3])

    if not epub_path.exists():
        _stderr_err({"ok": False, "error": f"EPUB not found: {epub_path}"})
        return 2

    # Ensure output dirs exist before we do any work.
    out_md.parent.mkdir(parents=True, exist_ok=True)

    # pandoc dumps extracted images here; we copy the ones we want into
    # out_images_dir, then discard the rest. System temp is fine — we copy
    # (not rename), so a cross-filesystem tmp doesn't matter.
    media_tmp = Path(tempfile.mkdtemp(prefix="epub_media_"))
    try:
        # Z3: normalisierte Paketansicht — pandoc stirbt an '..'-OPF-Referenzen
        norm = _normalized_epub_copy(epub_path, media_tmp)
        if norm != epub_path:
            logger.info(f"OPF hrefs normalized for pandoc: {epub_path.name}")
        # #226 F1: all four anchor dialects → landmark spans → {N} markers
        norm = _marker_canonicalized_copy(norm, media_tmp)
        if norm != epub_path:
            logger.info(f"page anchors canonicalized: {epub_path.name}")
        logger.info(f"Converting {epub_path.name} via pandoc...")
        _convert_via_pandoc(norm, out_md, media_tmp)

        markdown = out_md.read_text(encoding="utf-8")
        # Convert EPUB pagebreak landmarks -> {N} markers FIRST (the landmarks
        # live in <span> tags that _strip_styling_html would otherwise delete).
        markdown = _inject_page_markers(markdown)
        # pandoc leaves stylistic <span>/<div> wrappers in GFM output; strip
        # them and persist the cleaned markdown so the caller reads a clean
        # file from out_md (it consumes markdown_path, not the in-memory text).
        markdown = _strip_styling_html(markdown)
        out_md.write_text(markdown, encoding="utf-8")
        if not markdown.strip():
            _stderr_err({"ok": False, "error": "pandoc returned empty markdown"})
            return 1
        logger.info(f"Wrote markdown ({len(markdown)} chars) to {out_md}")

        mapping = _save_extracted_images(media_tmp, out_images_dir)
        logger.info(f"Wrote {len(mapping)} images to {out_images_dir}")

        _result(
            {
                "ok": True,
                "markdown_path": str(out_md),
                "images_dir": str(out_images_dir),
                "image_mapping": mapping,
            }
        )
        return 0

    except FileNotFoundError as exc:
        # Missing pandoc binary — operator actionable, no traceback noise.
        _stderr_err({"ok": False, "error": str(exc)})
        return 3

    except Exception as exc:
        # Includes DRM-protected / corrupt EPUBs: pandoc exits non-zero and
        # _convert_via_pandoc wraps it as RuntimeError. Surface the message
        # so the doc-processor can store it on the failed row.
        _stderr_err(
            {
                "ok": False,
                "error": str(exc),
                "traceback": traceback.format_exc(),
            }
        )
        return 1

    finally:
        shutil.rmtree(media_tmp, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
