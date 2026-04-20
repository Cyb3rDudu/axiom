"""Tests for SourceQualityService signal computation."""

# Prime the codebase's fragile import graph. axiom_backend has multiple
# cycles between agents, context_manager, database.async_crud, and api.* —
# the canonical entry point (main.py) sidesteps them by importing api first.
# We mirror that here so `from ai_researcher.agentic_layer.schemas.notes
# import Note` (which transitively triggers the cycle) works.
import api as _api_primer  # noqa: F401  # isort: skip

import datetime as _dt

import pytest

from ai_researcher.agentic_layer.schemas.notes import Note, SourceMetadata
from ai_researcher.agentic_layer.schemas.portfolio import QualitySignals
from ai_researcher.agentic_layer.services.source_quality import (
    assign_scientific_tier,
    compute_quality_signals,
    discovery_tool_label,
    extract_apa_citation,
)


def _make_note(
    *,
    source_type: str = "document",
    source_id: str = "doc_1",
    metadata: dict | None = None,
) -> Note:
    return Note(
        content="dummy",
        source_type=source_type,
        source_id=source_id,
        source_metadata=SourceMetadata(**(metadata or {})),
    )


# ---------------------------------------------------------------------------
# compute_quality_signals
# ---------------------------------------------------------------------------


def test_peer_reviewed_journal_signals() -> None:
    note = _make_note(
        metadata={
            "authors": "Contreras, F., Baykal, E., & Abid, G.",
            "publication_year": 2020,
            "title": "E-leadership and teleworking",
            "journal": "Frontiers in Psychology",
            "publisher": "Frontiers Media",
            "doi": "10.3389/fpsyg.2020.590271",
        }
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publication_type == "peer_reviewed_journal"
    assert signals.peer_reviewed is True
    assert signals.publisher_tier == "A"
    assert signals.has_doi is True
    assert signals.recency_years == 6
    assert assign_scientific_tier(signals) == "A"


def test_springer_monograph() -> None:
    note = _make_note(
        metadata={
            "authors": "Angerer, Thomas",
            "publication_year": 2022,
            "title": "Managementforschung",
            "publisher": "Springer Gabler",
            "isbn": "9783658388201",
        }
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "A"
    assert signals.publication_type == "monograph_scientific_publisher"
    assert signals.has_isbn is True
    assert assign_scientific_tier(signals) == "A"


def test_ifo_working_paper() -> None:
    note = _make_note(
        metadata={
            "authors": "Felbermayr, Gabriel",
            "publication_year": 2024,
            "title": "China and the German economy — working paper",
            "publisher": "ifo Institute",
            "url": "https://www.ifo.de/DocDL/wp-2024-15.pdf",
        }
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "B"
    assert assign_scientific_tier(signals) == "B"


def test_mckinsey_industry_report() -> None:
    note = _make_note(
        source_type="web",
        metadata={
            "publication_year": 2023,
            "title": "The next frontier of supply chain",
            "publisher": "McKinsey & Company",
            "url": "https://www.mckinsey.com/capabilities/supply-chain/insights",
        }
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "D"
    assert signals.publication_type == "industry_report"
    assert "consultancy_bias" in signals.bias_flags
    assert assign_scientific_tier(signals) == "D"


def test_wikipedia_blacklist() -> None:
    note = _make_note(
        source_type="web",
        metadata={
            "title": "Globalisation",
            "url": "https://en.wikipedia.org/wiki/Globalisation",
            "publication_year": 2025,
        }
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "blacklist"
    assert "disallowed_as_primary_source" in signals.bias_flags
    assert assign_scientific_tier(signals) == "D"


def test_recency_none_when_year_missing() -> None:
    note = _make_note(metadata={"title": "Something"})
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.recency_years is None


def test_internal_note_is_unknown() -> None:
    note = _make_note(source_type="internal", metadata={"title": "Agent thought"})
    signals = compute_quality_signals(note)
    assert signals.publication_type == "unknown"
    assert signals.publisher_tier == "unknown"
    assert assign_scientific_tier(signals) == "C"  # conservative fallback


def test_local_rag_document_with_thin_metadata_defaults_to_tier_a() -> None:
    """Regression: user-ingested PDFs without publisher/url metadata should
    land on Tier A (academic library), not on Tier C ('unknown' fallback).
    Mirrors the first mission run where Mankiw/Bofinger PDFs surfaced as
    Publisher-Tier unknown."""
    note = _make_note(
        source_type="document",
        metadata={
            "authors": "Bofinger, Peter",
            "publication_year": 2011,
            "title": "Grundzüge der Volkswirtschaftslehre",
            "original_filename": "Bofinger2011_VWL.pdf",
            # note: no publisher, no url, no doi
        },
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "A"
    assert signals.publication_type == "monograph_scientific_publisher"
    assert assign_scientific_tier(signals) == "A"


def test_document_with_known_publisher_keeps_its_real_tier() -> None:
    """The local-RAG fallback must not override explicit publisher signals."""
    note = _make_note(
        source_type="document",
        metadata={
            "title": "Enterprise X",
            "publisher": "McKinsey Global Institute",
            "publication_year": 2023,
        },
    )
    signals = compute_quality_signals(note, now=_dt.date(2026, 4, 20))
    assert signals.publisher_tier == "D"  # McKinsey is practitioner
    assert assign_scientific_tier(signals) == "D"


def test_web_note_without_publisher_stays_unknown() -> None:
    """Fallback is source_type='document' only — web notes stay unknown."""
    note = _make_note(
        source_type="web",
        metadata={"title": "Random blog", "url": "https://example.com/x"},
    )
    signals = compute_quality_signals(note)
    assert signals.publisher_tier == "unknown"


def test_bpb_classified_as_tier_b() -> None:
    """Bundeszentrale für politische Bildung — mentioned in regression."""
    from ai_researcher.agentic_layer.services.publisher_tiers import classify_tier
    assert classify_tier("https://www.bpb.de/themen/asien/china/") == "B"


def test_bwl_lexikon_and_wirtschaftslexikon24_blacklisted() -> None:
    """KMU Dos-and-Don'ts excludes lexicon-style sites as primary sources."""
    from ai_researcher.agentic_layer.services.publisher_tiers import classify_tier
    assert classify_tier("https://www.bwl-lexikon.de/wiki/foo") == "blacklist"
    assert classify_tier("https://www.wirtschaftslexikon24.com/d/bar") == "blacklist"
    assert classify_tier("https://www.scribbr.de/aufbau-und-gliederung/") == "blacklist"


# ---------------------------------------------------------------------------
# assign_scientific_tier policy
# ---------------------------------------------------------------------------


def test_force_a_for_peer_reviewed_journal_even_without_publisher_tier() -> None:
    signals = QualitySignals(
        publication_type="peer_reviewed_journal",
        publisher_tier="unknown",
    )
    assert assign_scientific_tier(signals) == "A"


def test_demote_a_publisher_when_peer_review_explicit_false() -> None:
    signals = QualitySignals(
        publication_type="working_paper",
        publisher_tier="A",
        peer_reviewed=False,
    )
    assert assign_scientific_tier(signals) == "B"


# ---------------------------------------------------------------------------
# discovery_tool_label
# ---------------------------------------------------------------------------


def test_discovery_tool_document() -> None:
    note = _make_note(source_type="document")
    assert discovery_tool_label(note) == "Axiom Local Library (RAG)"


def test_discovery_tool_arxiv() -> None:
    note = _make_note(
        source_type="web",
        metadata={"url": "https://arxiv.org/abs/2404.01234"},
    )
    assert discovery_tool_label(note) == "arXiv"


def test_discovery_tool_scholar() -> None:
    note = _make_note(
        source_type="web",
        metadata={"url": "https://scholar.google.com/scholar?q=xyz"},
    )
    assert discovery_tool_label(note) == "Google Scholar"


def test_discovery_tool_web_fallback_uses_mission_settings_provider() -> None:
    note = _make_note(
        source_type="web",
        metadata={"url": "https://example.com/whitepaper.pdf"},
    )
    label = discovery_tool_label(
        note,
        mission_settings={"comprehensive_settings": {"search_provider": "tavily"}},
    )
    assert "tavily" in label.lower()


# ---------------------------------------------------------------------------
# extract_apa_citation
# ---------------------------------------------------------------------------


def test_apa_citation_journal_article() -> None:
    note = _make_note(
        metadata={
            "authors": "Contreras, F., Baykal, E., & Abid, G.",
            "publication_year": 2020,
            "title": "E-leadership and teleworking",
            "journal": "Frontiers in Psychology",
            "volume": "11",
            "issue": "1",
            "pages": "1-11",
            "doi": "10.3389/fpsyg.2020.590271",
        }
    )
    apa = extract_apa_citation(note)
    assert "Contreras" in apa
    assert "(2020)" in apa
    assert "Frontiers in Psychology" in apa
    assert "11(1)" in apa or "11, 1" in apa
    assert "doi.org/10.3389/fpsyg.2020.590271" in apa


def test_apa_citation_book_with_publisher() -> None:
    note = _make_note(
        metadata={
            "authors": "Angerer, T.",
            "publication_year": 2022,
            "title": "Managementforschung",
            "publisher": "Springer Gabler",
        }
    )
    apa = extract_apa_citation(note)
    assert "Springer Gabler" in apa
    assert "(2022)" in apa


def test_apa_falls_back_to_no_date_and_title_only() -> None:
    note = _make_note(metadata={"title": "Untitled Snippet"})
    apa = extract_apa_citation(note)
    assert "n.d." in apa
    assert "Untitled Snippet" in apa
