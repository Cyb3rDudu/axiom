"""
Structured-briefing detection for the MessengerAgent path.

When a user sends a fully-specified research briefing (Leitfragen, Gliederung,
word count, etc.) we want axiom to honour it 1:1 rather than distilling the
topic and re-inventing research questions. This module provides:

- `detect_structured_briefing(msg) -> bool`
- `extract_leitfragen(msg) -> list[str]`

Both are pure, testable, no LLM involved.
"""

from __future__ import annotations

import re
from typing import List


# Markdown heading regex — at least ``##`` (not just ``#`` title).
_HEADING_RE = re.compile(r"(?m)^\s{0,3}#{2,}\s+\S+.*$")

# Numbered list line ≥ 40 chars.
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


def _count_multiple_numbered_items(text: str) -> int:
    """Return True when the text contains a bloc of ≥ 3 consecutive numbered lines."""
    matches = [m for m in _NUMBERED_LINE_RE.finditer(text)]
    if len(matches) < 3:
        return 0
    # Count consecutive numbered lines appearing close together.
    # We only care about "≥ 3" vs "fewer".
    lines = text.splitlines()
    line_index = {}
    running_idx = 0
    for i, line in enumerate(lines):
        line_index[running_idx] = i
        running_idx += len(line) + 1  # +1 for newline
    # Simpler: check that at least 3 numbered items exist within a 40-line window.
    positions = [m.start() for m in matches]
    window = 40  # lines proxy; we use character distance approximation
    # Convert char positions to line numbers
    char_to_line = {}
    idx = 0
    for i, line in enumerate(lines):
        for _ in range(len(line) + 1):
            char_to_line[idx] = i
            idx += 1
    line_nums = [char_to_line.get(p, 0) for p in positions]
    for i in range(len(line_nums) - 2):
        if line_nums[i + 2] - line_nums[i] <= window:
            return len(matches)
    return 0


def detect_structured_briefing(message: str) -> bool:
    """Return True when the message looks like a structured research briefing.

    Conservative: requires ≥ 3 signal groups to fire, so casual prompts and
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

    # Signal 2: ≥ 3 numbered Leitfragen-style lines.
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


_LEITFRAGEN_HEADER_RE = re.compile(
    r"(?im)^\s{0,3}#{1,6}\s+.*(?:"
    r"leitfragen|forschungsfragen|research\s+questions?|pflicht-?fragen|fragen|questions"
    r")"
)

_OUTLINE_HEADER_RE = re.compile(
    r"(?im)^\s{0,3}#{1,6}\s+.*(?:"
    r"gliederung|struktur|outline|aufbau|soll[-\s]?gliederung|table\s+of\s+contents"
    r")"
)


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


def _line_offsets(message: str) -> List[int]:
    """Return character offset of the start of each line."""
    offsets = [0]
    for i, ch in enumerate(message):
        if ch == "\n":
            offsets.append(i + 1)
    return offsets


def _header_line_numbers(message: str, pattern: re.Pattern) -> List[int]:
    """Return line numbers of headers matching `pattern`."""
    offsets = _line_offsets(message)
    line_nums: List[int] = []
    for m in pattern.finditer(message):
        start = m.start()
        # Binary-search-ish: find the line containing this offset.
        for i, off in enumerate(offsets):
            if off > start:
                line_nums.append(i - 1)
                break
        else:
            line_nums.append(len(offsets) - 1)
    return line_nums


def extract_leitfragen(message: str) -> List[str]:
    """Extract the user's numbered Leitfragen from the message.

    Selection order:
      1. First numbered block appearing below a heading whose text contains
         "Leitfragen" / "Forschungsfragen" / "Fragen" / "Questions".
      2. If none, the longest numbered block that is NOT below a "Gliederung"
         / "Outline" / "Struktur" heading.
      3. Fallback: longest numbered block overall.
    Returns at least 3 items (each ≥ 30 chars), otherwise an empty list.
    """
    if not message:
        return []

    blocks = _collect_numbered_blocks(message)
    if not blocks:
        return []

    fragen_headers = _header_line_numbers(message, _LEITFRAGEN_HEADER_RE)
    outline_headers = _header_line_numbers(message, _OUTLINE_HEADER_RE)

    def _nearest_header(start_line: int, headers: List[int]) -> int:
        """Return the line number of the nearest preceding header, or -1."""
        best = -1
        for h in headers:
            if h <= start_line and h > best:
                best = h
        return best

    def _is_under_header(start_line: int, headers_for: List[int], other_headers: List[int]) -> bool:
        """Return True when the nearest preceding header (any kind) belongs to headers_for."""
        nearest_for = _nearest_header(start_line, headers_for)
        nearest_other = _nearest_header(start_line, other_headers)
        return nearest_for != -1 and nearest_for >= nearest_other

    # (1) Prefer blocks directly under a Leitfragen/Forschungsfragen header.
    for start_line, items in blocks:
        if _is_under_header(start_line, fragen_headers, outline_headers):
            cleaned = [q for q in items if len(q) >= 30]
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
    cleaned = [q for q in best if len(q) >= 30]
    return cleaned if len(cleaned) >= 3 else []
