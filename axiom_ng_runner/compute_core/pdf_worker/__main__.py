"""PDF -> markdown conversion via Marker, as a short-lived subprocess.

Usage:
    python -m axiom_ng_runner.compute_core.pdf_worker <pdf_path> <out_markdown_path> <out_images_dir>

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


# Runner-Härtung Z1: Raster-Scan-Erkennung + Batch-Planung.
# Bewiesene Klasse (S6A3QW89/RSXUHT34 Bartscher 658 S. Vollseitenraster
# ≈7,4 GB RGB dekodiert, 0 Textzeichen; Bofinger ec03b267 gleiche Klasse):
# Marker stirbt am Speicher (cgroup oom_kill bewiesen), der Subprocess-
# Wrapper verwirft den Returncode (SIGKILL unsichtbar).

# Schwellen: unterhalb gilt ein Buch als harmlos (Normalpfad), oberhalb
# als Scan (Batch-Pfad). Konservativ gewählt: 1,5 GB dekodiertes RGB
# hat Marker auf der 24-GB-Karte nachweislich überlebt; 7,4 GB nachweis-
# lich nicht (oom_kill).
SCAN_DECODED_BYTES_LIMIT = 1_500_000_000
SCAN_TEXT_CHARS_PER_PAGE = 20
SCAN_BATCH_BYTES = 1_200_000_000  # Batch-Budget:_limit etwas unter dem Gate


class _ScanProfile(dict):
    """dict mit Attributzugriff für die Test-Oberfläche."""
    __getattr__ = dict.get


def _scan_profile(doc) -> _ScanProfile:
    """Textabdeckung/Seitenzahl/dekodierte Rastergröße einer PDF messen.

    ``doc`` ist alles, was page_count, [i].get_text() und [i].get_images()
    bereitstellt (echtes pymupdf.Document im Worker, Fake im Test).
    """
    n = doc.page_count
    total_chars = 0
    raster_pages = 0
    decoded = 0
    for i in range(n):
        page = doc[i]
        tlen = len((page.get_text() or "").strip())
        total_chars += tlen
        # get_images(full=True) liefert (xref, smask, width, height, ...) —
        # Maße ohne Dekompression direkt aus dem Tuple (Review-S6: kein
        # extract_image, kein Undercount bei kaputten Streams)
        page_px = 0
        for im in page.get_images(full=True):
            page_px += int(im[2] or 0) * int(im[3] or 0)
        if page_px * 3 > 2_000_000:  # >2 MB dekodiertes RGB (~0,7 MPix) gilt als Rasterseite
            raster_pages += 1
            decoded += page_px * 3
    return _ScanProfile(
        pages=n,
        text_chars=total_chars,
        chars_per_page=(total_chars / n) if n else 0.0,
        raster_pages=raster_pages,
        decoded_bytes=decoded,
    )


def _is_raster_scan(profile: _ScanProfile) -> bool:
    """Scan-Klasse: kaum Text + überwiegend Rasterseiten + Volumen über Limit."""
    if profile.pages < 8:
        return False
    if profile.chars_per_page >= SCAN_TEXT_CHARS_PER_PAGE:
        return False
    if profile.pages and profile.raster_pages / profile.pages < 0.8:
        return False
    return profile.decoded_bytes > SCAN_DECODED_BYTES_LIMIT


def _batch_bounds(profile: _ScanProfile, budget: int = SCAN_BATCH_BYTES) -> list[tuple[int, int]]:
    """Seiten-Batches so planen, dass jedes Batch im Dekodier-Budget bleibt.

    Rückgabe 0-basierte [start, end)-Intervalle; eine Rasterseite wiegt
    pagesgerecht decoded/pages Bytes (konservativ gleichverteilt — echte
    Scans sind uniform).
    """
    n = profile.pages
    if n == 0:
        return []
    per_page = max(1, profile.decoded_bytes // n)
    batch_pages = max(1, budget // per_page)
    out = []
    start = 0
    while start < n:
        end = min(n, start + batch_pages)
        out.append((start, end))
        start = end
    return out


def _classify_child_failure(returncode: int, stderr: str = "") -> str:
    """Z1c: Returncode/SIGKILL/OOM als eigene Fehlerklasse.

    Der alte Wrapper warf 'failed: <leerer stderr>' — ein oom_killtes
    Kind (SIGKILL, returncode -9) war unsichtbar. Jetzt: Signale werden
    benannt, SIGKILL bekommt die OOM-Klasse.
    """
    if returncode == 0:
        return ""
    if returncode < 0:
        sig = -returncode
        if sig == 9:
            return (
                "CHILD_OOM_SIGKILL: worker von SIGKILL getroffen "
                "(cgroup oom_kill wahrscheinlich) — Quelle ist ein "
                "Raster-Scan-Kandidat (Z1)"
            )
        return f"CHILD_SIGNAL_{sig}: worker von Signal {sig} beendet"
    return ""


def _convert_scan_batched(processor, pdf_path: Path, profile, logger) -> tuple[str, Dict[str, Any]]:
    """Z1b: Raster-Scans in begrenzten Batches durch Marker.

    Jedes Batch wird als eigenes Seiten-PDF (pymupdf insert_pdf) materialiert
    und durch dasselbe DocumentProcessor-Objekt geschickt — die Marker-Modelle
    laden EINMAL, das Dekodier-Volumen bleibt pro Batch im Budget. Die
    Marker-Paginierungsmarker ({N}----) jedes Batches werden auf die
    Original-Seitennummern verschoben zusammengeführt.
    """
    import re as _re
    import tempfile

    bounds = _batch_bounds(profile)
    logger.info(f"scan batch mode: {profile.pages} pages, "
                f"{profile.decoded_bytes / 1e9:.1f} GB decoded -> {len(bounds)} batches")
    parts: list[str] = []
    images: Dict[str, Any] = {}
    import pymupdf  # lazy: erst im Batch-Pfad nötig
    src = pymupdf.open(str(pdf_path))
    try:
        for b_i, (start, end) in enumerate(bounds):
            import os as _os
            fd, tmp_name = _os.mkstemp(suffix=".pdf")
            _os.close(fd)
            batch_path = Path(tmp_name)
            try:
                batch_doc = pymupdf.open()
                try:
                    batch_doc.insert_pdf(src, from_page=start, to_page=end - 1)
                    batch_doc.save(str(batch_path))
                finally:
                    batch_doc.close()
                md, batch_imgs = processor._convert_pdf_with_table_handling(batch_path)
            finally:
                batch_path.unlink(missing_ok=True)
            # Paginierungsmarker dieses Batches auf Original-Seiten schieben
            offset = start
            md = _re.sub(r"\{(\d+)\}-{10,}", lambda m, offset=offset: "{" + str(int(m.group(1)) + offset) + "}" + "-" * 48, md)
            # Review-Critical: die Bild-Dikt-Keys werden geprefixt (Kollisions-
            # freiheit über Batches) — die Markdown-Referenzen MÜSSEN mit-
            # geschrieben werden, sonst sterben die Refs am Persist-Gate
            # (CHUNK_IMAGE_REF_UNRESOLVED) obwohl die Bilder existieren.
            for k in (batch_imgs or {}):
                md = md.replace(f"]({k})", f"](b{b_i}_{k})")
            parts.append(md)
            for k, v in (batch_imgs or {}).items():
                images[f"b{b_i}_{k}"] = v
    finally:
        src.close()
    return "\n\n".join(parts), images


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
                    "usage: python -m axiom_ng_runner.compute_core.pdf_worker "
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
        # Z1: Pre-Marker-Gate — Scan-Profil messen, BEVOR die Modelle laden.
        import pymupdf as _pm
        _doc = _pm.open(str(pdf_path))
        try:
            _prof = _scan_profile(_doc)
        finally:
            _doc.close()
        logger.info(f"scan profile: {dict(_prof)}")

        # Construct a minimal DocumentProcessor whose only job is Marker
        # conversion. It owns no embedder, no vector store, no DB — just
        # the Marker models. When this process exits, all that state is
        # freed to the OS.
        from axiom_ng_runner.compute_core.pdf_processing import DocumentProcessor

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
        if _is_raster_scan(_prof):
            markdown, marker_images = _convert_scan_batched(processor, pdf_path, _prof, logger)
        else:
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
