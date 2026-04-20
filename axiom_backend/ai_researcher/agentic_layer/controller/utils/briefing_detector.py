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


def extract_leitfragen(message: str) -> List[str]:
    """Extract the user's numbered Leitfragen from the message.

    Heuristic: find the longest contiguous block of numbered lines and return
    each as a string. Returns an empty list if no numbered block is found.
    """
    if not message:
        return []

    lines = message.splitlines()
    current: List[str] = []
    best: List[str] = []

    num_re = re.compile(r"^\s{0,3}(\d+)\.\s+(.*)$")

    for line in lines:
        m = num_re.match(line)
        if m:
            current.append(m.group(2).strip())
        else:
            # Allow continuation lines (indented) to extend the last item.
            if current and line.strip() and line.startswith((" ", "\t")):
                current[-1] = (current[-1] + " " + line.strip()).strip()
                continue
            if len(current) > len(best):
                best = current
            current = []

    if len(current) > len(best):
        best = current

    # Only return if we found at least 3 items — otherwise it's not a
    # Leitfragen-list, probably just ordered steps or enumeration noise.
    if len(best) < 3:
        return []

    # Filter out obviously non-question items (lines shorter than 30 chars).
    cleaned = [q for q in best if len(q) >= 30]
    return cleaned if len(cleaned) >= 3 else []
