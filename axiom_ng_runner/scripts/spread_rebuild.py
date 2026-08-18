"""Spread-PDF rebuild: split 2-up scans into leaves, OCR, embed print labels.

First OCR-REBUILD heal case (#188): Einführung in die Managementlehre
(Dubs/Euler/Rüegg-Stürm 2004, attachment 9MGLCJ9C) — 39 landscape spreads,
zero text layer, zero embedded labels. The old stack's tier-2 footer
sampling produced spread-left folio guesses (never verified); the owner
formula (double-verified against anchors 94/95@PDF19, 96/97@PDF20) is:

    left leaf  = 2·spread + 56     # spread = 1-based PDF page
    right leaf = 2·spread + 57

Pipeline per spread: gutter detection by vertical dark-projection minimum
(window 30-70% of width, box-smoothed — NOT blind midline; the scan skews,
measured gutters range 0.35-0.63), crop both leaves, render at 300 dpi,
tesseract deu → searchable single-page PDFs, merge, embed one label range
per leaf with the print number.

Honest verification AFTER build (this script only builds; the verify gate
runs the standard service verify against the OCR'd footer folios — the
owner formula must survive contact with the OCR text layer).
"""
from __future__ import annotations

import io
import subprocess
import sys
import tempfile
from pathlib import Path

import pymupdf

LEFT_OFFSET = 56  # left = 2·spread + LEFT_OFFSET


def detect_gutter(page: pymupdf.Page, dpi: int = 40) -> float:
    """Fractional x of the gutter (binding shadow = DARKEST vertical band).

    The binding shadow in a 2-up scan shows as a local dark maximum near
    the horizontal center (measured 0.48-0.49 on the Dubs scans). The old
    minimum-of-darkness heuristic latched onto the white margin strip at
    ~0.42 (or the search-window edges at 0.30/0.70), producing shredded or
    unsplit leaves. Window is kept tight (0.42-0.58) and both resulting
    leaf widths are validated downstream.
    """
    pix = page.get_pixmap(dpi=dpi, colorspace=pymupdf.csGRAY)
    w, h, sp = pix.width, pix.height, pix.stride
    samples = pix.samples
    cols = []
    for x in range(w):
        dark = 0
        for y in range(0, h, 2):
            if samples[y * sp + x] < 128:
                dark += 1
        cols.append(dark)
    win = 4
    smooth = [sum(cols[max(0, i - win): i + win]) for i in range(w)]
    lo, hi = int(w * 0.42), int(w * 0.58)
    g = lo + smooth[lo:hi].index(max(smooth[lo:hi]))
    return g / w


def ocr_leaf_pdf(image_bytes: bytes, lang: str = "deu") -> bytes:
    """tesseract → searchable single-page PDF (image + invisible text)."""
    # macOS/leptonica quirk: tesseract cannot read from /tmp (symlinked
    # sandbox paths fail with 'failed to open locally') — keep work in $HOME
    with tempfile.TemporaryDirectory(dir=__import__('os').path.expanduser('~')) as td:
        img = Path(td) / "leaf.png"
        img.write_bytes(image_bytes)
        out = Path(td) / "leaf"
        subprocess.run(
            ["tesseract", str(img), str(out), "-l", lang, "--dpi", "300", "pdf"],
            check=True, capture_output=True)
        return (out.with_suffix(".pdf")).read_bytes()


def rebuild(src_pdf: str, dst_pdf: str) -> dict:
    src = pymupdf.open(src_pdf)
    n = src.page_count
    stats = {"spreads": n, "leaves": 0, "gutters": [], "ocr_chars": 0}
    leaf_pdfs: list[bytes] = []
    labels: list[int] = []
    for i in range(n):
        page = src[i]
        w, h = page.rect.width, page.rect.height
        g = detect_gutter(page)
        # split sanity: both leaves must span a plausible page width
        if not (0.35 <= g <= 0.62):
            stats.setdefault("gutter_fallbacks", []).append(i + 1)
            g = 0.5
        stats["gutters"].append(round(g, 3))
        for side, (x0, x1), lab in (
            ("L", (0, g * w), 2 * (i + 1) + LEFT_OFFSET),
            ("R", (g * w, w), 2 * (i + 1) + LEFT_OFFSET + 1),
        ):
            clip = pymupdf.Rect(x0, 0, x1, h)
            pix = page.get_pixmap(dpi=300, clip=clip, colorspace=pymupdf.csGRAY)
            leaf_pdfs.append(ocr_leaf_pdf(pix.tobytes("png")))
            labels.append(lab)
            stats["leaves"] += 1
    src.close()

    out = pymupdf.open()
    for b in leaf_pdfs:
        part = pymupdf.open("pdf", b)
        out.insert_pdf(part)
        part.close()
    # one label range per leaf (firstpagenum = print number)
    spec = [{"startpage": p, "prefix": "", "style": "D", "firstpagenum": lab}
            for p, lab in enumerate(labels)]
    out.set_page_labels(spec)
    for p in range(out.page_count):
        stats["ocr_chars"] += len(out[p].get_text().strip())
    out.save(dst_pdf, deflate=True)
    out.close()
    stats["gutters_min"] = min(stats["gutters"])
    stats["gutters_max"] = max(stats["gutters"])
    return stats


if __name__ == "__main__":
    src, dst = sys.argv[1], sys.argv[2]
    stats = rebuild(src, dst)
    print(f"spreads={stats['spreads']} leaves={stats['leaves']} "
          f"ocr_chars={stats['ocr_chars']} "
          f"gutter∈[{stats['gutters_min']},{stats['gutters_max']}]")
