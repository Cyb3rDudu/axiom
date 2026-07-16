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
from typing import List, Optional


# ---------------------------------------------------------------------------
# Low-level patterns
# ---------------------------------------------------------------------------

# Markdown heading regex — at least ``##`` (not just ``#`` title).
_HEADING_RE = re.compile(r"(?m)^\s{0,3}#{2,}\s+\S+.*$")

# Numbered list line >= 40 chars.
_NUMBERED_LINE_RE = re.compile(r"(?m)^\s{0,3}\d+\.\s+(.{40,})$")

# Word-count hint.
_WORDCOUNT_RE = re.compile(r"\b\d[\d.,]*\s*(?:Wörter|Worte|words)\b", re.IGNORECASE)

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
    # Walk every heading after the outline header. Keep extending the region
    # as long as each heading is itself a numbered outline section (``# 1.`` /
    # ``## 2.1``). Stop at the first heading that is not a numbered section.
    # Note: _HEADING_RE only matches ``##`` (2+ hashes); the outline's top-level
    # ``# 1.`` sections are NOT matched here, which is fine — they sit between
    # the ``##`` subsections and never terminate the region prematurely.
    end = len(message)
    for m in _HEADING_RE.finditer(message, hm.end()):
        # _HEADING_RE may capture a leading newline as part of the leading
        # whitespace, so strip before splitting to get the actual heading line.
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
    has_outline = len(outline) >= 3
    has_scope = bool(_WORDCOUNT_RE.search(message))
    deliverable_match = _TASK_RE.search(message)
    has_deliverable = bool(deliverable_match)
    deliverable = deliverable_match.group(0) if deliverable_match else None

    is_complete = (
        is_structured
        and has_outline
        and has_scope
        and has_deliverable
    )

    specificity = "complete" if is_complete else ("structured" if is_structured else "open")

    return {
        "specificity": specificity,
        "primary_question": primary_question,
        "questions": questions,
        "outline": [s.to_dict() for s in outline],
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
        "has_outline": False,
        "has_scope": False,
        "has_deliverable": False,
        "deliverable": None,
        "briefing_style": "open",
    }


__all__ = [
    "detect_structured_briefing",
    "extract_leitfragen",
    "extract_primary_leitfrage",
    "extract_outline",
    "classify_assignment",
    "OutlineSection",
]
