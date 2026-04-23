"""Deterministic rendering for structured bibliography entries (#51/#57).

The structured registry (draft_references with authors/year/title/…) is
authoritative; at display/export time this module projects it into the
Markdown form dictated by the active CitationProfile.

Contract:
- render_entry(entry, profile_id) -> str
- render_bibliography(entries, profile_id) -> str (full Literaturverzeichnis)

`entry` is a dict with the same keys as StructuredReferenceCreate.
Unknown fields are ignored so the writer can send extras without
breaking rendering.
"""

from __future__ import annotations

from datetime import date, datetime
from typing import Any, Dict, Iterable, List, Mapping, Optional

_KMU_HEADING = "## Literaturverzeichnis"
_APA7_HEADING = "## References"
_NUMBERED_HEADING = "## References"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _authors_list(entry: Mapping[str, Any]) -> List[Dict[str, str]]:
    raw = entry.get("authors") or []
    out: List[Dict[str, str]] = []
    for a in raw:
        if isinstance(a, Mapping):
            out.append({"family": (a.get("family") or "").strip(), "given": (a.get("given") or "").strip()})
    return [a for a in out if a["family"]]


def _format_given_initials(given: str) -> str:
    """\"Peter Ulrich\" -> \"P. U.\", \"A.\" -> \"A.\", \"\" -> \"\"."""
    if not given:
        return ""
    tokens = [t for t in given.replace(".", " ").split() if t]
    return " ".join(f"{t[0].upper()}." for t in tokens)


def _format_authors_bibliography(authors: List[Dict[str, str]], *, apa_amp: bool = True) -> str:
    """\"Müller, P., & Schmidt, A.\" style for APA / KMU bibliography."""
    if not authors:
        return "o. A."  # "ohne Autor:in" — KMU convention; apa7_en overridden below
    parts: List[str] = []
    for a in authors:
        family = a["family"]
        given = _format_given_initials(a["given"])
        parts.append(f"{family}, {given}".strip().rstrip(",") if given else family)
    if len(parts) == 1:
        return parts[0]
    if len(parts) == 2:
        sep = ", & " if apa_amp else " & "
        return sep.join(parts)
    head = ", ".join(parts[:-1])
    return f"{head}, & {parts[-1]}" if apa_amp else f"{head} & {parts[-1]}"


def _format_authors_bibliography_en(authors: List[Dict[str, str]]) -> str:
    if not authors:
        return "Anon."
    return _format_authors_bibliography(authors, apa_amp=True)


def _year_str(entry: Mapping[str, Any], *, german: bool) -> str:
    year = entry.get("year")
    if year is None or year == "":
        return "o. J." if german else "n.d."
    return str(year)


def _accessed_str(entry: Mapping[str, Any], *, german: bool) -> Optional[str]:
    raw = entry.get("accessed_at")
    if raw in (None, ""):
        return None
    if isinstance(raw, (datetime, date)):
        dt = raw
    else:
        try:
            dt = datetime.fromisoformat(str(raw).replace("Z", "+00:00"))
        except ValueError:
            return None
    if german:
        return dt.strftime("%d.%m.%Y")
    return dt.strftime("%B %d, %Y")


def _sort_apa(entries: Iterable[Mapping[str, Any]]) -> List[Mapping[str, Any]]:
    def key(e: Mapping[str, Any]):
        authors = _authors_list(e)
        first_family = authors[0]["family"].lower() if authors else "zzz"
        year = int(e.get("year") or 0)
        return (first_family, year)

    return sorted(entries, key=key)


# ---------------------------------------------------------------------------
# Per-profile entry renderers
# ---------------------------------------------------------------------------


def _render_kmu_apa6(entry: Mapping[str, Any]) -> str:
    authors = _authors_list(entry)
    author_str = _format_authors_bibliography(authors, apa_amp=True)
    year = _year_str(entry, german=True)

    title = (entry.get("title") or "").strip() or "o. T."
    container = (entry.get("container_title") or "").strip()
    publisher = (entry.get("publisher") or "").strip()
    pages = (entry.get("pages") or "").strip()
    url = (entry.get("url") or entry.get("web_url") or "").strip()
    doi = (entry.get("doi") or "").strip()
    accessed = _accessed_str(entry, german=True)
    ref_type = (entry.get("reference_type") or "").lower()

    parts: List[str] = [f"{author_str} ({year})."]

    # Journal article: author (year). article title. *journal, vol*(issue), pages.
    if container and ref_type != "web" and not url:
        parts.append(f"{title}.")
        journal = f"*{container}*"
        if pages:
            journal += f", {pages}"
        parts.append(f"{journal}.")
    # Book / Report: author (year). *title*. publisher.
    elif publisher:
        parts.append(f"*{title}*.")
        parts.append(f"{publisher}.")
    # Web source with container (e.g. site name): author (year). *title*. Site. URL
    elif container and url:
        parts.append(f"*{title}*.")
        parts.append(f"{container}.")
    else:
        parts.append(f"*{title}*.")

    if doi:
        parts.append(f"https://doi.org/{doi}")
    elif url:
        if accessed:
            parts.append(f"Abgerufen am {accessed}, von {url}")
        else:
            parts.append(url)

    return " ".join(parts).strip()


