"""
Structured-briefing detection for the MessengerAgent path.

When a user sends a fully-specified research briefing (Leitfragen, Gliederung,
word count, etc.) we want axiom to honour it 1:1 rather than distilling the
topic and re-inventing research questions. This module provides:

- ``detect_structured_briefing(msg) -> bool``
- ``extract_leitfragen(msg) -> list[str]``
- ``extract_primary_leitfrage(msg) -> Optional[str]``
- ``extract_outline(msg) -> list[OutlineSection]``
- ``classify_assignment(msg) -> dict``

All pure / testable, no LLM involved.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple


# ---------------------------------------------------------------------------
# Low-level patterns
# ---------------------------------------------------------------------------

# Markdown heading regex — at least ``##`` (not just ``#`` title).
_HEADING_RE = re.compile(r"(?m)^\s{0,3}#{2,}\s+\S+.*$")

# ANY markdown heading (``#``..``######``). Used by ``_outline_region`` to
# detect where the Gliederung ends: the next heading of ANY depth that is
# not itself a numbered outline section terminates the region. (Previously
# only ``##``+ was scanned, so a single-``#`` category like ``# Quellen-``
# ``anforderungen`` after the outline never ended the region, and numbered
# lists in those later sections were pulled in as bogus outline entries.)
_ANY_HEADING_RE = re.compile(r"(?m)^\s{0,3}#{1,6}\s+\S.*$")

# Numbered list line >= 40 chars.
_NUMBERED_LINE_RE = re.compile(r"(?m)^\s{0,3}\d+\.\s+(.{40,})$")

# Word-count hint.
_WORDCOUNT_RE = re.compile(r"\b\d[\d.,]*\s*(?:Wörter|Worte|words)\b", re.IGNORECASE)

# ---------------------------------------------------------------------------
# Word-budget extraction (deterministic, prompt-independent)
# ---------------------------------------------------------------------------
# IMPORTANT: only numbers adjacent to a word-count unit (Wörter/Worte/words)
# qualify as a budget. We deliberately must NOT match "470.000 Euro",
# "85 Mitarbeitende", "13 bis 16 Quellen", etc. This is enforced by anchoring
# the number to the unit and stripping thousands separators carefully.

# A bare integer or a "min bis/–/-/to max" range, in German or English, that is
# IMMEDIATELY followed (possibly after whitespace) by a word-count unit.
# Units include German inflections (Wörter/Worte/Wörtern) and English
# (words/word). Captures the raw number tokens (e.g. "3.000", "230 bis 270",
# "1,100 to 1,200") so they can be parsed into ints after removing thousands
# separators.
_WORD_RANGE_RE = re.compile(
    r"\b(\d[\d.,]*)\s*(?:bis|\u2013|-|to|\u2013|\u2014)\s*(\d[\d.,]*)\s*"
    r"(?:Wörter|Worte|Wörtern|words|word)\b",
    re.IGNORECASE,
)
_WORD_SINGLE_RE = re.compile(
    r"\b(?:ca\.|ca|circa|ungef\u00e4hr|about|around|~)?\s*(\d[\d.,]*)\s*"
    r"(?:Wörter|Worte|Wörtern|words|word)\b",
    re.IGNORECASE,
)

# A per-section scope line, e.g. "Umfang: ungefähr 230 bis 270 Wörter" /
# "Scope: 550-650 words". Captures the full numeric range phrase; the caller
# parses the min/max out. Scoped to a line so we can attach it to the nearest
# preceding numbered outline heading.
_SECTION_SCOPE_RE = re.compile(
    r"(?im)^\s*(?:umfang|scope|length)\s*[:\-]?\s*(.+?(?:Wörter|Worte|words).*)$"
)

# Staged-output directive. A briefing that says "Gib zunächst noch keinen
# vollständigen Fließtext aus" / "Liefere als erste Ausgabe ..." / "do NOT write
# the full text yet" is asking for a PLANNING-ONLY first deliverable (outline,
# thesis, source matrix), NOT an immediate full draft. This must override the
# 'complete -> direct full-draft start' path (Priority 5).
_STAGED_OUTPUT_RE = re.compile(
    r"(?im)(?:"
    r"gib\s+(?:zun(?:ä|ae)chst|zuerst)\s+(?:noch\s+)?keinen?\s+(?:vollst(?:ä|ae)ndigen?\s+)?(?:flie(?:ß|ss)text|text)\s*(?:aus|her)"
    r"|liefere\s+(?:zun(?:ä|ae)chst|zuerst|als\s+erste\s+ausgabe)"
    r"|erstelle\s+(?:zun(?:ä|ae)chst|zuerst|als\s+erste\s+ausgabe)"
    r"|als\s+erste\s+ausgabe\s*[:\-]"
    r"|do\s+not\s+(?:write|generate|produce)\s+(?:the\s+)?(?:full|complete)\s+(?:text|draft|report)\s+(?:yet|first)"
    r"|first\s+(?:deliverable|output|stage)\s*[:\-]"
    r")"
)


def detect_staged_output(message: str) -> bool:
    """Return True when the briefing asks for a planning-only first deliverable.

    Such a briefing should NOT trigger the 'complete -> immediate full-draft
    start' path. Instead the first output should be the outline / theses /
    source matrix, then await user confirmation before writing the full text.
    """
    if not message:
        return False
    return bool(_STAGED_OUTPUT_RE.search(message))


# A monetary figure tagged as a turnover/revenue metric. We capture the
# keyword, number + unit scale so two statements about the same metric can be
# compared. German uses Mio./Million (jahresumsatz/umsatz/erträge) and English
# revenue/turnover. NOTE: we deliberately do NOT use a negative lookahead to
# exclude per-employee figures here — a lookbehind/ahead window large enough to
# catch '470.000 Euro pro Mitarbeitendem' also caught unrelated per-employee
# lines ~40 chars later and rejected the CORRECT '19 Mio.' turnover match (it
# shrank the capture to '1'). Instead, per-employee matches are filtered AFTER
# extraction in extract_case_assumptions() by checking the SAME clause for a
# 'pro/per/je Mitarbeitend' marker.
_TURNOVER_RE = re.compile(
    r"(?im)"
    r"(?:jahresumsatz|umsatz|ertr(?:a|ä)ge|revenue|turnover)"
    r"[^\n]{0,40}?"
    r"(\d[\d.,]*)\s*"
    r"(mio\.|million(en)?|m\.|mrd\.|milliarde(n)?|billion(en)?|eur[ o]?|€)?"
)
# Marker that a turnover candidate is actually a per-employee metric. Checked
# within a SHORT window (same clause) AFTER the number, so an unrelated
# per-employee line further down is not mis-attributed.
_PER_EMPLOYEE_AFTER_RE = re.compile(
    r"(?im)\s*(?:eur[ o]?|€)?\s*(?:pro|per|/|je)\s*(?:mitarbeitend|besch(?:ä|ae)ftigt|employee)"
)
_PER_EMPLOYEE_RE = re.compile(
    r"(?im)"
    r"(\d[\d.,]*)\s*(eur[ o]?|€)\s*(?:pro|per|/)?\s*(?:mitarbeitend(?:em|er)?|besch(?:ä|ae)ftigt(?:em|er)?|employee)"
)
_HEADCOUNT_RE = re.compile(
    r"(?im)(\d[\d.,]*)\s*(?:mitarbeitende|besch(?:ä|ae)ftigte|angestellte|employees|staff|kopf)"
)


@dataclass
class CaseAssumptions:
    """Structured extraction of the 'feste Fallannahmen' in a case-study briefing.

    Each field holds a list of ``(normalised_value, raw_text)`` pairs. We keep
    ALL occurrences (not just one) so conflict detection can compare multiple
    stated figures, and so a user correction can REPLACE every value for a
    field it mentions (resolving the conflict deterministically rather than by
    string concatenation — review finding 1).
    """
    turnovers: List[Tuple[int, str]] = field(default_factory=list)
    per_employees: List[Tuple[int, str]] = field(default_factory=list)
    headcounts: List[Tuple[int, str]] = field(default_factory=list)

    def has_turnover(self) -> bool:
        return bool(self.turnovers)

    def has_per_employee(self) -> bool:
        return bool(self.per_employees)

    def has_headcount(self) -> bool:
        return bool(self.headcounts)


def _normalise_turnover(raw_num: str, unit: str) -> Optional[int]:
    """Normalise a turnover token to absolute units (euros)."""
    n = _parse_int(raw_num)
    if n is None or n <= 0:
        return None
    unit = (unit or "").lower()
    if "mio" in unit or "million" in unit or unit in ("m.",):
        return n * 1_000_000
    if "mrd" in unit or "milliard" in unit:
        return n * 1_000_000_000
    if "billion" in unit:
        return n * 1_000_000_000_000
    return n  # plain euros (e.g. 470.000 Euro)


def extract_case_assumptions(message: str) -> CaseAssumptions:
    """Extract the structured case assumptions from a briefing or correction.

    Used by ``detect_case_assumption_conflicts`` and by the correction-resolution
    flow (review finding 1): a follow-up correction is parsed with the same
    extractor so we know precisely WHICH assumption fields it addresses, and can
    override only those fields while leaving untouched ones in place.
    """
    ca = CaseAssumptions()
    if not message:
        return ca
    for m in _TURNOVER_RE.finditer(message):
        # Post-filter (was a fragile negative lookahead): a turnover candidate
        # like 'Umsatzleistung 470.000 Euro pro Mitarbeitendem' is actually a
        # per-employee metric, not the company total. We detect that by checking
        # the SAME clause (a short window right after the number/unit) for a
        # 'pro/per/je Mitarbeitend' marker. An unrelated per-employee line
        # further down is NOT mis-attributed because the window is short.
        tail = message[m.end(): m.end() + 30]
        if _PER_EMPLOYEE_AFTER_RE.match(tail):
            continue
        value = _normalise_turnover(m.group(1), m.group(2))
        if value is not None:
            ca.turnovers.append((value, m.group(0).strip()))
    for m in _PER_EMPLOYEE_RE.finditer(message):
        n = _parse_int(m.group(1))
        if n is not None and n > 0:
            ca.per_employees.append((n, m.group(0).strip()))
    for m in _HEADCOUNT_RE.finditer(message):
        n = _parse_int(m.group(1))
        if n is not None and n > 0:
            ca.headcounts.append((n, m.group(0).strip()))
    return ca


def conflicts_in_assumptions(ca: CaseAssumptions) -> List[str]:
    """Detect contradictory case assumptions from a structured ``CaseAssumptions``.

    Pure: the same logic ``detect_case_assumption_conflicts`` used to inline, now
    factored out so it can run on EITHER the raw text OR a corrected/merged
    assumption set (review finding 1).

    Currently detects:
      - Two different annual-turnover figures for the same case company
        (e.g. 'Jahresumsatz: 19 Mio. Euro' vs 'rund 40 Mio. Euro').
      - A per-employee revenue figure inconsistent with turnover/headcount
        (e.g. 470.000 €/employee × 85 employees ≈ 40 Mio. ≠ stated 19 Mio.).
    Conservative: only flags when the gap is large (>25%) and the numbers are
    explicitly anchored to the same metric keyword.
    """
    conflicts: List[str] = []

    # --- Turnover conflicts: multiple distinct turnover figures ---
    distinct: Dict[int, str] = {}
    for value, raw in ca.turnovers:
        bucket = round(value, -4)  # bucket to 10k to absorb rounding noise
        distinct.setdefault(bucket, raw)
    turnover_vals = sorted(distinct.keys())
    if len(turnover_vals) >= 2:
        lo, hi = turnover_vals[0], turnover_vals[-1]
        # Two genuinely different turnover figures (gap > 25%).
        if lo > 0 and (hi - lo) / hi > 0.25:
            conflicts.append(
                f"Widersprüchliche Umsatz-Fallannahmen: "
                f"'{distinct[lo]}' vs '{distinct[hi]}' (Abweichung > 25%)."
            )

    # --- Per-employee revenue vs turnover×headcount consistency ---
    per_emp = ca.per_employees[0][0] if ca.per_employees else None
    headcount = ca.headcounts[0][0] if ca.headcounts else None
    if per_emp and headcount and per_emp > 0 and headcount > 0:
        implied = per_emp * headcount
        # Compare against the largest stated turnover (the 'real' one).
        if turnover_vals:
            stated = max(turnover_vals)
            if stated > 0:
                ratio = implied / stated
                # Flag if implied turnover differs from stated by > 25% in EITHER
                # direction, but only when implied is in a plausible turnover
                # range (> 100k, i.e. not a per-section word count coincidence).
                if implied > 100_000 and (ratio > 1.25 or ratio < 0.8):
                    conflicts.append(
                        f"Umsatzleistung/Personal-Zahl inkonsistent mit Umsatz: "
                        f"{per_emp} € × {headcount} = {implied:,} € vs "
                        f"angegebener Umsatz ~{stated:,} €."
                    )

    return conflicts


def detect_case_assumption_conflicts(message: str) -> List[str]:
    """Detect contradictory 'feste Fallannahmen' in a case-study briefing.

    Returns a list of human-readable conflict descriptions. When non-empty,
    classify_assignment should downgrade specificity to
    'structured_needs_clarification' so the user resolves the conflict before
    the mission starts (the writer must not silently pick one figure).
    """
    return conflicts_in_assumptions(extract_case_assumptions(message))


def resolve_case_assumptions(
    original_text: str, correction_text: str
) -> Tuple[CaseAssumptions, List[str]]:
    """Merge a user correction into the original case assumptions.

    A correction (the user's follow-up while ``awaiting_clarification`` is set)
    is interpreted as an AUTHORITATIVE override for every assumption field it
    explicitly mentions. For each field the correction touches, the original's
    values for that field are fully REPLACED (not concatenated) — so e.g. a
    follow-up "Jahresumsatz ist 40 Mio. Euro" replaces the original "19 Mio."
    instead of adding a second, still-contradictory figure (review finding 1:
    the old text-concatenation approach could never resolve a conflict).

    Fields the correction does NOT mention keep the original's values.

    Returns ``(merged_assumptions, remaining_conflicts)``. When
    ``remaining_conflicts`` is empty the conflict is resolved and the caller can
    clear ``awaiting_clarification`` and persist the merged assumptions.
    """
    orig = extract_case_assumptions(original_text)
    corr = extract_case_assumptions(correction_text)

    # Correction overrides ONLY the fields it explicitly mentions.
    turnovers = corr.turnovers if corr.has_turnover() else orig.turnovers
    per_employees = corr.per_employees if corr.has_per_employee() else orig.per_employees
    headcounts = corr.headcounts if corr.has_headcount() else orig.headcounts

    merged = CaseAssumptions(
        turnovers=turnovers, per_employees=per_employees, headcounts=headcounts
    )
    return merged, conflicts_in_assumptions(merged)


def _parse_int(s: str) -> Optional[int]:
    """Parse a number token like ``3.000`` / ``1,200`` / ``3000`` into an int.

    German uses ``.`` as a thousands separator and ``,`` as a decimal point;
    English is the reverse. A trailing decimal part is dropped (word budgets
    are integers). Returns None if the token is not a clean integer.
    """
    s = s.strip().replace(".", "").replace(",", "")
    if not s.isdigit():
        # Allow a trailing .0 / ,0 style that survived the strip — unlikely
        # after removing both separators, but be safe.
        if s.replace("0", "").isdigit() and s.endswith("0"):
            pass
        else:
            return None
    try:
        return int(s)
    except ValueError:
        return None


def _parse_range(phrase: str) -> Optional[tuple[int, int]]:
    """Parse a numeric word-count phrase into ``(min, max)``.

    Accepts:
      - "230 bis 270 Wörter"        -> (230, 270)
      - "1,100 to 1,200 words"      -> (1100, 1200)
      - "ca. 3000 Wörter"           -> (2700, 3300)   # ±10% window
      - "ungefähr 3.000 Wörter"     -> (2700, 3300)
      - "3000-3500 Wörter"          -> (3000, 3500)
    Returns None when no clean number is adjacent to a word-count unit.
    """
    # Range first (min bis/–/-/to max).
    m = _WORD_RANGE_RE.search(phrase)
    if m:
        lo = _parse_int(m.group(1))
        hi = _parse_int(m.group(2))
        if lo is not None and hi is not None and lo <= hi:
            return lo, hi
        # swapped range (e.g. "500 to 300") — take the ordered pair
        if lo is not None and hi is not None:
            return min(lo, hi), max(lo, hi)
    # Single number -> ±10% window (a single figure is a target, not a max).
    m = _WORD_SINGLE_RE.search(phrase)
    if m:
        n = _parse_int(m.group(1))
        if n is not None and n > 0:
            return max(1, round(n * 0.9)), round(n * 1.1)
    return None


class WordBudget:
    """Structured word-count budget extracted from a briefing.

    Deterministic (no LLM). Drives the planner's per-section budget contract
    and the writer's hard ``max_tokens`` limit, so a briefing that says
    "ca. 3.000 Wörter" cannot balloon into 47.000.

    - ``total``: ``(min, max, target)`` for the whole document body, or None.
    - ``sections``: maps an outline NUMBER (``"1"``, ``"2.1"``) to ``(min, max)``.
      Keyed by number (not title) so it survives title normalisation.
    - ``source``: short human-readable note about where the budget came from,
      for debugging / budget_source on the section model.
    """

    __slots__ = ("total", "sections", "source")

    def __init__(
        self,
        total: Optional[tuple[int, int, int]] = None,
        sections: Optional[dict[str, tuple[int, int]]] = None,
        source: str = "",
    ):
        self.total = total
        self.sections = sections or {}
        self.source = source

    @property
    def has_any(self) -> bool:
        return self.total is not None or bool(self.sections)

    def to_dict(self) -> dict:
        total = None
        if self.total:
            lo, hi, tgt = self.total
            total = {"min": lo, "target": tgt, "max": hi}
        return {
            "total_word_budget": total,
            "section_word_budgets": {
                k: [lo, hi] for k, (lo, hi) in self.sections.items()
            },
            "budget_source": self.source,
        }

    def __repr__(self) -> str:  # pragma: no cover
        return f"WordBudget(total={self.total!r}, sections={self.sections!r}, source={self.source!r})"


def extract_word_budget(message: str) -> WordBudget:
    """Extract the total + per-section word budget from a briefing.

    Deterministic. Only counts numbers directly attached to a word-count unit
    (Wörter/Worte/words); "470.000 Euro", "85 Mitarbeitende" and "13 bis 16
    Quellen" never qualify because the unit anchor is missing.

    Per-section budgets are scoped to the Gliederung region and attached to the
    nearest preceding numbered outline heading (``## 1.`` / ``### 2.1``), so an
    "Umfang: 230 bis 270 Wörter" line lands on section ``"1"``, not on a bare
    instruction list.
    """
    if not message:
        return WordBudget()

    total: Optional[tuple[int, int, int]] = None
    total_source = ""
    sections: dict[str, tuple[int, int]] = {}
    source_bits: list[str] = []

    # --- TOTAL budget: a word count stated BEFORE the Gliederung region ---
    # The document total is conventionally stated up top ("Die Hausarbeit
    # umfasst ca. 3.000 Wörter"). A per-section scope line like "230 bis 270
    # Wörter" sits INSIDE the Gliederung region and must not be mistaken for
    # the total. So: only consider word-count mentions that appear before the
    # outline region starts (or before any Gliederung/Outline header).
    region = _outline_region(message)
    total_search_end = region[0] if region is not None else len(message)
    total_search_end = _OUTLINE_HEADER_RE.search(message).start() \
        if _OUTLINE_HEADER_RE.search(message) else total_search_end
    head = message[:total_search_end]
    m_range = _WORD_RANGE_RE.search(head)
    m_single = _WORD_SINGLE_RE.search(head)
    candidates = []
    if m_range:
        candidates.append((m_range.start(), "range", m_range))
    if m_single:
        candidates.append((m_single.start(), "single", m_single))
    candidates.sort(key=lambda c: c[0])
    if candidates:
        _, kind, m = candidates[0]
        if kind == "range":
            lo = _parse_int(m.group(1)); hi = _parse_int(m.group(2))
            if lo is not None and hi is not None:
                lo, hi = (lo, hi) if lo <= hi else (hi, lo)
                total = (lo, hi, (lo + hi) // 2)
                total_source = f"total range '{m.group(0).strip()}'"
        else:
            n = _parse_int(m.group(1))
            if n is not None and n > 0:
                total = (round(n * 0.9), round(n * 1.1), n)
                total_source = f"total single '{m.group(0).strip()}' (±10%)"

    # --- PER-SECTION budgets: scan the outline region, attach scope lines ---
    if region is not None:
        r_start, r_end = region
        region_text = message[r_start:r_end]
        # Map each section_scope line to the nearest preceding numbered heading.
        # Build a list of (char_pos_in_region, number) for every outline heading.
        heading_positions: list[tuple[int, str]] = []
        for hm in _OUTLINE_SECTION_RE.finditer(region_text):
            prefix = region_text[hm.start(): hm.start() + 6]
            marker_match = re.match(r"\s*(#{1,6})\s*", prefix)
            if not marker_match:
                continue  # bare numbered list — not a real heading
            heading_positions.append((hm.start(), hm.group(1)))
        for sm in _SECTION_SCOPE_RE.finditer(region_text):
            phrase = sm.group(1)
            rng = _parse_range(phrase)
            if rng is None:
                continue
            # Find nearest preceding heading.
            sp = sm.start()
            nearest_num = None
            for hp, num in heading_positions:
                if hp < sp:
                    nearest_num = num
                else:
                    break
            if nearest_num is not None:
                sections[nearest_num] = rng
                source_bits.append(f"sec {nearest_num}: {phrase.strip()}")

    source = total_source
    if source_bits:
        source = (source + "; " if source else "") + "; ".join(source_bits[:8])

    return WordBudget(total=total, sections=sections, source=source)

# Academic task keywords.
_TASK_RE = re.compile(
    r"\b(?:Hausarbeit|Seminararbeit|Bachelor(?:arbeit)?|Master(?:arbeit)?|Diplomarbeit|"
    r"Dissertation|Term\s+Paper|Case\s+Study|Essay|Thesis|Modulprüfung)\b",
    re.IGNORECASE,
)

# Source-pool / methodology directives (plural of Quellen, Datenbanken, zitierstil).
_SOURCES_RE = re.compile(
    r"\b(?:APA[\s\-]?7|Zitier(?:stil|richtlin|ung)|Literaturportfolio|"
    r"Quellen(?:liste|strategie|typen|portfolio|angabe)?|Datenbanken|Recherchetool|"
    r"ProQuest|CrossRef|OpenAlex|Peer[\s\-]?Review|facheinschlägig|Bewertungskriterien)\b",
    re.IGNORECASE,
)

# Generic Leitfragen/Fragen header (singular OR plural) — used to locate the
# block of numbered sub-questions.
_LEITFRAGEN_HEADER_RE = re.compile(
    r"(?im)^\s{0,3}#{1,6}\s+.*(?:"
    r"leitfrage(n)?|forschungsfrage(n)?|research\s+questions?|pflicht-?frage(n)?|frage(n)?|questions"
    r")"
)

# A SINGULAR primary-question header (Leitfrage / Forschungsfrage / zentrale Frage)
# — deliberately excludes the plural "Leitfragen"/"Fragen" which hold sub-questions.
_PRIMARY_LEITFRAGE_HEADER_RE = re.compile(
    r"(?im)^\s{0,3}#{1,6}\s+.*?(?:"
    r"leitfrage(?![n])|forschungsfrage(?![n])|zentrale\s+frage|research\s+question"
    r")"
)

# Explicit lead-in phrasing, e.g. "Verwende folgende Leitfrage:" / "Die Leitfrage lautet:".
_PRIMARY_LEITFRAGE_LEADIN_RE = re.compile(
    r"(?im)(?:verwende|nutze|verfolge|die\s+leitfrage\s+(?:ist|lautet)|"
    r"zentrale\s+leitfrage\s*(?:ist|lautet)?)\s*(?:folgende\s*)?leitfrage\s*[:\-]"
)

# A primary research question is usually a quoted sentence or a sentence ending
# in "?". German typographic quotes („… ") are common in academic briefings.
_QUOTED_SPAN_RE = re.compile(
    r"[\u201E\u00AB\u201C\u201D\"]([^\u201E\u00AB\u201C\u201D\u00BB\"]{15,600}?)[\u201D\u00BB\u201C\u201E\"]",
    re.DOTALL,
)
# A standalone question sentence (line ending with "?").
_QUESTION_LINE_RE = re.compile(r"(?m)^\s{0,3}([^\n]{15,400}\?)\s*$")

# An outline section line, e.g. ``# 1. Einleitung``, ``## 2.1 Foo``, ``1. Einleitung``.
# IMPORTANT: a bare numbered Leitfrage line (``1. Welche Rolle ...?``) also matches
# this, so callers MUST scope it to an outline-header region, never the whole text.
_OUTLINE_SECTION_RE = re.compile(r"(?m)^\s{0,3}#{0,6}\s*(\d+(?:\.\d+)*)\.?\s+(\S.*)$")

_OUTLINE_HEADER_RE = re.compile(
    r"(?im)^\s{0,3}#{1,6}\s+.*(?:"
    r"gliederung|struktur|outline|aufbau|soll[-\s]?gliederung|table\s+of\s+contents"
    r")"
)


# ---------------------------------------------------------------------------
# Structured outline extraction (Finding 1)
# ---------------------------------------------------------------------------

class OutlineSection:
    """A single section parsed out of a user's Gliederung.

    ``number`` is the original numeric label (``"1"``, ``"2.1"``) kept separately
    so the planner can reproduce the user's hierarchy WITHOUT baking the number
    into the ``title`` (which caused duplicated headings like ``# 1. 1. Einleitung``).
    ``title`` is the cleaned, number-free heading text.
    ``level`` is derived from the number depth (``1`` -> 1, ``2.1`` -> 2).
    """

    __slots__ = ("number", "title", "level", "heading_marker")

    def __init__(self, number: str, title: str, heading_marker: str = ""):
        self.number = number
        self.title = title
        self.level = (number.count(".") + 1) if number else 1
        self.heading_marker = heading_marker

    def __repr__(self) -> str:  # pragma: no cover - debugging only
        return f"OutlineSection(number={self.number!r}, title={self.title!r}, level={self.level})"

    def to_dict(self) -> dict:
        return {"number": self.number, "title": self.title, "level": self.level}


def _strip_number_prefix(title: str) -> str:
    """Remove a leading numeric label like ``1.`` / ``2.1 `` from a heading title."""
    # Collapse the "1. 1. Einleitung" double-numbering the planner produced when
    # it prepended its own number to a title that still carried the briefing number.
    title = re.sub(r"^\s*\d+(?:\.\d+)*\.?\s*-?\s*", "", title)
    return title.strip()


def _outline_region(message: str) -> Optional[tuple[int, int]]:
    """Return the (start_char, end_char) span of the Gliederung/Outline section.

    Starts at the outline header. The region extends across all numbered
    outline sections (including nested ``## 2.1`` subsections) and only ends at
    the next heading that is NOT itself a numbered outline section — e.g. a
    new category like ``## Literaturanforderungen`` or ``## Arbeitsauftrag``.
    Returns None if no outline header is present.
    """
    hm = _OUTLINE_HEADER_RE.search(message)
    if not hm:
        return None
    start = hm.start()
    # Walk every heading (of ANY depth) after the outline header. Keep
    # extending the region as long as each heading is itself a numbered
    # outline section (``# 1.`` / ``## 2.1``). Stop at the first heading that
    # is not a numbered section. Using ANY-depth headings (not just ``##``+)
    # means a later single-``#`` category like ``# Quellenanforderungen`` now
    # correctly terminates the region instead of letting numbered lists in
    # the post-outline body leak in as bogus outline sections.
    end = len(message)
    for m in _ANY_HEADING_RE.finditer(message, hm.end()):
        heading_line = m.group(0).strip().splitlines()[0]
        if not re.match(r"\s*#{1,6}\s*\d+(?:\.\d+)*\.?\s+\S", heading_line):
            # This heading has no numeric prefix -> a new category; region ends.
            end = m.start()
            break
    return start, end


def extract_outline(message: str) -> List[OutlineSection]:
    """Extract the structured Gliederung from a briefing.

    Returns one ``OutlineSection`` per numbered heading found INSIDE the
    Gliederung/Outline region. Each section's ``title`` is the number-free
    heading. Numbered Leitfragen outside the outline region are ignored.

    Example input::

        ## Empfohlene Gliederung
        # 1. Einleitung
        Umfang: ca. 250 Wörter
        ## 2.1 NexMach als Unternehmung
        # 3. Analyse

    -> [OutlineSection("1","Einleitung"), OutlineSection("2.1","NexMach als Unternehmung"),
        OutlineSection("3","Analyse")]
    """
    if not message:
        return []
    region = _outline_region(message)
    if region is None:
        return []
    start, end = region
    region_text = message[start:end]

    sections: List[OutlineSection] = []
    seen_titles: set = set()
    for m in _OUTLINE_SECTION_RE.finditer(region_text):
        number = m.group(1)
        raw_title = m.group(2).strip()
        # Capture the markdown marker (e.g. "##") preceding the number, if any.
        prefix = region_text[m.start(): m.start() + 6]
        marker_match = re.match(r"\s*(#{1,6})\s*", prefix)
        marker = marker_match.group(1) if marker_match else ""
        title = _strip_number_prefix(raw_title)
        # Skip lines that are obviously questions (Leitfragen that slipped in),
        # very short fragments, or duplicates of a title already seen.
        if not title or len(title) < 3:
            continue
        if title.endswith("?"):
            continue
        if title.lower() in seen_titles:
            continue
        seen_titles.add(title.lower())
        sections.append(OutlineSection(number=number, title=title, heading_marker=marker))

    # A real Gliederung marks its sections with markdown headings (``# 1.``,
    # ``## 2.1``). Instruction text inside the outline — e.g. the per-factor
    # structure block "Für jeden Faktor ist folgende Struktur einzuhalten:
    # 1. Beschreibung ... 2. Beleg ..." nested under ``### 4.2`` — uses BARE
    # numbered lists (no ``#``). Such bare items were collected as bogus
    # top-level sections, which made the planner invent ~17 sections instead
    # of the required 6. When the outline uses markdown headings at all, drop
    # any item lacking a heading marker so only genuine headings survive.
    # (If NO item carries a marker — a plain numbered-list outline — we keep
    # everything, since there the bare numbers ARE the headings.)
    if any(s.heading_marker for s in sections):
        sections = [s for s in sections if s.heading_marker]
    return sections


# ---------------------------------------------------------------------------
# Structural-briefing detection
# ---------------------------------------------------------------------------

def _count_multiple_numbered_items(text: str) -> int:
    """Return the count of numbered items when there is a bloc of >= 3 of them."""
    matches = [m for m in _NUMBERED_LINE_RE.finditer(text)]
    if len(matches) < 3:
        return 0
    lines = text.splitlines()
    char_to_line = {}
    idx = 0
    for i, line in enumerate(lines):
        for _ in range(len(line) + 1):
            char_to_line[idx] = i
            idx += 1
    line_nums = [char_to_line.get(m.start(), 0) for m in matches]
    window = 40
    for i in range(len(line_nums) - 2):
        if line_nums[i + 2] - line_nums[i] <= window:
            return len(matches)
    return 0


def detect_structured_briefing(message: str) -> bool:
    """Return True when the message looks like a structured research briefing.

    Conservative: requires >= 3 signal groups to fire, so casual prompts and
    short questions stay classified as open research.
    """
    if not message or len(message) < 200:
        # Too short to contain a real briefing.
        return False

    signals = 0

    # Signal 1: two or more markdown ``##`` headings.
    headings = _HEADING_RE.findall(message)
    if len(headings) >= 2:
        signals += 1

    # Signal 2: >= 3 numbered Leitfragen-style lines.
    if _count_multiple_numbered_items(message) >= 3:
        signals += 1

    # Signal 3: word-count hint.
    if _WORDCOUNT_RE.search(message):
        signals += 1

    # Signal 4: academic-task keyword.
    if _TASK_RE.search(message):
        signals += 1

    # Signal 5: source-pool / methodology directive.
    if _SOURCES_RE.search(message):
        signals += 1

    return signals >= 3


# ---------------------------------------------------------------------------
# Header-region helpers
# ---------------------------------------------------------------------------

def _line_offsets(message: str) -> List[int]:
    """Return character offset of the start of each line."""
    offsets = [0]
    for i, ch in enumerate(message):
        if ch == "\n":
            offsets.append(i + 1)
    return offsets


def _header_line_numbers(message: str, pattern: re.Pattern) -> List[int]:
    """Return line numbers of headers matching ``pattern``."""
    offsets = _line_offsets(message)
    line_nums: List[int] = []
    for m in pattern.finditer(message):
        start = m.start()
        for i, off in enumerate(offsets):
            if off > start:
                line_nums.append(i - 1)
                break
        else:
            line_nums.append(len(offsets) - 1)
    return line_nums


def _nearest_header(start_line: int, headers: List[int]) -> int:
    """Return the line number of the nearest preceding header, or -1."""
    best = -1
    for h in headers:
        if h <= start_line and h > best:
            best = h
    return best


def _is_under_header(start_line: int, headers_for: List[int], other_headers: List[int]) -> bool:
    """Return True when the nearest preceding header (any kind) belongs to ``headers_for``."""
    nearest_for = _nearest_header(start_line, headers_for)
    nearest_other = _nearest_header(start_line, other_headers)
    return nearest_for != -1 and nearest_for >= nearest_other


def _collect_numbered_blocks(message: str) -> List[tuple[int, List[str]]]:
    """Return every contiguous block of numbered lines as (starting_line_index, items)."""
    lines = message.splitlines()
    num_re = re.compile(r"^\s{0,3}(\d+)\.\s+(.*)$")
    blocks: List[tuple[int, List[str]]] = []
    current: List[str] = []
    current_start = -1
    for i, line in enumerate(lines):
        m = num_re.match(line)
        if m:
            if not current:
                current_start = i
            current.append(m.group(2).strip())
        else:
            if current and line.strip() and line.startswith((" ", "\t")):
                current[-1] = (current[-1] + " " + line.strip()).strip()
                continue
            if current:
                blocks.append((current_start, current))
            current = []
            current_start = -1
    if current:
        blocks.append((current_start, current))
    return blocks


# ---------------------------------------------------------------------------
# Leitfragen extraction
# ---------------------------------------------------------------------------

def extract_leitfragen(message: str) -> List[str]:
    """Extract the user's numbered Leitfragen (sub-questions) from the message.

    Selection order:
      1. First numbered block appearing below a heading whose text contains
         "Leitfragen" / "Forschungsfragen" / "Fragen" / "Questions".
      2. If none, the longest numbered block that is NOT below a "Gliederung"
         / "Outline" / "Struktur" heading.
      3. Fallback: longest numbered block overall.
    Returns at least 3 items (each >= 30 chars), otherwise an empty list.
    """
    if not message:
        return []

    blocks = _collect_numbered_blocks(message)
    if not blocks:
        return []

    fragen_headers = _header_line_numbers(message, _LEITFRAGEN_HEADER_RE)
    outline_headers = _header_line_numbers(message, _OUTLINE_HEADER_RE)

    # (1) Prefer blocks directly under a Leitfragen/Forschungsfragen header.
    for start_line, items in blocks:
        if _is_under_header(start_line, fragen_headers, outline_headers):
            cleaned = [_strip_number_prefix(q) for q in items if len(q) >= 30]
            if len(cleaned) >= 3:
                return cleaned

    # (2) Longest block that is NOT under an Outline/Gliederung header.
    non_outline = [
        (start, items)
        for start, items in blocks
        if not _is_under_header(start, outline_headers, fragen_headers)
    ]
    pool = non_outline or blocks
    best: List[str] = []
    for _, items in pool:
        if len(items) > len(best):
            best = items

    if len(best) < 3:
        return []
    cleaned = [_strip_number_prefix(q) for q in best if len(q) >= 30]
    return cleaned if len(cleaned) >= 3 else []


def extract_primary_leitfrage(message: str) -> Optional[str]:
    """Return the primary research question (Leitfrage) when present.

    A primary Leitfrage is a SINGLE quoted sentence or standalone question line
    appearing under a SINGULAR Leitfrage/Forschungsfrage heading (or after an
    explicit lead-in like "Verwende folgende Leitfrage:"). This is distinct from
    the numbered sub-questions handled by ``extract_leitfragen``.

    To avoid the bug where the first numbered sub-question under a plural
    "## Pflicht-Leitfragen" / "## Fragen" header was mistaken for the primary
    question, we now ONLY accept:
      1. a question under a singular header, or
      2. a question immediately after an explicit lead-in, or
      3. fallback: first quoted span anywhere (quoted spans are almost never
         numbered sub-questions).
    """
    if not message:
        return None

    offsets = _line_offsets(message)
    singular_headers = _header_line_numbers(message, _PRIMARY_LEITFRAGE_HEADER_RE)
    fragen_headers = _header_line_numbers(message, _LEITFRAGEN_HEADER_RE)
    outline_headers = _header_line_numbers(message, _OUTLINE_HEADER_RE)

    def _char_range_for_line_block(start_line: int) -> tuple[int, int]:
        """Char range from start_line until the next heading or end of message."""
        start_off = offsets[start_line] if start_line < len(offsets) else 0
        next_off = len(message)
        for m in _HEADING_RE.finditer(message):
            if m.start() > start_off:
                next_off = m.start()
                break
        return start_off, next_off

    # (1) Singular primary-question header (e.g. "## Zentrale Leitfrage").
    for h_line in singular_headers:
        # Skip if this header is itself nested under a Gliederung header.
        if _is_under_header(h_line, outline_headers, singular_headers):
            continue
        start, end = _char_range_for_line_block(h_line)
        block = message[start:end]
        q = _first_question_in(block)
        if q:
            return _clean_question(q)

    # (2) Explicit lead-in phrasing ("Verwende folgende Leitfrage: ...").
    lead = _PRIMARY_LEITFRAGE_LEADIN_RE.search(message)
    if lead:
        tail = message[lead.end():]
        q = _first_question_in(tail)
        if q:
            return _clean_question(q)

    # (3) Fallback: first quoted span anywhere — but ONLY if it is NOT one of the
    # numbered sub-questions (i.e. not directly under a plural "Fragen" header).
    m_q = _QUOTED_SPAN_RE.search(message)
    if m_q:
        q_line = _char_to_line(message, m_q.start(), offsets)
        # If the nearest preceding header is a plural Fragen/Leitfragen header
        # (and not a singular one), treat it as a sub-question, not the primary.
        if not _is_under_header(q_line, fragen_headers, singular_headers):
            return _clean_question(m_q.group(1))

    return None


def _char_to_line(message: str, char_pos: int, offsets: Optional[List[int]] = None) -> int:
    """Return the line number containing ``char_pos``."""
    if offsets is None:
        offsets = _line_offsets(message)
    line = 0
    for i, off in enumerate(offsets):
        if off > char_pos:
            return i - 1
        line = i
    return line


def _first_question_in(text: str) -> Optional[str]:
    """Return the first quoted span, else the first standalone '?' line."""
    m = _QUOTED_SPAN_RE.search(text)
    if m:
        return m.group(1)
    m = _QUESTION_LINE_RE.search(text)
    if m:
        return m.group(1)
    return None


def _clean_question(q: str) -> str:
    """Normalise an extracted question: collapse whitespace, strip surrounding quotes/numbers."""
    quote_chars = "\u201E\u201C\u201D\u00AB\u00BB\u201A\u2018\u2019\"'"
    q = q.strip().strip(quote_chars).strip()
    q = _strip_number_prefix(q)
    q = re.sub(r"\s+", " ", q)
    return q


def _count_outline_sections(message: str) -> int:
    """Count numbered outline section headings, scoped to the Gliederung region.

    Only counts sections that appear under a Gliederung/Outline/Struktur header,
    so numbered Leitfragen elsewhere are NOT mis-counted as outline sections.
    """
    return len(extract_outline(message))


# ---------------------------------------------------------------------------
# Classification
# ---------------------------------------------------------------------------

def classify_assignment(message: str) -> dict:
    """Classify how specific a mission assignment is.

    Returns a dict::

        {
          "specificity": "open" | "structured" | "complete",
          "primary_question": Optional[str],
          "questions": List[str],
          "outline": List[dict],            # structured Gliederung (number/title/level)
          "word_budget": dict,             # {total_word_budget, section_word_budgets, budget_source}
          "has_outline": bool,
          "has_scope": bool,
          "has_deliverable": bool,
          "deliverable": Optional[str],
          "briefing_style": "open" | "structured",
        }

    Thresholds:
      - ``open``       : not a structured briefing.
      - ``structured`` : a briefing was detected but not a complete ready-to-run
                         assignment — present extracted questions, honour the
                         briefing in planning, keep the approval loop.
      - ``complete``   : a structured briefing AND a real Gliederung (>=3 numbered
                         sections inside an outline region) AND a scope (word count)
                         AND a deliverable keyword.
    """
    if not message:
        return _empty_classification()

    is_structured = detect_structured_briefing(message)
    questions = extract_leitfragen(message) if is_structured else []
    primary_question = extract_primary_leitfrage(message) if is_structured else None
    outline = extract_outline(message) if is_structured else []
    word_budget = extract_word_budget(message) if is_structured else WordBudget()
    has_outline = len(outline) >= 3
    has_scope = bool(_WORDCOUNT_RE.search(message)) or word_budget.has_any
    deliverable_match = _TASK_RE.search(message)
    has_deliverable = bool(deliverable_match)
    deliverable = deliverable_match.group(0) if deliverable_match else None

    # Staged-output directive (Priority 5). A briefing that says "Gib zunächst
    # noch keinen vollständigen Fließtext aus" wants a planning-only first
    # deliverable (outline/theses/source matrix), then confirmation. It must
    # NOT trigger the 'complete -> immediate full-draft start' path: we
    # downgrade specificity to 'structured' (keeps the approval loop) and flag
    # output_stage='planning_only' so downstream agents produce the staged
    # deliverable first instead of the full Hausarbeit.
    staged = detect_staged_output(message)

    is_complete = (
        is_structured
        and has_outline
        and has_scope
        and has_deliverable
        and not staged  # a staged briefing is never a direct full-draft start
    )

    # Plausibility check (Priority 6): contradictory case assumptions
    # (e.g. 19 Mio. vs 40 Mio. Euro turnover) block a direct start and surface
    # the conflict list so the user can resolve it first.
    case_conflicts = detect_case_assumption_conflicts(message) if is_structured else []
    if case_conflicts:
        specificity = "structured_needs_clarification"
    else:
        specificity = "complete" if is_complete else ("structured" if is_structured else "open")

    return {
        "specificity": specificity,
        "primary_question": primary_question,
        "questions": questions,
        "outline": [s.to_dict() for s in outline],
        "word_budget": word_budget.to_dict(),
        "output_stage": "planning_only" if staged else "full",
        "case_assumption_conflicts": case_conflicts,
        "has_outline": has_outline,
        "has_scope": has_scope,
        "has_deliverable": has_deliverable,
        "deliverable": deliverable,
        "briefing_style": "structured" if is_structured else "open",
    }


def _empty_classification() -> dict:
    return {
        "specificity": "open",
        "primary_question": None,
        "questions": [],
        "outline": [],
        "word_budget": WordBudget().to_dict(),
        "output_stage": "full",
        "case_assumption_conflicts": [],
        "has_outline": False,
        "has_scope": False,
        "has_deliverable": False,
        "deliverable": None,
        "briefing_style": "open",
    }


__all__ = [
    "detect_structured_briefing",
    "detect_staged_output",
    "detect_case_assumption_conflicts",
    "extract_case_assumptions",
    "conflicts_in_assumptions",
    "resolve_case_assumptions",
    "CaseAssumptions",
    "extract_leitfragen",
    "extract_primary_leitfrage",
    "extract_outline",
    "extract_word_budget",
    "classify_assignment",
    "OutlineSection",
    "WordBudget",
]
