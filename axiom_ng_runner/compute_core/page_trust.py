"""Page-label trust pipeline (#173 — locator robustness).

Design principle (dudu's sharpened requirement): NEVER GUESS. Every page
reference carries its trust level visibly:

  folio_verified   printed folio read from the text layer AND verified as a
                   consistent ascending sequence — the ONLY level that may be
                   cited as a printed page
  pdf_label_sane   PDF label, sanity-checked (unique, monotone, plausible) —
                   presentable only WITH a marker
  print_verified   #223 EPUB print folios PROVEN book-internally: the
                   printed TOC's page numbers match the chapter-start
                   anchors (or a sibling PDF proved them, #221) — citable
  derived_from_sibling  #222/#226 page map derived from the PDF sibling
                   via content alignment and INJECTED (declared by the
                   injector in the OPF axiom-page-source meta — the anchor
                   shape mimics native format, provenance is explicit).
                   Round-trip-verified ~99%, but never masquerades as
                   native publisher anchors; below print_verified
  print_unverified #220/#223 monotone publisher page anchors WITHOUT
                   proof (no printed TOC, too few joins) — present as
                   marker pagination, never as print folio; vendor-crawled
                   pagination (ProQuest drift) lives here or is refused
                   (divergent); never silently upgraded
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
BLIND = "blind"  # v2.1: no text layer at all — a scan needing OCR rebuild
PDF_LABEL_SANE = "pdf_label_sane"
PRINT_VERIFIED = "print_verified"      # #223: TOC-proven print folios
DERIVED_FROM_SIBLING = "derived_from_sibling"  # #222: injected sibling map
PRINT_UNVERIFIED = "print_unverified"   # #220: markers without proof
PHYSICAL_ONLY = "physical_only"
NONE = "none"

_FOLIO_LINE = re.compile(r"^\s*(\d{1,4})\s*$")
# v2.1: ELI 'N/M' page-of-total markers (EU official journals: the AI Act
# family paginates '16/144', '51/71' — bare standalone lines, and the ELI
# URL line carrying the N/M at either edge). The numerator is the printed
# folio; the spread guard would otherwise eat the bare form.
_ELI_BARE = re.compile(r"^(\d{1,4})/(\d{1,4})$")
_ELI_TAIL = re.compile(r"(?:^|\s)(\d{1,4})/(\d{1,4})(?:\s|$)")
_ROMAN_LINE = re.compile(r"^\s*([ivxlcdm]{1,7})\s*$", re.IGNORECASE)
_LSERIES = re.compile(r"\bL\s*\d{1,4}\s*/\s*(\d{1,4})\b")  # Amtsblatt: folio = page after the slash
_NUM_IN_LINE = re.compile(r"(?<![\d./(])(\d{1,4})(?![\d./)])")
_YEAR_RANGE = lambda v: 1900 <= v <= 2030
_HEAD_LINE_MAX = 90  # running heads are short; body prose in a band is not a head


def _is_year(v: int) -> bool:
    return 1900 <= v <= 2030


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


def harvest_folio_candidates(doc: pymupdf.Document) -> dict[int, list[tuple[str, str, str]]]:
    """Zone-based multi-form folio harvest (W10 extractor v2).

    Every family from the zone-dump ground truth gets eyes:
      bare      whole line is a number (top OR bottom band) — strongest
      roman     whole line is a roman numeral; trailing roman on a short
                head line (WEO copyright pages) counts as medium
      lseries   'L 333/80' Amtsblatt form — folio is the after-slash page
      lead/mid/trail  numbers inside SHORT band lines (journal running
                heads with issue constants, DORA bottom strips, verso/recto
                heads, IMF bands) — medium: the picker and the sequence
                proof sort them out

    Returns per 0-based page a list of (form, value, zone) in encounter
    order. Nothing here decides trust — verify_folio_sequence remains the
    only path to folio_verified.
    """
    out: dict[int, list[tuple[str, str, str]]] = {}
    for i in range(doc.page_count):
        page = doc[i]
        rect = page.rect
        cands: list[tuple[str, str, str]] = []
        for zone, clip in (
            ("top", pymupdf.Rect(rect.x0, rect.y0, rect.x1, rect.y1 * 0.12)),
            ("bot", pymupdf.Rect(rect.x0, rect.y1 * 0.88, rect.x1, rect.y1)),
        ):
            text = page.get_text("text", clip=clip)
            for line in (l.strip() for l in text.splitlines()):
                if not line:
                    continue
                if _FOLIO_LINE.match(line):
                    v = int(line)
                    if _is_year(v):
                        cands.append(("weak", line, zone))
                    else:
                        cands.append(("bare", line, zone))
                    continue
                if _ROMAN_LINE.match(line):
                    cands.append(("roman", line.lower(), zone))
                    continue
                if len(line) > _HEAD_LINE_MAX:
                    continue  # body prose bleeding into the band: not a head
                m = _LSERIES.search(line)
                if m:
                    cands.append(("lseries", m.group(1), zone))
                    continue
                # v2.1 ELI family: bare '16/144' line (would be eaten by the
                # spread guard below — two numbers, no letters), or the ELI
                # URL line with N/M at either edge. Numerator = folio.
                mb = _ELI_BARE.match(line)
                if mb and not (len(mb.group(1)) > 1 and mb.group(1)[0] == "0"):
                    # leading-zero numerators are issue/artifact numbers
                    # ("0431/2001"), never printed folios
                    cands.append(("eli", mb.group(1), zone))
                    continue
                if "eli" in line.lower():
                    mt = _ELI_TAIL.search(line)
                    if mt and not (len(mt.group(1)) > 1 and mt.group(1)[0] == "0"):
                        cands.append(("eli", mt.group(1), zone))
                        continue
                # roman trailing a short head line ('October 2025 iii')
                mr = re.search(r"\s([ivxlcdm]{1,7})\s*$", line, re.IGNORECASE)
                if mr and len(line.split()) >= 2:
                    cands.append(("roman", mr.group(1).lower(), zone))
                    continue
                nums = _NUM_IN_LINE.findall(line)
                if not nums:
                    continue
                # spread guard: a line that is essentially ONLY numbers
                # (two book pages photographed onto one PDF page) is
                # ambiguous — the zone contributes nothing for it
                if len(nums) >= 2 and len(re.sub(r"[\d\s.,/–-]", "", line)) < 4:
                    continue
                # prose guard: running heads never end in sentence
                # punctuation; body prose bleeding into the band does
                if line[-1] in ".!;:":
                    continue
                stripped = line.lstrip()
                for v in nums:
                    vi = int(v)
                    if _is_year(vi):
                        form = "weak"
                    elif stripped.startswith(v):
                        form = "lead" if v == nums[0] else "mid"
                    elif stripped.endswith(v):
                        form = "trail" if v == nums[-1] else "mid"
                    else:
                        form = "mid"
                    cands.append((form, v, zone))
        if cands:
            out[i] = cands
    return out


def _drop_constants(harvest: dict[int, list[tuple[str, str, str]]], page_count: int) -> None:
    """Running-head constants (issue numbers, article ids, reg fragments)
    repeat on a large share of pages; real folios advance. Only MEDIUM/WEAK
    forms (numbers inside head/text lines) can be constants — a BARE number
    line is a printed folio even when a restart book repeats its values
    ('1' per chapter); those never drop. A medium/weak value on >= 1/3 of
    the pages (min 2) is a constant and drops from medium/weak slots
    everywhere (the Data Act '22' x75 and journal 'VOL. 103 NO. 6' classes)."""
    if page_count < 4:
        return
    limit = max(2, (page_count + 2) // 3)
    seen: dict[str, int] = {}
    for cands in harvest.values():
        for form, v, _ in cands:
            if form in ("lead", "mid", "trail", "weak"):
                seen[v] = seen.get(v, 0) + 1
    consts = {v for v, k in seen.items() if k >= limit}
    if not consts:
        return
    for p in list(harvest):
        kept = [c for c in harvest[p] if not (c[0] in ("lead", "mid", "trail", "weak") and c[1] in consts)]
        if kept:
            harvest[p] = kept
        else:
            del harvest[p]


def _pick_candidates(harvest: dict[int, list[tuple[str, str, str]]]) -> dict[int, str]:
    """One candidate per page: strength first, chain continuation second.

    Strength: bare (top) > bare (bot) > lseries > lead/trail/mid > roman >
    weak (years) — the pre-W10 behavior for top-bare books is preserved
    exactly (first bare top line wins). A medium/weak/roman candidate may
    only win via CHAIN CONTINUATION against the last STRONG pick (bare or
    lseries): journal alternating heads and verso/recto books resolve this
    way, while ascending junk (footnote numbers, ordinals) can never seed a
    chain because it is never strong. Spread pages (two book pages on one
    PDF page) are guarded at LINE level in the harvest (a numbers-only line
    contributes nothing); two separate bare LINES in one zone resolve by
    chain continuation, else first-encounter order (old behavior).
    """
    # eli sits BELOW bare: an N/M marker is strong evidence, but when a page
    # carries BOTH, the classic bare line wins (dry-run regression: junk
    # N/M lines like issue numbers "0431/2001" displaced true bare folios
    # on encounter order when both were strength 0).
    _STRENGTH = {"bare": 0, "eli": 1, "lseries": 1, "lead": 2, "trail": 2, "mid": 3, "roman": 4, "weak": 5}
    picked: dict[int, str] = {}
    last_strong: tuple[int, int] | None = None  # (page, value) of last bare/lseries pick
    for p in sorted(harvest):
        cands = harvest[p]
        prev_v = last_strong[1] if last_strong and last_strong[0] == p - 1 else None
        best = None
        best_key = None
        for form, v, zone in cands:
            vi = _arabic(v)
            cont = False
            if vi is not None and prev_v is not None and vi == prev_v + 1:
                cont = True
            key = (0 if cont else 1, _STRENGTH.get(form, 9), 0 if zone == "top" else 1)
            if best_key is None or key < best_key:
                best, best_key = (form, v, zone), key
        if best is None:
            continue
        picked[p] = best[1]
        if best[0] in ("bare", "eli", "lseries"):
            vi = _arabic(best[1])
            last_strong = (p, vi) if vi is not None else None
    return picked


def extract_folio_candidates(doc: pymupdf.Document) -> dict[int, str]:
    """Folio candidates per page — W10 zone harvester (better eyes).

    Same contract as before (0-based page -> candidate string; roman lower,
    arabic bare): the harvest now reads BOTH bands in every printed form
    (bare, L-series, running-head numbers, embedded first lines, roman
    trailing), drops running constants, and picks per page by strength +
    chain continuation. verify_folio_sequence is still the only proof.
    """
    harvest = harvest_folio_candidates(doc)
    _drop_constants(harvest, doc.page_count)
    return _pick_candidates(harvest)


def offset_map(labels: dict[int, str], verified: dict[int, str]) -> dict:
    """Label↔folio relation for wave stamping (W10).

    Emitted where folios and labels diverge, so the wave operator sees the
    per-book relation without re-deriving it: identity (labels are the
    print), shift (labels lag/lead print by a constant — the z5 class:
    label 149, print 151 -> offset +2, offset = folio - label), divergent
    (no single constant: chapter restarts, mixed sections), none (no
    verified folios — nothing to relate). No new trust semantics: purely
    descriptive.
    """
    pairs: list[tuple[int, int]] = []
    for p, fv in verified.items():
        lv = labels.get(p, "")
        if str(lv).isdigit() and str(fv).isdigit():
            pairs.append((int(str(fv)), int(str(lv))))
    if not pairs:
        return {"type": "none", "samples": 0}
    offsets: dict[int, int] = {}
    for f, l in pairs:
        offsets[f - l] = offsets.get(f - l, 0) + 1
    dom = max(offsets, key=lambda k: offsets[k])
    if offsets[dom] >= len(pairs) * 0.9:
        return {"type": "identity" if dom == 0 else "shift", "offset": dom,
                "samples": len(pairs)}
    return {"type": "divergent", "samples": len(pairs),
            "offsets": {str(k): v for k, v in sorted(offsets.items())}}


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
    corroborated = 0
    for pages, vals in folio_runs(candidates):
        if pages[0] < first_start:
            if pages[-1] >= first_start:
                return None  # crosses out of front matter into section 1
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
        corroborated += 1
    if corroborated == 0:
        return None  # zero folio evidence: never guess from the tree alone
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
        v = key[1]
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
        value_runs: dict[tuple[int, int], list[int]] = {}
        for idx, (_, rv) in enumerate(runs):
            for v in rv:
                value_runs.setdefault((0, v), []).append(idx)
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
        # v2.1 BLIND: a page with NO text layer at all is a scan — it is
        # not "evidence-free physical", it needs an OCR rebuild. Pure
        # classification (the runner never executes OCR); feeds the honest
        # "scan — needs OCR rebuild" marker downstream.
        blind = {
            i for i in range(n)
            if i not in folio and not doc[i].get_text("text").strip()
        }
        page_label_map: dict[int, str] = {}
        page_source_map: dict[int, str] = {}
        for i in range(n):
            if i in blind:
                page_label_map[i] = str(i + 1)
                page_source_map[i] = BLIND
                continue
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
