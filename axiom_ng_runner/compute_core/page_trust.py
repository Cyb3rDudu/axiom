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


def folio_runs(candidates: dict[int, str]) -> list[tuple[list[int], list[int]]]:
    """Consecutive-page runs whose folios ascend by +1 (>= 3 members).

    Shared by verify_folio_sequence and the #176 analyze CLI (read-only
    diagnostics: run start, length, value range, gaps between runs).
    """
    pages = sorted(candidates)
    runs: list[tuple[list[int], list[int]]] = []  # (page indices, folio values)
    run: list[int] = []
    run_vals: list[int] = []

    def _flush() -> None:
        nonlocal run, run_vals
        # A run of >= 3 consecutive pages with +1 folios is a valid proof
        if len(run) >= 3:
            runs.append((run, run_vals))
        run = []
        run_vals = []

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
    return runs


def _arabic(v: str | None) -> int | None:
    if v is None:
        return None
    m = _FOLIO_LINE.match(v)
    return int(m.group(1)) if m else None


def label_section_starts(doc: pymupdf.Document) -> list[tuple[int, int]]:
    """Arabic label sections from the PDF label tree (W12 anchor form).

    The fix service bakes verified anchor plans into the healed PDF as
    label sections (set_page_labels facts pinned there: startpage 0-based,
    firstpagenum, empty prefix + decimal style for the numbered sections —
    a leading front-matter section carries a non-empty prefix like "C").
    Only the empty-prefix decimal sections count: they are the chapter
    restarts. Returns [(start_page0, start_value)] sorted by page.
    """
    try:
        spec = doc.get_page_labels()
    except Exception:
        return []
    out: list[tuple[int, int]] = []
    for s in spec or []:
        if not isinstance(s, dict):
            continue
        if s.get("prefix") or s.get("style", "D") != "D":
            continue
        try:
            out.append((int(s["startpage"]), int(s.get("firstpagenum", 1))))
        except (KeyError, TypeError, ValueError):
            continue
    return sorted(out)


def chapter_of(page: int, chapters: list[tuple[int, int]]) -> int:
    """1-based chapter ordinal of a physical page (0 = before the first
    arabic section, i.e. front matter)."""
    ch = 0
    for start, ordinal in chapters:
        if start <= page:
            ch = ordinal
    return ch


def chapter_restarts(
    doc: pymupdf.Document, candidates: dict[int, str]
) -> list[tuple[int, int]] | None:
    """Chapter-relative hypothesis, corroborated by the folio runs (W12).

    A book is chapter-relative when the label tree carries >= 2 arabic
    sections AND every folio run corroborates the section math: a run must
    sit inside ONE section and its values must equal start_value +
    (page - section_start). Any contradiction -> None, and the caller
    falls back to the legacy global mode (byte-identical to pre-W12 —
    never guess). Returns [(section_start_page0, ordinal_1based)].
    """
    sections = label_section_starts(doc)
    if len(sections) < 2:
        return None
    first_start = sections[0][0]
    for pages, vals in folio_runs(candidates):
        if pages[0] < first_start:
            continue  # front matter: not a chapter claim, no contradiction
        idx = 0
        for i, (start, _) in enumerate(sections):
            if start <= pages[0]:
                idx = i
        start_page, start_val = sections[idx]
        next_start = sections[idx + 1][0] if idx + 1 < len(sections) else pages[-1] + 1
        if pages[-1] >= next_start:
            return None  # run crosses a restart the folios do not show
        if vals[0] != start_val + (pages[0] - start_page):
            return None  # folio contradicts the label-section math
    return [(p, i + 1) for i, (p, _) in enumerate(sections)]


def _resolve_value_clashes(
    runs: list[tuple[list[int], list[int]]],
    claims: dict,
) -> dict[int, str]:
    """Longest run per key wins; a length tie is ambiguous — the value
    verifies NOWHERE (never guess)."""
    verified: dict[int, str] = {}
    for key, idxs in claims.items():
        if len(idxs) == 1:
            winner = idxs[0]
        else:
            lengths = [len(runs[i][0]) for i in idxs]
            mx = max(lengths)
            if lengths.count(mx) > 1:
                continue
            winner = idxs[lengths.index(mx)]
        rp, rv = runs[winner]
        v = key if isinstance(key, int) else key[1]
        verified[rp[rv.index(v)]] = str(v)
    return verified