def _render_apa7_en(entry: Mapping[str, Any]) -> str:
    authors = _authors_list(entry)
    author_str = _format_authors_bibliography_en(authors)
    year = _year_str(entry, german=False)

    title = (entry.get("title") or "").strip() or "Untitled"
    container = (entry.get("container_title") or "").strip()
    publisher = (entry.get("publisher") or "").strip()
    pages = (entry.get("pages") or "").strip()
    url = (entry.get("url") or entry.get("web_url") or "").strip()
    doi = (entry.get("doi") or "").strip()
    accessed = _accessed_str(entry, german=False)
    ref_type = (entry.get("reference_type") or "").lower()

    parts: List[str] = [f"{author_str} ({year})."]

    if container and ref_type != "web" and not url:
        parts.append(f"{title}.")
        journal = f"*{container}*"
        if pages:
            journal += f", {pages}"
        parts.append(f"{journal}.")
    elif publisher:
        parts.append(f"*{title}*.")
        parts.append(f"{publisher}.")
    elif container and url:
        parts.append(f"*{title}*.")
        parts.append(f"{container}.")
    else:
        parts.append(f"*{title}*.")

    if doi:
        parts.append(f"https://doi.org/{doi}")
    elif url:
        if accessed:
            parts.append(f"Retrieved {accessed}, from {url}")
        else:
            parts.append(url)

    return " ".join(parts).strip()


def _render_numbered(entry: Mapping[str, Any], *, index: Optional[int] = None) -> str:
    """Numbered format: [N] Author, Year. Title. Source."""
    authors = _authors_list(entry)
    author_str = _format_authors_bibliography_en(authors) if authors else (entry.get("publisher") or "Anon.")
    year = entry.get("year") or "n.d."
    title = (entry.get("title") or "").strip() or "Untitled"
    container = (entry.get("container_title") or "").strip()
    publisher = (entry.get("publisher") or "").strip()
    url = (entry.get("url") or entry.get("web_url") or "").strip()
    doi = (entry.get("doi") or "").strip()

    segments = [f"{author_str}, {year}.", f"{title}."]
    if container:
        segments.append(f"{container}.")
    elif publisher:
        segments.append(f"{publisher}.")
    if doi:
        segments.append(f"https://doi.org/{doi}")
    elif url:
        segments.append(url)

    body = " ".join(segments).strip()
    if index is not None:
        return f"[{index}] {body}"
    return body


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def render_entry(
    entry: Mapping[str, Any],
    profile_id: str = "numbered",
    *,
    index: Optional[int] = None,
) -> str:
    """Render one structured entry in the chosen citation style."""
    pid = (profile_id or "numbered").lower()
    if pid == "kmu_apa6":
        return _render_kmu_apa6(entry)
    if pid == "apa7_en":
        return _render_apa7_en(entry)
    return _render_numbered(entry, index=index)


def render_bibliography(
    entries: Iterable[Mapping[str, Any]],
    profile_id: str = "numbered",
    *,
    include_heading: bool = True,
) -> str:
    """Render the full Literaturverzeichnis section.

    Output is Markdown, suitable for appending to a draft body or the
    DOCX export pipeline. Empty entry list returns the empty string.
    """
    items = [e for e in entries if e]
    if not items:
        return ""

    pid = (profile_id or "numbered").lower()
    lines: List[str] = []
    if include_heading:
        if pid == "kmu_apa6":
            lines.append(_KMU_HEADING)
        elif pid == "apa7_en":
            lines.append(_APA7_HEADING)
        else:
            lines.append(_NUMBERED_HEADING)
        lines.append("")

    if pid == "numbered":
        # Preserve input order so callers can pass the first-appearance
        # sort done during the citation-sync pass.
        for i, entry in enumerate(items, start=1):
            lines.append(_render_numbered(entry, index=i))
    else:
        for entry in _sort_apa(items):
            rendered = _render_kmu_apa6(entry) if pid == "kmu_apa6" else _render_apa7_en(entry)
            lines.append(f"- {rendered}")

    return "\n".join(lines).strip() + "\n"
