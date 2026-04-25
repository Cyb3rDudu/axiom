"""Single source of truth for Markdown patterns the writing subsystem parses.

Prior state: the same regexes (section heading, document fence,
references fence, word-count trailer) were duplicated across
writing_continuation.py, writing_response_audit.py,
response_postprocess.py, citation_sync.py, structured_bibliography.py,
and the agent's summariser — with subtle variations that produced
bugs (H1-only vs H1-4, Wortbilanz-only vs "Word count"-aware).

All writing-subsystem code that parses response text SHOULD consume
the patterns + helpers here. Adding a new pattern variant goes here
first.
"""

from __future__ import annotations

import re
from typing import Iterable, Optional, Tuple


# ---------------------------------------------------------------------------
# Section headings
# ---------------------------------------------------------------------------

# Numbered section headings at any Markdown level (H1-H4). The writer
# chooses the depth based on document style:
#   # 1. Section    — flat report style
#   ## 1. Section   — paper with H1 title + H2 sections (academic norm)
#   ### 1. Section  — chapter-with-sections
# The continuation + audit logic cares about the number, not the depth.
SECTION_HEADING_RE = re.compile(
    r"^#{1,4}\s+(\d+)\.\s+([^\n]+)$", re.MULTILINE
)


def iter_section_heads(text: str) -> Iterable[Tuple[int, str, int]]:
    """Yield `(section_index, title, char_offset)` for each numbered heading."""
    for m in SECTION_HEADING_RE.finditer(text or ""):
        yield int(m.group(1)), m.group(2).strip(), m.start()


# ---------------------------------------------------------------------------
# content-block fences
# ---------------------------------------------------------------------------

# Match ```content-block:document … ``` (capture body and span).
DOC_FENCE_RE = re.compile(
    r"```content-block:document\s*\n(?P<body>.*?)\n```",
    re.DOTALL,
)

# Match ```content-block:references … ``` (case-insensitive, multiline).
REFS_FENCE_RE = re.compile(
    r"```content-block:references\s*\n(?P<body>.*?)\n```",
    re.DOTALL | re.IGNORECASE,
)


def extract_document_body(text: str) -> Optional[str]:
    """Return the body of the FIRST content-block:document fence, or None."""
    if not text:
        return None
    m = DOC_FENCE_RE.search(text)
    return m.group("body") if m else None


def extract_references_body(text: str) -> Optional[str]:
    """Return the body of the FIRST content-block:references fence, or None."""
    if not text:
        return None
    m = REFS_FENCE_RE.search(text)
    return m.group("body") if m else None


def replace_document_body(text: str, new_body: str) -> str:
    """Replace the body of the first content-block:document fence in-place.

    Returns `text` unchanged if no document fence is present. Preserves
    everything outside the fence (references block, trailers, prose).
    """
    if not text:
        return text
    m = DOC_FENCE_RE.search(text)
    if m is None:
        return text
    prefix = text[: m.start("body")]
    suffix = text[m.end("body") :]
    return prefix + (new_body or "") + suffix


# ---------------------------------------------------------------------------
# Word-count trailer
# ---------------------------------------------------------------------------

# Match the declared total from a word-count trailer, tolerant of the
# Markdown-formatted variants the writer emits in either language:
#   Wortbilanz: 2910 insgesamt
#   **Wortbilanz: 2.910 Wörter**
#   Wortbilanz (exkl. Titelblatt und Literaturverzeichnis): **2.910 Wörter**
#   Word count: 2910
#   **Word count (excl. title page and bibliography): 2,910 words**
#   Total: 2910 words
WORDCOUNT_TRAILER_RE = re.compile(
    r"(?:Wortbilanz|Word\s*count|Total)"
    r"(?:\s*\([^\)]*\))?\s*[:]\s*\**\s*([\d\.\s,]+)",
    re.IGNORECASE,
)

# Match the full word-count trailer block: header line + optional
# breakdown continuations. A breakdown line looks like one of:
#   - 1. Introduction: 410
#   - Einleitung (410)
#   - Introduction: 410 words
# Stops at a blank line, a Markdown heading (`#`), or a code fence.
WORDCOUNT_BLOCK_RE = re.compile(
    r"(?P<prefix>^\s*(?:\*\*)?)"
    r"(?P<full>(?:Wortbilanz|Word\s*count|Total)"
    r"(?:\s*\([^\)]*\))?\s*[:]\s*[^\n]*"
    r"(?:\n(?:[ \t]*[-•*·][^\n]*|[^\n#`\n]*\([\d\.]+\)[^\n]*))*"
    r")",
    re.MULTILINE | re.IGNORECASE,
)


def parse_declared_wordcount(text: str) -> Optional[int]:
    """Parse the first number on a word-count trailer, or None."""
    if not text:
        return None
    m = WORDCOUNT_TRAILER_RE.search(text)
    if not m:
        return None
    digits = re.sub(r"[^\d]", "", m.group(1))
    if not digits:
        return None
    try:
        return int(digits)
    except ValueError:
        return None
