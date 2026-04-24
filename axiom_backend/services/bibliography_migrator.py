"""Inline-Markdown → structured-reference migration (#51/#54).

Per-draft opt-in: the user triggers the migration via the Bibliography
panel, we parse the Literaturverzeichnis / References section out of
the draft body, emit structured entries, and let the user review
before commit.

Parser is deliberately conservative: unknown-format entries are
surfaced with their original literal for manual entry, never dropped
silently. Profile-aware splitting (APA vs numbered) with a generic
line-based fallback.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

from services.structured_bibliography import slugify_entry_key


_BIB_SECTION_HEAD = re.compile(
    r"^\s*#{1,3}\s*(?:Literaturverzeichnis|References|Bibliography|Bibliografie)\s*$",
    re.MULTILINE,
)

_NUMBERED_ENTRY = re.compile(
    r"^\s*(?:\[(?P<num>\d+)\]|(?P<num2>\d{1,3})\.)\s+(?P<body>.+?)\s*$",
    re.MULTILINE,
)

_APA_BULLET = re.compile(r"^\s*[-\*]\s+(?P<body>.+?)\s*$", re.MULTILINE)

# APA opener: "Müller, P. (2024). ..." or "Destatis. (2024). ..."
_APA_OPENER = re.compile(
    r"""
    ^
    (?P<author>[^\(\)]+?)            # everything up to the (year)
    \s*\(
        (?P<year>\d{4}|n\.\s*d\.|o\.\s*J\.)
    \)\.\s*
    (?P<rest>.*)
    $
    """,
    re.VERBOSE | re.DOTALL,
)

_URL_RE = re.compile(r"(https?://[^\s\)]+)")
_DOI_RE = re.compile(r"(?:doi:|https?://doi\.org/)(\S+)", re.IGNORECASE)


# ---------------------------------------------------------------------------
# Types
# ---------------------------------------------------------------------------


@dataclass
class MigratedEntry:
    """One parsed registry candidate + its source literal, for review."""

    entry_key: str
    source_markdown: str
    authors: List[Dict[str, str]] = field(default_factory=list)
    year: Optional[int] = None
    title: Optional[str] = None
    container_title: Optional[str] = None
    publisher: Optional[str] = None
    url: Optional[str] = None
    doi: Optional[str] = None
    reference_type: str = "web"
    confidence: str = "low"  # 'high' if APA/numbered pattern matched; 'low' for fallback

    def to_dict(self) -> Dict[str, Any]:
        return {
            "entry_key": self.entry_key,
            "source_markdown": self.source_markdown,
            "authors": self.authors,
            "year": self.year,
            "title": self.title,
            "container_title": self.container_title,
            "publisher": self.publisher,
            "url": self.url,
            "doi": self.doi,
            "reference_type": self.reference_type,
            "confidence": self.confidence,
        }


@dataclass
class MigrationPreview:
    entries: List[MigratedEntry] = field(default_factory=list)
    unparsable: List[str] = field(default_factory=list)  # literal Markdown lines

    def to_dict(self) -> Dict[str, Any]:
        return {
            "entries": [e.to_dict() for e in self.entries],
            "unparsable": self.unparsable,
            "parsed_count": len(self.entries),
            "unparsable_count": len(self.unparsable),
        }


# ---------------------------------------------------------------------------
# Section extraction
# ---------------------------------------------------------------------------


def extract_bibliography_section(markdown: str) -> Optional[str]:
    """Return the text of the Literaturverzeichnis section, or None.

    Looks for a level-1-to-3 heading matching a bibliography title and
    returns everything from the end of the heading to the next same-or-
    higher-level heading (or end of document).
    """
    if not markdown:
        return None
    match = _BIB_SECTION_HEAD.search(markdown)
    if match is None:
        return None
    start = match.end()

    # Find the next heading at the same or higher level
    tail = markdown[start:]
    next_head = re.search(r"\n#{1,3}\s", tail)
    if next_head is None:
        return tail.strip() or None
    return tail[: next_head.start()].strip() or None


# ---------------------------------------------------------------------------
# Entry parsing
# ---------------------------------------------------------------------------


def _parse_authors_apa_head(head: str) -> List[Dict[str, str]]:
    """APA author head: "Müller, P., & Schmidt, A." → [{family, given}, …]."""
    head = head.strip().rstrip(".").strip()
    if not head:
        return []
    # Split on ", &" / " and " / ";"
    normalised = re.sub(r"\s*&\s*|\s+and\s+|;\s*", "||", head)
    chunks = [c.strip(" ,") for c in normalised.split("||") if c.strip(" ,")]

    authors: List[Dict[str, str]] = []
    for chunk in chunks:
        if "," in chunk:
            family, _, given = chunk.partition(",")
            authors.append({"family": family.strip(), "given": given.strip()})
        else:
            # Institutional / single-token author
            authors.append({"family": chunk, "given": ""})
    return authors


def _classify_reference_type(rest: str, url: Optional[str]) -> str:
    """Heuristic: URL present + italic title → web; otherwise document."""
    if url:
        return "web"
    return "document"


def _parse_apa_line(body: str) -> Optional[MigratedEntry]:
    """Parse one APA-style bibliography entry. Returns None on miss."""
    body = body.strip()
    match = _APA_OPENER.match(body)
    if match is None:
        return None

    author_head = match.group("author").strip().rstrip(",")
    year_token = match.group("year").strip().lower().replace(" ", "")
    rest = match.group("rest").strip()

    year: Optional[int] = None
    if year_token.isdigit():
        try:
            year = int(year_token)
        except ValueError:
            year = None

    # The remainder: usually "Title. Publisher." or
    # "Article title. *Journal, Vol*(Issue), pp. URL"
    url_match = _URL_RE.search(rest)
    url = url_match.group(1) if url_match else None

    doi_match = _DOI_RE.search(rest)
    doi = doi_match.group(1) if doi_match else None

    # Strip URL / DOI for further parsing
    without_url = _URL_RE.sub("", rest).strip()
    # Drop "Retrieved …, from" / "Abgerufen am …, von" trailing phrases
    without_url = re.sub(
        r"(Retrieved|Abgerufen am)[^\.]*\.?",
        "",
        without_url,
        flags=re.IGNORECASE,
    ).strip()

    title = None
    container_title = None
    publisher = None

    # Prefer an italicised segment as the title (e.g. *China in der Weltwirtschaft*).
    italic_match = re.match(r"\s*\*([^*]+)\*\s*(.*)$", without_url, re.DOTALL)
    if italic_match:
        title = italic_match.group(1).strip().rstrip(".")
        trailing = italic_match.group(2).strip()
        # Strip leading "(Edition)" / "(2. Aufl.)" parenthetical
        trailing = re.sub(r"^\([^)]*\)\s*\.?\s*", "", trailing).lstrip(".").strip()
        if trailing:
            # Take everything up to the next period as publisher/container
            publisher_or_container = trailing.split(".", 1)[0].strip()
            publisher = publisher_or_container.strip("*") or None
    else:
        # Fallback (#75): split on ". " but skip both (a) single
        # uppercase initials like "A." and (b) multi-initial runs like
        # "A. B." / "P. U.". The old lookbehind only covered case (a)
        # and fragmented titles like "Müller, P. U. (2024). Title …"
        # into ["…P. U", "Title …"]. We now pre-replace the initial
        # runs with sentinel tokens, split, then restore.
        protected = re.sub(
            r"\b([A-Z])\.(?=\s)",
            lambda m: f"{m.group(1)}\x00",
            without_url,
        )
        segments = [s.replace("\x00", ".").strip() for s in re.split(r"\.\s+", protected) if s.strip()]
        if segments:
            title = segments[0].rstrip(".").strip(" *_")
        if len(segments) >= 2:
            second = segments[1].rstrip(".").strip()
            if second.startswith("*") and second.endswith("*"):
                container_title = second.strip("*")
            else:
                publisher = second.strip("*")

    # Journal/container detection: look for an italic *Journal, Vol*(Issue)
    # pattern anywhere in the rest
    if container_title is None:
        journal_match = re.search(r"\*([^*]+?),?\s*(\d{1,4})?\*", without_url)
        if journal_match and title and journal_match.group(0).strip("*") != title:
            container_title = journal_match.group(0).strip("*").strip()
            # Fallthrough: this catches "*Journal of International Economics, 112*"

    authors = _parse_authors_apa_head(author_head)

    entry_key_hint = slugify_entry_key(
        authors[0]["family"] if authors else "",
        str(year) if year else "",
        (title or "")[:30],
    )

    return MigratedEntry(
        entry_key=entry_key_hint,
        source_markdown=body,
        authors=authors,
        year=year,
        title=title,
        container_title=container_title,
        publisher=publisher,
        url=url,
        doi=doi,
        reference_type=_classify_reference_type(rest, url),
        confidence="high",
    )


def _parse_numbered_line(body: str, index: int) -> Optional[MigratedEntry]:
    """Parse `[N] Author, Year. Title. Source.` into structured fields."""
    body = body.strip()
    # Try the APA opener first — many numbered bibliographies use APA-ish
    # formatting inside the bracket
    apa = _parse_apa_line(body)
    if apa is not None:
        return apa

    # Fall back: `Author, Year. Title. Source.`
    match = re.match(
        r"^(?P<author>[^,]+?),\s*(?P<year>\d{4}|n\.d\.)\.\s*(?P<rest>.+)$",
        body,
        re.IGNORECASE,
    )
    if match is None:
        return None
    author = match.group("author")
    year_raw = match.group("year")
    year = int(year_raw) if year_raw.isdigit() else None
    rest = match.group("rest")
    url_match = _URL_RE.search(rest)
    url = url_match.group(1) if url_match else None
    without_url = _URL_RE.sub("", rest).strip()
    # Title = first segment before period
    title = without_url.split(".", 1)[0].strip(" *_") or None

    authors = [{"family": author.strip(), "given": ""}]
    entry_key = slugify_entry_key(author.strip(), str(year) if year else "", (title or "")[:30])

    return MigratedEntry(
        entry_key=entry_key,
        source_markdown=body,
        authors=authors,
        year=year,
        title=title,
        url=url,
        reference_type=_classify_reference_type(rest, url),
        confidence="high",
    )


# ---------------------------------------------------------------------------
# Top-level entry point
# ---------------------------------------------------------------------------


def migrate_markdown_bibliography(
    markdown: str,
    *,
    profile_hint: Optional[str] = None,
) -> MigrationPreview:
    """Parse the Literaturverzeichnis out of `markdown` into structured entries.

    `profile_hint` biases the parser toward APA / numbered ordering.
    """
    preview = MigrationPreview()
    section = extract_bibliography_section(markdown)
    if not section:
        return preview

    # Try numbered entries first if the section has `[1]` / `1.` markers
    numbered_candidates = list(_NUMBERED_ENTRY.finditer(section))
    bullet_candidates = list(_APA_BULLET.finditer(section))

    parsed: List[MigratedEntry] = []
    unparsable: List[str] = []
    seen_keys: set[str] = set()

    def _add(entry: Optional[MigratedEntry], original: str):
        if entry is None:
            unparsable.append(original.strip())
            return
        # Ensure entry_key unique within this preview
        base = entry.entry_key or "ref"
        candidate = base
        n = 2
        while candidate in seen_keys:
            candidate = f"{base}-{n}"
            n += 1
        seen_keys.add(candidate)
        if candidate != entry.entry_key:
            entry.entry_key = candidate
        parsed.append(entry)

    if numbered_candidates and (profile_hint == "numbered" or len(numbered_candidates) >= len(bullet_candidates)):
        for i, match in enumerate(numbered_candidates, start=1):
            body = match.group("body").strip()
            _add(_parse_numbered_line(body, i), body)
    elif bullet_candidates:
        for match in bullet_candidates:
            body = match.group("body").strip()
            _add(_parse_apa_line(body), body)
    else:
        # Fallback: treat each non-empty line as an entry candidate
        for line in section.splitlines():
            line = line.strip()
            if not line:
                continue
            # Skip trailing "Abgerufen am …" continuation lines
            _add(_parse_apa_line(line), line)

    preview.entries = parsed
    preview.unparsable = unparsable
    return preview
