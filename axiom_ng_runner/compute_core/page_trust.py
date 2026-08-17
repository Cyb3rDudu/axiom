"""Page-label trust pipeline (#173 — locator robustness).

Design principle (dudu's sharpened requirement): NEVER GUESS. Every page
reference carries its trust level visibly:

  folio_verified   printed folio read from the text layer AND verified as a
                   consistent ascending sequence — the ONLY level that may be
                   cited as a printed page
  pdf_label_sane   PDF label, sanity-checked (unique, monotone, plausible) —
                   presentable only WITH a marker
  physical_only    bare PDF page index — NEVER as a printed page ("PDF-S. X")
  none             EPUB without page-list / unpaginated — chapter/CFI, no number

The strong proof is the folio SEQUENCE: a single leading-number line can be
anything (a list item, a year), but a run of pages whose top lines form
n, n+1, n+2 ... validates EVERY member of the run. Discontinuities fall back
to pdf_label_sane, never higher.
"""
from __future__ import annotations

import logging
import re

import pymupdf

logger = logging.getLogger(__name__)

# Trust levels (wire values — contract, Epic C clients switch on these).
FOLIO_VERIFIED = "folio_verified"
PDF_LABEL_SANE = "pdf_label_sane"
PHYSICAL_ONLY = "physical_only"
NONE = "none"

_FOLIO_LINE = re.compile(r"^\s*(\d{1,4})\s*$")
_ROMAN_LINE = re.compile(r"^\s*([ivxlcdm]{1,7})\s*$", re.IGNORECASE)


def assess_labels(labels: dict[int, str], page_count: int) -> tuple[str, str]:
    """Sanity-check a PDF label map.

    Returns (trust, reason). Suspect shapes (all found in dudu's
    verification run): the SAME label repeated chapter-wise ("C1" x n),
    non-monotone numeric sequences (off-by-4 folio lag), sparse coverage.
    """
    if not labels or page_count == 0:
        return PHYSICAL_ONLY, "no labels"
    vals = list(labels.values())
    uniq = len(set(vals))
    # A label used on many pages is a section prefix, not a page number
    # (C1 repeated across a chapter).
    if uniq < len(vals) * 0.8:
        return PHYSICAL_ONLY, f"labels repeat ({uniq} unique of {len(vals)})"
    # Numeric cores must be monotone non-decreasing (allow equal runs of 1).
    nums: list[int] = []
    for v in vals:
        m = re.search(r"(\d{1,4})", v)
        if m:
            nums.append(int(m.group(1)))
    if len(nums) >= len(vals) * 0.8 and len(nums) > 1:
        drops = sum(1 for a, b in zip(nums, nums[1:]) if b < a)
        if drops > max(1, len(nums) // 50):
            return PHYSICAL_ONLY, f"non-monotone ({drops} drops)"
    coverage = len(labels) / page_count
    if coverage < 0.5:
        return PHYSICAL_ONLY, f"sparse coverage ({coverage:.0%})"
    return PDF_LABEL_SANE, "unique, monotone, covering"


def extract_folio_candidates(doc: pymupdf.Document) -> dict[int, str]:
    """Leading bare-number lines from the TOP of each page's text layer.

    Only the top ~12% of the page counts (that is where printed folios live);
    footer numbers are deliberately NOT mixed in to avoid double counting.
    """
    out: dict[int, str] = {}
    for i in range(doc.page_count):
        page = doc[i]
        rect = page.rect
        head = page.get_text("text", clip=pymupdf.Rect(rect.x0, rect.y0, rect.x1, rect.y1 * 0.12))
        for line in (l.strip() for l in head.splitlines()):
            if not line:
                continue
            if _FOLIO_LINE.match(line):
                out[i] = line
                break
            if _ROMAN_LINE.match(line):
                out[i] = line.lower()
                break
    return out


def verify_folio_sequence(candidates: dict[int, str]) -> dict[int, str]:
    """The strong proof: keep only members of consistent ascending runs.

    A run (page i..j) is consistent when each consecutive page's folio equals
    the previous + 1 (arabic) — roman numerals validate only within a roman
    run by position, and mixed runs break. Every member of a validated run is
    folio_verified; pages outside any run are NOT verified.
    """
    def _arabic(v: str | None) -> int | None:
        if v is None:
            return None
        m = _FOLIO_LINE.match(v)
        return int(m.group(1)) if m else None

    pages = sorted(candidates)
    verified: dict[int, str] = {}
    run: list[int] = []  # page indices of the current arabic run
    run_vals: list[int] = []

    def _flush() -> None:
        # A run of >= 3 consecutive pages with +1 folios validates its members
        if len(run) >= 3:
            for p, v in zip(run, run_vals):
                verified[p] = str(v)
        run.clear()
        run_vals.clear()

    prev_page: int | None = None
    prev_val: int | None = None
    for p in pages:
        v = _arabic(candidates.get(p))
        if v is None:
            _flush()
            prev_page = prev_val = None
            continue
        consecutive = prev_page is not None and p == prev_page + 1
        ascending = prev_val is not None and v == prev_val + 1
        if consecutive and ascending:
            run.append(p)
            run_vals.append(v)
        else:
            _flush()
            run.append(p)
            run_vals.append(v)
        prev_page, prev_val = p, v
    _flush()
    return verified


def build_page_trust(pdf_path: str) -> tuple[dict[int, str], dict[int, str]]:
    """Orchestrate: labels (3-tier extract) + sanity + folio sequence.

    Returns (page_label_map, page_source_map) keyed by 0-based physical page.
    Label precedence per page: verified folio > sane PDF label > physical+1.
    Trust precedence: folio_verified > pdf_label_sane > physical_only.
    """
    from axiom_ng_runner.compute_core.pdf_processing import extract_page_labels

    doc = pymupdf.open(str(pdf_path))
    try:
        n = doc.page_count
        labels = extract_page_labels(pdf_path)
        # Only TIER-1 labels (publisher metadata) may reach pdf_label_sane:
        # the 3-tier extractor silently falls back to parsed footer numbers
        # (tier 2, sampled only) or fabricated physical+1 (tier 3) — those
        # are not print-page evidence. Folio verification remains the only
        # path above physical_only for such books.
        tier1 = sum(
            1 for i in range(n) if doc[i].get_label() and doc[i].get_label().strip()
        )
        if tier1 < n * 0.5:
            trust, reason = PHYSICAL_ONLY, "no publisher labels (tier 2/3 fallback)"
        else:
            trust, reason = assess_labels(labels, n)
        folio: dict[int, str] = {}
        if trust != PDF_LABEL_SANE:
            # Labels are suspect — the folio sequence is the way out.
            folio = verify_folio_sequence(extract_folio_candidates(doc))
            if folio:
                logger.info("page_trust: %d/%d pages folio-verified (labels suspect: %s)", len(folio), n, reason)
        page_label_map: dict[int, str] = {}
        page_source_map: dict[int, str] = {}
        for i in range(n):
            if i in folio:
                page_label_map[i] = folio[i]
                page_source_map[i] = FOLIO_VERIFIED
            elif trust == PDF_LABEL_SANE and i in labels:
                page_label_map[i] = labels[i]
                page_source_map[i] = PDF_LABEL_SANE
            else:
                page_label_map[i] = str(i + 1)
                page_source_map[i] = PHYSICAL_ONLY
        if trust != PDF_LABEL_SANE and not folio:
            logger.info("page_trust: labels suspect (%s) and no folio sequence — physical_only", reason)
        return page_label_map, page_source_map
    finally:
        doc.close()
