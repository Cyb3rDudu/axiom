"""Post-response integrity audit for SimplifiedWritingAgent output.

Writing-agent responses have three recurring failure modes that look
valid on the surface but break the deliverable:

1. URL-in-parens citations — a general violation of author-year
   citation styles (APA, KMU APA 7, Harvard) that requires an
   author/institution name rather than a raw URL in the parenthetical.
   Slips in when the writer lacked a clean author hint.
2. Unbalanced content-block fences — the frontend parser requires a
   closing ``` on every block. Truncation or continuation-merge bugs
   can leave a block open, collapsing it to plain text. The fence
   balancer auto-closes in the agent, but we want to see how often
   that safety net fires.
3. Declared vs actual word-count mismatch — the writer often emits a
   trailer line like "Wortbilanz: N" (German house style) or
   "Word count: N" (English). A silent mismatch hides a word budget
   miss, which is worse than no declaration. The deterministic
   recomputer below rewrites the trailer with real counts regardless.

This module runs all three as pure functions against the final merged
response text and returns a structured result that the API handler
persists on the message row and forwards over WebSocket so the UI can
badge the bubble.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import List, Optional, Tuple

from services.writing_i18n import t
from services.writing_markdown import (
    DOC_FENCE_RE,
    SECTION_HEADING_RE,
    WORDCOUNT_BLOCK_RE,
    WORDCOUNT_TRAILER_RE,
    extract_document_body,
    parse_declared_wordcount,
)


# Matches a URL wrapped in parentheses. Citation-like usage only —
# markdown hyperlinks are also parens-wrapped but always follow a
# bracketed label, so `[label](url)` is NOT matched here. A standalone
# `(https://…)` inside prose violates author-year citation styles
# across the board (APA, KMU APA 7, Harvard, etc.).
_URL_IN_PARENS = re.compile(r"(?<!\])\(https?://[^)\s]{1,400}\)")

# Word-count trailer patterns consolidated in writing_markdown. Re-exported
# here as module-level names so external callers that import _WORTBILANZ
# from this module keep working.
_WORDCOUNT_TRAILER = WORDCOUNT_TRAILER_RE
_WORTBILANZ = WORDCOUNT_TRAILER_RE


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
    """Thin alias around the shared writing_markdown helper, kept here
    for backwards compatibility with tests that import this symbol."""
    return parse_declared_wordcount(content)


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


# ---------------------------------------------------------------------------
# Stage 1a — deterministic Wortbilanz recompute
#
# The writer hallucinates word counts reliably — observed delta 15-25%
# across every Hausarbeit turn. Correct behaviour in a KMU context
# requires the number in the Wortbilanz line to match the actual body.
# Backend recomputes and rewrites before the user ever sees the response.
# ---------------------------------------------------------------------------


# Re-export the shared block matcher.
_WORTBILANZ_BLOCK = WORDCOUNT_BLOCK_RE


# Section splitter now accepts H1-H4 (matches writing_markdown's
# SECTION_HEADING_RE). A split point is the start of a numbered heading
# line regardless of depth — this is what lets the Wortbilanz recompute
# see sections in academic papers that use H2 for sections + H1 only
# for the document title.
_DOC_SECTION_SPLIT = re.compile(r"(?=^#{1,4}\s+\d+\.)", re.MULTILINE)


def _extract_document_block(content: str) -> Optional[str]:
    """Return the body of the first content-block:document fence, or None.

    Alias for `writing_markdown.extract_document_body`; kept for tests.
    """
    return extract_document_body(content)


def _strip_for_wordcount(text: str) -> str:
    """Remove non-prose artifacts before counting words.

    What counts as a word: running German/English prose, including
    citations. What does NOT count: figure Markdown, caption italics,
    scaffold markers, fence lines, subsection trailing word counters
    like [410].
    """
    if not text:
        return ""
    # Figure Markdown lines including caption italics (*Abbildung N: ...*)
    text = re.sub(r"!\[[^\]]*\]\([^)]*\)", "", text)
    text = re.sub(r"^\s*\*Abbildung\s+\d+:[^\n]*\*\s*$", "", text, flags=re.MULTILINE)
    # Code/table fences (we already stripped the document block header,
    # but guard against nested fences)
    text = re.sub(r"```[^\n]*\n", "", text)
    text = text.replace("```", "")
    # Per-section word counters [NNN]
    text = re.sub(r"\[\d{2,4}\]", "", text)
    # [Wortstand: NNN] scaffolding
    text = re.sub(r"\[Wortstand:\s*\d+\]", "", text)
    return text


def count_document_words(document_body: str) -> Tuple[int, List[Tuple[str, int]]]:
    """Deterministic word count per section.

    Returns (total, [(section_title, words), ...]). Sections are
    identified by level-1 Markdown headings; any prose before the
    first `# 1.` heading is counted as a "preamble" section.
    """
    if not document_body:
        return 0, []
    cleaned = _strip_for_wordcount(document_body)
    parts = _DOC_SECTION_SPLIT.split(cleaned)
    sections: List[Tuple[str, int]] = []
    total = 0
    for part in parts:
        part = part.strip()
        if not part:
            continue
        head_match = re.match(r"^# (\d+\.\s+[^\n]+)", part)
        if head_match:
            title = head_match.group(1).strip()
            body = part[head_match.end():]
        else:
            title = "preamble"
            body = part
        words = len(body.split())
        sections.append((title, words))
        total += words
    return total, sections


def _format_wortbilanz(
    total: int,
    sections: List[Tuple[str, int]],
    language_code: str = "en",
) -> str:
    """Render the word-count trailer in the active language."""
    de = (language_code or "en").lower().startswith("de")
    # German convention: 2.910 (dot as thousands separator)
    total_str = f"{total:,}".replace(",", ".") if de else f"{total:,}"
    header = t("wordcount.trailer_header", language_code, total=total_str)
    lines = [header]
    for title, words in sections:
        if title == "preamble":
            continue
        lines.append(f"- {title}: {words}")
    return "\n".join(lines)


def recompute_wortbilanz(
    content: str, language_code: Optional[str] = None
) -> Tuple[str, Optional[dict]]:
    """Replace the word-count trailer in a writer response with real counts.

    Returns (updated_content, telemetry_dict). The telemetry dict holds
    the declared / actual / delta numbers so the caller can ship them
    to observability without re-parsing.

    `language_code` controls the surface language of the rendered
    trailer (default inferred from the content: German if a Wortbilanz
    line is detected, English otherwise). Explicit override wins.

    Safe on inputs that don't contain a word-count trailer — returns
    the original content unchanged and a minimal telemetry dict.
    """
    if not content:
        return content, None

    doc_body = _extract_document_block(content) or ""
    if not doc_body:
        # No document block to measure; leave content untouched.
        return content, None

    # Infer language from the existing trailer if the caller didn't
    # pass one explicitly.
    if language_code is None:
        if re.search(r"\bWortbilanz\b", content, re.IGNORECASE):
            language_code = "de"
        else:
            language_code = "en"

    total, per_section = count_document_words(doc_body)
    declared = _parse_declared_wordcount(content)
    telemetry = {
        "declared": declared,
        "actual": total,
        "delta": (declared - total) if declared is not None else None,
        "sections": [{"title": t, "words": w} for t, w in per_section],
        "language_code": language_code,
    }

    replacement = _format_wortbilanz(total, per_section, language_code)

    if _WORTBILANZ_BLOCK.search(content):
        # Replace the first block occurrence in-place
        updated = _WORTBILANZ_BLOCK.sub(
            lambda m: m.group("prefix") + replacement.lstrip("*"),
            content,
            count=1,
        )
    else:
        # No Wortbilanz emitted — append our computed one at the end
        # so the user always sees the real count.
        updated = content.rstrip() + "\n\n" + replacement + "\n"

    return updated, telemetry
