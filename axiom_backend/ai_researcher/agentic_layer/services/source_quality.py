"""
Compute quality signals for a cited source.

Pure function layer over publisher_tiers + whatever metadata the existing
enrichment pipeline has already attached to the Note. No network calls —
signals come only from what axiom already knows about the source.
"""

from __future__ import annotations

import datetime as _dt
import re
from typing import Any, Dict, Optional
from urllib.parse import urlparse

from ai_researcher.agentic_layer.schemas.notes import Note, SourceMetadata
from ai_researcher.agentic_layer.schemas.portfolio import (
    PublicationType,
    QualitySignals,
    ScientificTier,
)
from ai_researcher.agentic_layer.services.publisher_tiers import classify_tier


_PEER_REVIEWED_PUBTYPES = {
    "peer_reviewed_journal",
    "monograph_scientific_publisher",
    "conference_proceedings",
}


def _extract_domain(url: Optional[str]) -> Optional[str]:
    if not url:
        return None
    try:
        host = urlparse(url).hostname
        if host and host.startswith("www."):
            host = host[4:]
        return host
    except Exception:
        return None


def _metadata_dict(metadata: Any) -> Dict[str, Any]:
    if metadata is None:
        return {}
    if isinstance(metadata, dict):
        return metadata
    # Pydantic model — prefer model_dump, fall back to dict()
    for method in ("model_dump", "dict"):
        fn = getattr(metadata, method, None)
        if callable(fn):
            try:
                return fn()
            except Exception:
                continue
    return {}


def _infer_publication_type(
    source_type: str,
    meta: Dict[str, Any],
    publisher_tier: str,
) -> PublicationType:
    journal = (meta.get("journal") or "").lower() if isinstance(meta.get("journal"), str) else ""
    publisher = (meta.get("publisher") or "").lower() if isinstance(meta.get("publisher"), str) else ""
    url = meta.get("url") or ""
    doi = meta.get("doi")
    isbn = meta.get("isbn")

    if source_type == "internal":
        return "unknown"

    # Local-RAG documents that end up on tier A/B without an explicit journal
    # are academic monographs / edited volumes. This covers the typical case
    # of ingesting a VWL textbook into the user's library.
    if source_type == "document" and publisher_tier in ("A", "B") and not journal:
        if isbn:
            return "monograph_scientific_publisher"
        # Even without ISBN, a document-type source on a scientific publisher
        # is very likely a book or book chapter.
        return "monograph_scientific_publisher"

    # Explicit signals first. Any non-empty `journal` field on a tier-A or
    # tier-B publisher is a peer-reviewed journal article for our purposes
    # — we also accept common title tokens for edge cases where publisher
    # metadata is missing.
    journal_title_hint = any(
        tok in journal for tok in ("journal", "review", "quarterly", "letters", "transactions", "frontiers in")
    )
    if journal and publisher_tier in ("A", "B"):
        return "peer_reviewed_journal"
    if journal and doi and journal_title_hint:
        return "peer_reviewed_journal"
    if journal and journal_title_hint:
        return "peer_reviewed_journal" if doi else "working_paper"

    if "arxiv" in url or "biorxiv" in url or "preprints.org" in url or "ssrn" in url:
        return "preprint" if "ssrn" not in url else "working_paper"

    if isbn or (publisher_tier == "A" and ("springer" in publisher or "wiley" in publisher or "routledge" in publisher) and not journal):
        return "monograph_scientific_publisher"

    if publisher_tier == "B":
        return "working_paper" if "working paper" in (meta.get("title") or "").lower() else "industry_report"

    if publisher_tier == "D":
        host = _extract_domain(url) or ""
        if any(tok in host for tok in ("mckinsey", "bcg", "deloitte", "pwc", "kpmg", "ey.com", "gartner", "forrester")):
            return "industry_report"
        if any(tok in host for tok in ("ft.com", "economist", "bloomberg", "reuters", "nzz.ch", "faz.net", "handelsblatt")):
            return "news_article"
        return "whitepaper"

    if source_type == "web":
        return "web_page"

    return "unknown"


def _peer_reviewed_from_type(pubtype: PublicationType) -> Optional[bool]:
    if pubtype in _PEER_REVIEWED_PUBTYPES:
        return True
    if pubtype in {"preprint", "working_paper", "industry_report", "whitepaper", "news_article", "blog", "web_page"}:
        return False
    return None


def _recency_years(meta: Dict[str, Any], now: Optional[_dt.date] = None) -> Optional[int]:
    now = now or _dt.date.today()
    year = meta.get("publication_year") or meta.get("year")
    if year is None:
        return None
    try:
        y = int(str(year)[:4])
    except (TypeError, ValueError):
        return None
    if y < 1500 or y > now.year + 1:
        return None
    return now.year - y


def _bias_flags(source_type: str, meta: Dict[str, Any], tier: str) -> list[str]:
    flags: list[str] = []
    url = meta.get("url") or ""
    host = _extract_domain(url) or ""
    if tier == "D":
        flags.append("practice_source_possible_interest_bias")
    if tier == "blacklist":
        flags.append("disallowed_as_primary_source")
    if any(tok in host for tok in ("mckinsey", "bcg", "deloitte", "pwc", "kpmg", "ey.com")):
        flags.append("consultancy_bias")
    if any(tok in host for tok in ("microsoft.com", "google.com", "meta.com", "amazon.com", "apple.com")):
        flags.append("corporate_bias")
    return flags


