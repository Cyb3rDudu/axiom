"""Reference → portfolio source_record adapter (#61/#62).

Bridges the writing-mode structured registry (Epic #51) into the shape
LiteraturePortfolioAgent already expects — no agent changes needed.

The mission-side `_build_source_records` reads from Notes; here we
reproduce its output dict shape starting from `draft_references` rows,
their `citation_entries` occurrences, and the draft body.

Pure functions: no DB, no LLM. All DB access lives in the caller
(WritingPortfolioManager, #65).
"""

from __future__ import annotations

import re
from types import SimpleNamespace
from typing import Any, Dict, Iterable, List, Mapping, Optional

from database import models
from ai_researcher.agentic_layer.services import source_quality


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

CONTEXT_RADIUS = 180  # ±chars around each cite, matches mission-side windows
SECTION_SLUG_FALLBACK = "body"


_HEADING_RE = re.compile(r"^(#{1,3})\s+(.+?)\s*$", re.MULTILINE)
_SLUG_DROP = re.compile(r"[^a-z0-9]+")
_GERMAN_UMLAUTS = str.maketrans({
    "ä": "ae", "Ä": "ae",
    "ö": "oe", "Ö": "oe",
    "ü": "ue", "Ü": "ue",
    "ß": "ss",
})


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _slugify_heading(heading: str) -> str:
    import unicodedata

    transliterated = (heading or "").translate(_GERMAN_UMLAUTS)
    ascii_ = unicodedata.normalize("NFKD", transliterated).encode("ascii", "ignore").decode("ascii")
    slug = _SLUG_DROP.sub("-", ascii_.lower()).strip("-")
    return slug or SECTION_SLUG_FALLBACK


def _authors_to_apa_string(authors: Optional[Iterable[Mapping[str, Any]]]) -> str:
    """Join [{family, given}] into an APA-ish "Müller, P., & Schmidt, A." string.

    source_quality.extract_apa_citation expects `authors` as a plain
    string — we give it one so its output stays consistent with the
    mission-side path.
    """
    if not authors:
        return ""
    parts: List[str] = []
    for a in authors:
        if not isinstance(a, Mapping):
            continue
        family = (a.get("family") or "").strip()
        given = (a.get("given") or "").strip()
        if not family:
            continue
        given_initials = " ".join(f"{t[0].upper()}." for t in given.split() if t)
        parts.append(f"{family}, {given_initials}".rstrip(", "))
    if not parts:
        return ""
    if len(parts) == 1:
        return parts[0]
    if len(parts) == 2:
        return f"{parts[0]} & {parts[1]}"
    head = ", ".join(parts[:-1])
    return f"{head}, & {parts[-1]}"


def _fake_note(ref: models.Reference) -> SimpleNamespace:
    """Build a note-lookalike that satisfies the source_quality helpers.

    `compute_quality_signals`, `discovery_tool_label`, `extract_apa_citation`
    all read from `.source_type` + `.source_metadata`. We reproduce both
    from the structured Reference columns. Extra fields the writer put
    on the ref (e.g. isbn in a future version) carry through via the
    generic dict projection.
    """
    source_type = "document" if ref.document_id else "web"

    meta: Dict[str, Any] = {
        "authors": _authors_to_apa_string(ref.authors),
        "publication_year": ref.year,
        "title": ref.title,
        "publisher": ref.publisher,
        "journal": ref.container_title,
        "pages": ref.pages,
        "doi": ref.doi,
        "url": ref.url or ref.web_url,
        "original_filename": None,
    }
    # Drop Nones so _metadata_dict's isinstance checks don't trip
    meta = {k: v for k, v in meta.items() if v not in (None, "")}

    return SimpleNamespace(source_type=source_type, source_metadata=meta)


def _nearest_heading_slug(body: str, offset: int) -> str:
    """Slug of the closest preceding Markdown heading. Fallback = 'body'."""
    if not body:
        return SECTION_SLUG_FALLBACK
    # Walk headings in order; keep the last one whose start < offset
    last_match = None
    for m in _HEADING_RE.finditer(body):
        if m.start() < offset:
            last_match = m
        else:
            break
    if last_match is None:
        return SECTION_SLUG_FALLBACK
    return _slugify_heading(last_match.group(2))


def _context_window(body: str, start: int, end: int, radius: int = CONTEXT_RADIUS) -> str:
    if not body:
        return ""
    lo = max(0, start - radius)
    hi = min(len(body), end + radius)
    # Collapse whitespace runs for readable snippets
    return re.sub(r"\s+", " ", body[lo:hi]).strip()


def _writing_mission_settings_like(session_settings: Optional[Mapping[str, Any]]) -> Dict[str, Any]:
    """Shape session settings into the `mission_settings` dict the helper expects.

    `discovery_tool_label` reads
    `mission_settings["comprehensive_settings"]["search_provider"]`. We
    project the writing session's effective search provider (or the
    user's default) under that key so the helper stays unforked.
    """
    if not session_settings:
        return {}
    provider = None
    if isinstance(session_settings, Mapping):
        ws = session_settings.get("writing_settings")
        if isinstance(ws, Mapping):
            provider = ws.get("search_provider")
        if not provider:
            provider = session_settings.get("search_provider")
    if not provider:
        return {}
    return {"comprehensive_settings": {"search_provider": provider}}


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def reference_to_source_record(
    ref: models.Reference,
    citation_entries: Iterable[models.CitationEntry],
    draft_body: str,
    session_settings: Optional[Mapping[str, Any]] = None,
) -> Dict[str, Any]:
    """Project one Reference + its in-text occurrences into a portfolio source_record.

    Result shape matches mission-side `_build_source_records` output:

        {
            "source_id": str,              # entry_key
            "apa_citation": str,
            "discovery_tool": str,
            "quality_signals": dict,       # QualitySignals.model_dump()
            "scientific_tier": str,        # "A" | "B" | "C" | "D"
            "sections_used_in": List[str], # heading slugs, deduped in order
            "section_context_snippets": List[{"section_id", "snippet"}],
        }
    """
    note_like = _fake_note(ref)
    mission_settings_like = _writing_mission_settings_like(session_settings)

    # Use the ref's pre-rendered citation_text when present; fall back to
    # the helper so the output is never empty even on partial refs.
    apa_citation = (ref.citation_text or "").strip() or source_quality.extract_apa_citation(note_like)

    discovery_tool = source_quality.discovery_tool_label(note_like, mission_settings_like)

    quality_signals = source_quality.compute_quality_signals(note_like)
    scientific_tier = source_quality.assign_scientific_tier(quality_signals)

    sections_seen: List[str] = []
    snippets: List[Dict[str, str]] = []
    occurrences = [ce for ce in citation_entries if ce is not None]
    # Process in the order they appear in the body so first-appearance
    # semantics are preserved.
    occurrences.sort(key=lambda ce: ce.char_offset_start or 0)
    for ce in occurrences:
        start = int(ce.char_offset_start or 0)
        end = int(ce.char_offset_end or start)
        section_id = _nearest_heading_slug(draft_body, start)
        if section_id not in sections_seen:
            sections_seen.append(section_id)
        snippet = _context_window(draft_body, start, end)
        if snippet:
            snippets.append({"section_id": section_id, "snippet": snippet})

    return {
        "source_id": ref.entry_key or ref.id,
        "apa_citation": apa_citation,
        "discovery_tool": discovery_tool,
        "quality_signals": quality_signals.model_dump(),
        "scientific_tier": scientific_tier,
        "sections_used_in": sections_seen,
        "section_context_snippets": snippets,
    }
