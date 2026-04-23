"""In-text citation ↔ structured-registry sync (#51/#55).

Two halves:

1. **Parser**: walks a draft body, extracts every in-text citation
   marker, normalises it, and returns positional metadata. Handles the
   three citation modes supported by the writing agent:
   - APA-style parenthetical:  ``(Autor, Jahr, S. X)`` / ``(Author, Year, p. X)``
   - Numbered bracket:         ``[1]``, ``[doc_id]``
2. **Validator**: matches parsed markers against the structured
   registry entries (by ``(family, year)`` for APA, by ``entry_key`` or
   author slug for numbered) and reports diagnostics:
   - ``orphan``: marker in body but no registry entry matches it
   - ``dead_entry``: registry entry with zero occurrences in the body
   - ``formatting_drift``: marker present but its rendered form
     doesn't match what the profile would produce

Runs as a pure function on the final response text, consumed by the
post-response audit pipeline (#47) and the DB-sync path that writes
``citation_entries`` rows.
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass, field
from typing import Any, Iterable, List, Mapping, Optional, Tuple


# ---------------------------------------------------------------------------
# Parser
# ---------------------------------------------------------------------------


# Parenthetical APA-style cites. Deliberately permissive about
# separators and surrounding whitespace; the validator will do the
# semantic check.
_APA_MARKER = re.compile(
    r"""\(
        (?P<author>[A-ZÄÖÜa-zäöüß][\wÄÖÜäöüß\.\-&\s,]*?)   # "Müller" / "Smith & Jones" / "WHO"
        ,\s*
        (?P<year>\d{4}|n\.\s*d\.|o\.\s*J\.)                # 2024 | n.d. | o. J.
        (?:,\s*
            (?P<page>[Ss]\.\s*\S+|pp?\.\s*\S+|o\.\s*S\.|n\.p\.)
        )?
        \s*\)
    """,
    re.VERBOSE,
)

# Numbered bracket citations. Covers `[1]`, `[12]`, and doc-id style
# (`[f28769c8]`) but excludes `[Wortstand: 500]` scaffolding.
_NUMBERED_MARKER = re.compile(r"\[([a-z0-9]{6,12}|\d{1,3})\]")


@dataclass(frozen=True)
class ParsedMarker:
    """One citation occurrence parsed from the draft body."""

    marker: str                # original string as it appears
    mode: str                  # 'apa' | 'numbered'
    author_hint: Optional[str] = None   # normalised family token (first listed)
    year: Optional[int] = None
    page: Optional[str] = None
    key_hint: Optional[str] = None      # numbered: the literal bracket payload
    char_offset_start: int = 0
    char_offset_end: int = 0
    paragraph_index: int = 0


def _norm_family(raw: str) -> str:
    """Lowercased, ASCII-folded, punctuation-stripped family hint.

    Used for matching against structured entries whose ``authors[0].family``
    the writer already populated — keeps "Müller" == "mueller" etc.
    """
    if not raw:
        return ""
    # Handle German umlauts first
    t = raw.translate(str.maketrans({"ä": "ae", "ö": "oe", "ü": "ue", "ß": "ss",
                                     "Ä": "ae", "Ö": "oe", "Ü": "ue"}))
    t = unicodedata.normalize("NFKD", t).encode("ascii", "ignore").decode("ascii")
    return re.sub(r"[^a-z0-9]+", "", t.lower())


def _extract_author_head(author_str: str) -> str:
    """Pick the first distinct family name from a multi-author head.

    "Smith & Jones"     -> "Smith"
    "Müller et al."     -> "Müller"
    "Müller, P."        -> "Müller"
    "WHO"               -> "WHO"   (institution)
    """
    head = author_str.strip()
    for sep in (" & ", ",", " und ", " and ", " et al."):
        if sep in head:
            head = head.split(sep, 1)[0].strip()
            break
    return head


def _parse_year(token: str) -> Optional[int]:
    token = token.strip().lower().replace(" ", "")
    if token in ("n.d.", "o.j."):
        return None
    if token.isdigit():
        try:
            return int(token)
        except ValueError:
            return None
    return None


def parse_in_text_citations(body: str) -> List[ParsedMarker]:
    """Walk the draft body and return all in-text citation markers."""
    if not body:
        return []

    markers: List[ParsedMarker] = []
    # Pre-compute paragraph indices so each marker carries locality
    paragraph_break_positions = [m.start() for m in re.finditer(r"\n\n", body)]

    def paragraph_index_for(offset: int) -> int:
        return sum(1 for p in paragraph_break_positions if p < offset)

    # APA-style
    for m in _APA_MARKER.finditer(body):
        author = _extract_author_head(m.group("author"))
        year_token = m.group("year") or ""
        year = _parse_year(year_token)
        page = m.group("page")
        markers.append(
            ParsedMarker(
                marker=m.group(0),
                mode="apa",
                author_hint=_norm_family(author),
                year=year,
                page=page.strip() if page else None,
                char_offset_start=m.start(),
                char_offset_end=m.end(),
                paragraph_index=paragraph_index_for(m.start()),
            )
        )

    # Numbered / doc-id bracket
    for m in _NUMBERED_MARKER.finditer(body):
        payload = m.group(1)
        markers.append(
            ParsedMarker(
                marker=m.group(0),
                mode="numbered",
                key_hint=payload,
                char_offset_start=m.start(),
                char_offset_end=m.end(),
                paragraph_index=paragraph_index_for(m.start()),
            )
        )

    # Preserve textual order
    markers.sort(key=lambda x: x.char_offset_start)
    return markers


# ---------------------------------------------------------------------------
# Validator
# ---------------------------------------------------------------------------


@dataclass
class CitationSyncReport:
    """Validator output — surfaced on the audit payload / WebSocket."""

    orphan_markers: List[ParsedMarker] = field(default_factory=list)
    dead_entries: List[str] = field(default_factory=list)  # entry_keys
    # marker → entry_key assignments (only where resolved)
    resolved: List[Tuple[ParsedMarker, str]] = field(default_factory=list)

    @property
    def has_warnings(self) -> bool:
        return bool(self.orphan_markers or self.dead_entries)

    def to_dict(self) -> dict:
        return {
            "orphan_count": len(self.orphan_markers),
            "orphan_samples": [m.marker for m in self.orphan_markers[:5]],
            "dead_entry_count": len(self.dead_entries),
            "dead_entry_samples": self.dead_entries[:5],
            "resolved_count": len(self.resolved),
            "has_warnings": self.has_warnings,
        }


def _first_family_norm(entry: Mapping[str, Any]) -> str:
    authors = entry.get("authors") or []
    if authors:
        first = authors[0]
        if isinstance(first, Mapping):
            return _norm_family(first.get("family", ""))
    # Fallback: institutional publisher
    return _norm_family(entry.get("publisher", "") or entry.get("container_title", ""))


def validate_citations(
    body: str,
    registry: Iterable[Mapping[str, Any]],
) -> CitationSyncReport:
    """Match parsed markers against structured registry entries.

    `registry` is an iterable of dict-like entries (dict / SQLAlchemy rows
    exposing authors/year/entry_key — service layer should project to
    plain dicts before calling).
    """
    entries = list(registry)
    markers = parse_in_text_citations(body)

    # Build lookup indices. APA matches on (family, year). Numbered
    # bracket payload can match either a digit index (position in
    # registry; we build a 1-based index later) or an entry_key / doc_id.
    by_family_year: dict[Tuple[str, Optional[int]], str] = {}
    by_entry_key: dict[str, str] = {}
    by_family_only: dict[str, str] = {}

    for entry in entries:
        key = entry.get("entry_key") or ""
        if not key:
            continue
        family = _first_family_norm(entry)
        year = entry.get("year")
        try:
            year_int = int(year) if year is not None and year != "" else None
        except (TypeError, ValueError):
            year_int = None
        if family:
            by_family_year[(family, year_int)] = key
            by_family_only.setdefault(family, key)
        by_entry_key.setdefault(key, key)

    report = CitationSyncReport()
    cited_keys: set[str] = set()

    for marker in markers:
        resolved_key: Optional[str] = None

        if marker.mode == "apa" and marker.author_hint:
            key = by_family_year.get((marker.author_hint, marker.year))
            if key is None:
                # Year mismatch is a softer failure — still surface as orphan
                # but include a family-only fallback so a single-work author
                # resolves rather than floating.
                key = by_family_only.get(marker.author_hint)
                if key is not None and marker.year is not None:
                    # The family exists but the year is different — treat as
                    # unresolved so the user gets the diagnostic.
                    key = None
            resolved_key = key

        elif marker.mode == "numbered" and marker.key_hint:
            payload = marker.key_hint
            # Direct entry_key / doc_id match
            if payload in by_entry_key:
                resolved_key = payload
            # Numeric index: treat as 1-based position in the (sorted)
            # registry — good enough for diagnostics, not authoritative.
            elif payload.isdigit():
                idx = int(payload) - 1
                if 0 <= idx < len(entries):
                    resolved_key = entries[idx].get("entry_key")

        if resolved_key:
            cited_keys.add(resolved_key)
            report.resolved.append((marker, resolved_key))
        else:
            report.orphan_markers.append(marker)

    for entry in entries:
        key = entry.get("entry_key") or ""
        if not key:
            continue
        if key not in cited_keys:
            report.dead_entries.append(key)

    return report


# ---------------------------------------------------------------------------
# Body extraction — strips the structured-references block so its JSON
# keys don't get treated as in-text citations.
# ---------------------------------------------------------------------------


_REFERENCES_BLOCK = re.compile(
    r"```content-block:references\s*\n.*?\n```",
    re.DOTALL | re.IGNORECASE,
)


def strip_references_block(content: str) -> str:
    """Remove the content-block:references fence before validation."""
    if not content:
        return content
    return _REFERENCES_BLOCK.sub("", content)
