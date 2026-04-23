"""Post-response integrity audit for SimplifiedWritingAgent output.

Writing-agent responses have three recurring failure modes that look
valid on the surface but break the deliverable:

1. URL-in-parens citations — violates KMU APA 7 Rule 12. Should never
   appear, but slips in when the writer lacked a clean
   author/institution hint.
2. Unbalanced content-block fences — the frontend parser requires a
   closing ``` on every block. Truncation or continuation-merge bugs
   can leave a block open, collapsing it to plain text. The fence
   balancer auto-closes in the agent, but we want to see how often
   that safety net fires.
3. Declared vs actual word-count mismatch — every response starts
   with "Wortbilanz: N insgesamt". A silent mismatch hides a word
   budget miss, which is worse than no declaration.

This module runs all three as pure functions against the final merged
response text and returns a structured result that the API handler
persists on the message row and forwards over WebSocket so the UI can
badge the bubble.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import List, Optional


# Matches a URL wrapped in parentheses. Citation-like usage only —
# markdown hyperlinks are also parens-wrapped but always follow a
# bracketed label, so `[label](url)` is NOT matched here. A standalone
# `(https://…)` inside prose, however, is the Run-1 failure mode.
_URL_IN_PARENS = re.compile(r"(?<!\])\(https?://[^)\s]{1,400}\)")

_WORTBILANZ = re.compile(r"Wortbilanz:\s*([\d\.\s]+)")


@dataclass
class AuditResult:
    """Structured outcome of a single response audit."""

    url_in_parens: List[str] = field(default_factory=list)
    unbalanced_fences: bool = False
    declared_wordcount: Optional[int] = None
    actual_wordcount: int = 0
    wordcount_delta_pct: Optional[float] = None

    @property
    def has_warnings(self) -> bool:
        return bool(
            self.url_in_parens
            or self.unbalanced_fences
            or (
                self.wordcount_delta_pct is not None
                and abs(self.wordcount_delta_pct) > 5.0
            )
        )

    def to_dict(self) -> dict:
        return {
            "url_in_parens_count": len(self.url_in_parens),
            "url_in_parens_samples": self.url_in_parens[:3],
            "unbalanced_fences": self.unbalanced_fences,
            "declared_wordcount": self.declared_wordcount,
            "actual_wordcount": self.actual_wordcount,
            "wordcount_delta_pct": self.wordcount_delta_pct,
            "has_warnings": self.has_warnings,
        }


def _parse_declared_wordcount(content: str) -> Optional[int]:
    match = _WORTBILANZ.search(content)
    if not match:
        return None
    raw = match.group(1)
    # Handle "2.910", "2,910", "2 910" — the writer's been observed to
    # use all three separators.
    digits = re.sub(r"[^\d]", "", raw)
    if not digits:
        return None
    try:
        return int(digits)
    except ValueError:
        return None


def _count_words_outside_fences(content: str) -> int:
    """Word count excluding the ``` fence markers themselves and the
    Wortbilanz header line. Counts the actual prose + block bodies.
    """
    # Strip fence markers so "```content-block:document" doesn't
    # inflate the count. Keep the bodies.
    stripped = re.sub(r"```[^\n]*\n", "", content)
    stripped = stripped.replace("```", "")
    # Drop the entire Wortbilanz header LINE, including the "insgesamt"
    # trailer and any section-breakdown continuation line ("Einleitung
    # (410) · Theorie (520) …") that conventionally follows it.
    stripped = re.sub(
        r"^\s*Wortbilanz:[^\n]*\n(?:[^\n]*\([\d]+\)[^\n]*\n)?",
        "",
        stripped,
        count=1,
        flags=re.MULTILINE,
    )
    # Drop per-section [Wortstand: NNN] markers too — they're scaffolding
    stripped = re.sub(r"\[Wortstand:\s*\d+\]", "", stripped)
    # Standard whitespace split; good enough for German/English prose
    return len(stripped.split())


def audit_writing_response(content: str) -> AuditResult:
    """Run all three integrity checks against a writing response.

    The result is cheap to compute (pure regex + split) and safe on
    partial or malformed input — each check is isolated with
    conservative fallbacks.
    """
    if not content:
        return AuditResult()

    # URL-in-parens citations
    url_hits = _URL_IN_PARENS.findall(content)

    # Fence balance — odd count means an unclosed block
    fence_count = content.count("```")
    unbalanced = fence_count % 2 == 1

    # Wordcount check
    declared = _parse_declared_wordcount(content)
    actual = _count_words_outside_fences(content)
    delta_pct: Optional[float] = None
    if declared is not None and actual > 0:
        delta_pct = ((actual - declared) / actual) * 100.0

    return AuditResult(
        url_in_parens=url_hits,
        unbalanced_fences=unbalanced,
        declared_wordcount=declared,
        actual_wordcount=actual,
        wordcount_delta_pct=delta_pct,
    )