def verify_folio_sequence(
    candidates: dict[int, str], chapters: list[tuple[int, int]] | None = None
) -> dict[int, str]:
    """The strong proof: keep only members of consistent ascending runs.

    A run (page i..j) is consistent when each consecutive page's folio equals
    the previous + 1 (arabic). Roman-numeral candidates are deliberately NOT
    verified (no arabic parse) — roman front matter stays physical_only,
    honest rather than guessed. Every member of a validated run is
    folio_verified; pages outside any run are NOT verified.

    Chapter-restart folios (per-chapter 1,2,3 numbering) can form SEVERAL
    valid short runs with clashing values — several pages would claim "3"
    under the highest trust. The longest run per VALUE wins; clashing
    shorter runs are dropped entirely (a citation must not silently resolve
    to the earliest chapter's page).

    W12 chapter mode (``chapters`` = corroborated section starts, see
    chapter_restarts): the citation key becomes (chapter ordinal, value) —
    "Kap. 2, S. 3" and "Kap. 5, S. 3" are different pages, so restart
    values no longer clash ACROSS chapters. Within one chapter the
    longest-run/tie-drop rule is unchanged.
    """
    runs = folio_runs(candidates)
    if chapters is None:
        value_runs: dict[int, list[int]] = {}
        for idx, (_, rv) in enumerate(runs):
            for v in rv:
                value_runs.setdefault(v, []).append(idx)
        return _resolve_value_clashes(runs, value_runs)
    key_runs: dict[tuple[int, int], list[int]] = {}
    for idx, (rp, rv) in enumerate(runs):
        ch = chapter_of(rp[0], chapters)
        for v in rv:
            key_runs.setdefault((ch, v), []).append(idx)
    return _resolve_value_clashes(runs, key_runs)


def build_page_trust(pdf_path: str) -> tuple[dict[int, str], dict[int, str], dict[int, int]]:
    """Orchestrate: labels (3-tier extract) + sanity + folio sequence.

    Returns (page_label_map, page_source_map, page_chapter_map) keyed by
    0-based physical page. Label precedence per page: verified folio > sane
    PDF label > physical+1. Trust precedence: folio_verified >
    pdf_label_sane > physical_only. page_chapter_map is non-empty only for
    corroborated chapter-relative books (W12): 1-based chapter ordinal per
    page, the runner stamps it into chunk locators ("Kap. N, S. X", W4).
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
        if tier1 < n * 0.5 or (n > 0 and tier1 * 2 == n):
            # <= semantics: at EXACTLY 50% the extractor itself has already
            # fallen back to tier 2/3 (its gate is empty_count < n*0.5) —
            # routing those labels into assess_labels would stamp fabricated
            # physical+1 as pdf_label_sane (boundary drift, review finding).
            trust, reason = PHYSICAL_ONLY, "no publisher labels (tier 2/3 fallback)"
        else:
            trust, reason = assess_labels(labels, n)
        # The folio sequence is consulted ALWAYS, not only for suspect
        # labels: the offset fault (qa2/f17 — unique, monotone labels
        # LAGGING behind the printed folios) is undetectable by sanity
        # alone; a verified folio run wins over a sane label run because
        # it is the printed truth. (One clipped text-layer pass per book.)
        folio_candidates = extract_folio_candidates(doc)
        # W12: chapter-relative books (healed anchor sections) verify per
        # chapter; without corroboration this is None and everything below
        # behaves exactly as before.
        chapters = chapter_restarts(doc, folio_candidates)
        folio: dict[int, str] = verify_folio_sequence(folio_candidates, chapters)
        page_chapter_map: dict[int, int] = {}
        if chapters:
            for i in range(n):
                ch = chapter_of(i, chapters)
                if ch:
                    page_chapter_map[i] = ch
        if folio:
            logger.info("page_trust: %d/%d pages folio-verified (labels: %s — %s; chapters: %s)",
                        len(folio), n, trust, reason, len(chapters or []))
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
        if not folio and trust != PDF_LABEL_SANE:
            logger.info("page_trust: labels suspect (%s) and no folio sequence — physical_only", reason)
        return page_label_map, page_source_map, page_chapter_map
    finally:
        doc.close()
