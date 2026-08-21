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


def _normalized_epub_copy(epub_path: Path, out_dir: Path) -> Path:
    """Runner-Härtung Z3: pandoc-sichere Paketansicht (OPF an der Wurzel).

    Bewiesene Klasse (Jobs CVM26KLA/FFMTJA3S, pandoc-Fehler wörtlich:
    'No entry on path: OEBPS/../NL$…/OEBPS/Text/Cover.xhtml'): pandoc löst
    OPF-hrefs korrekt OPF-relativ auf, normalisiert die Pfadsegmente aber
    NICHT — der literal gejointe Pfad mit '..' existiert im Zip nie, obwohl
    alle 105 Manifest-Ziele nach POSIX-Normalisierung existieren (bewiesen).
    Die Kopie legt das OPF an die ARCHIV-WURZEL ('axiom_content.opf',
    container.xml zeigt dorthin) und rewritet href/src auf die literalen
    Archiv-Namen — Wurzel + Name braucht kein '..'. Fast-Path: ohne '..'
    im OPF wird der Original-Pfad zurückgegeben."""
    import posixpath
    import zipfile

    with zipfile.ZipFile(epub_path) as z:
        names = set(z.namelist())
        opf = next((n for n in names if n.lower().endswith(".opf")), None)
        if opf is None:
            return epub_path
        src = z.read(opf).decode("utf-8", "replace")
        if "../" not in src:
            return epub_path
        opf_dir = posixpath.dirname(opf)

        def _norm_attr(m: "re.Match[str]") -> str:
            raw = m.group(2)
            fixed = posixpath.normpath(posixpath.join(opf_dir, raw))
            if fixed == raw or fixed not in names:
                return m.group(0)  # nie Kaputtes erfinden — nur reale Ziele
            return f'{m.group(1)}="{fixed}"'

        fixed_src = re.sub(r'(href|src)="([^"]+)"', _norm_attr, src)
        out = out_dir / ("normalized_" + epub_path.name)
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zout:
            for item in z.infolist():
                if item.filename == opf:
                    continue  # OPF wandert an die Wurzel (neuer Name)
                data = z.read(item.filename)
                if item.filename == "META-INF/container.xml":
                    data = ('<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">' 
                            '<rootfiles><rootfile full-path="axiom_content.opf" ' 
                            'media-type="application/oebps-package+xml"/></rootfiles></container>').encode()
                zout.writestr(item, data)
            zout.writestr("axiom_content.opf", fixed_src.encode("utf-8"))
        return out


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