def compute_quality_signals(note: Note, now: Optional[_dt.date] = None) -> QualitySignals:
    """Compute the quality signals for a single note's source."""
    meta = _metadata_dict(note.source_metadata)
    url = meta.get("url") if isinstance(meta.get("url"), str) else None
    publisher = meta.get("publisher") if isinstance(meta.get("publisher"), str) else None
    journal = meta.get("journal") if isinstance(meta.get("journal"), str) else None
    filename = meta.get("original_filename") or meta.get("filename") or ""

    # Also look at filename for publisher hints — many ingested books encode
    # the publisher in the filename (e.g. "Mankiw2018_Book_…").
    tier = classify_tier(publisher, journal, url, _extract_domain(url), meta.get("doi"), filename)

    # Local-RAG document fallback: when the user has ingested a document into
    # their library and no external signal matches a known tier, default to
    # Tier A. The assumption is that scientific-library ingestion is curated;
    # this keeps Mankiw / Bofinger / Heine books etc. from landing in Tier C
    # (unknown → conservative "C") just because their metadata block is thin.
    if note.source_type == "document" and tier == "unknown":
        tier = "A"

    pubtype = _infer_publication_type(note.source_type, meta, tier)
    peer = _peer_reviewed_from_type(pubtype)

    return QualitySignals(
        publication_type=pubtype,
        peer_reviewed=peer,
        publisher_tier=tier,
        journal_name=journal,
        publisher=publisher,
        has_doi=bool(meta.get("doi")),
        has_isbn=bool(meta.get("isbn")),
        recency_years=_recency_years(meta, now=now),
        author_credentials_note=None,  # Filled by agent from prompt context if OpenAlex data present.
        bias_flags=_bias_flags(note.source_type, meta, tier),
    )


def assign_scientific_tier(signals: QualitySignals) -> ScientificTier:
    """Map QualitySignals to a scientific-vs-practitioner tier.

    A/B = peer-reviewed / scientific; C/D = grey / practitioner.
    Consumed by portfolio compliance rubrics that enforce a minimum
    scientific share (for example the KMU APA 7 ≥50 % rule, but the
    tiers themselves are style-agnostic)."""
    if signals.publication_type == "peer_reviewed_journal":
        return "A"
    if signals.publisher_tier == "blacklist":
        return "D"
    if signals.publisher_tier == "A":
        if signals.peer_reviewed is False:
            return "B"
        return "A"
    if signals.publisher_tier == "B":
        return "B"
    if signals.publisher_tier == "C":
        return "C"
    if signals.publisher_tier == "D":
        return "D"
    # unknown — conservative
    return "C"


def discovery_tool_label(
    note: Note,
    mission_settings: Optional[Dict[str, Any]] = None,
) -> str:
    """Best-effort label for the Recherchetool column.

    Without a per-note retrieval provenance field (follow-up), we map
    source_type plus mission settings to a reasonable string. Users can edit.
    """
    meta = _metadata_dict(note.source_metadata)
    url = meta.get("url") if isinstance(meta.get("url"), str) else None
    host = (_extract_domain(url) or "").lower()

    if note.source_type == "document":
        return "Axiom Local Library (RAG)"
    if note.source_type == "internal":
        return "Agent Synthesis"
    if note.source_type == "web":
        if "scholar.google" in host:
            return "Google Scholar"
        if "crossref" in host:
            return "CrossRef"
        if "openalex" in host:
            return "OpenAlex"
        if "arxiv.org" in host:
            return "arXiv"
        if "doi.org" in host or meta.get("doi"):
            return "CrossRef / DOI"
        if "semanticscholar" in host:
            return "Semantic Scholar"
        if mission_settings:
            provider = (
                mission_settings.get("comprehensive_settings", {}).get("search_provider")
                if isinstance(mission_settings, dict)
                else None
            )
            if provider:
                return f"Web Search ({provider})"
        return "Web Search"
    return "Unknown"


def extract_apa_citation(note: Note) -> str:
    """Render a best-effort APA 7 citation from the note's source metadata.
    Deliberately conservative — the agent's prompt includes the raw metadata
    fields, so it can also refine this string."""
    meta = _metadata_dict(note.source_metadata)
    authors = meta.get("authors") or ""
    year = meta.get("publication_year") or meta.get("year") or "n.d."
    title = (meta.get("title") or meta.get("original_filename") or "Untitled").strip()
    publisher = meta.get("publisher")
    journal = meta.get("journal")
    url = meta.get("url")
    doi = meta.get("doi")

    parts: list[str] = []
    if authors:
        parts.append(f"{authors}")
    parts.append(f"({year}).")
    parts.append(f"{title}.")
    if journal:
        vol = meta.get("volume")
        issue = meta.get("issue")
        pages = meta.get("pages") or ""
        journal_part = f"*{journal}*"
        if vol:
            journal_part += f", {vol}"
            if issue:
                journal_part += f"({issue})"
        if pages:
            journal_part += f", {pages}"
        parts.append(journal_part + ".")
    elif publisher:
        parts.append(f"{publisher}.")
    if doi:
        parts.append(f"https://doi.org/{doi}")
    elif url:
        parts.append(url)
    return " ".join(p for p in parts if p).strip()
