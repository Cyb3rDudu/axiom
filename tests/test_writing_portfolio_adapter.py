"""Tests for writing_portfolio_adapter (#61/#62)."""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.writing_portfolio_adapter import (  # noqa: E402
    _authors_to_apa_string,
    _fake_note,
    _nearest_heading_slug,
    _context_window,
    _writing_mission_settings_like,
    reference_to_source_record,
)


# --- fixtures ---------------------------------------------------------------


def make_ref(**overrides):
    """Minimal Reference-like object with the attrs the adapter reads."""
    defaults = dict(
        id="ref-id-1",
        entry_key="destatis-2024",
        authors=[{"family": "Destatis", "given": ""}],
        year=2024,
        title="Außenhandel 2024",
        container_title="Statistisches Bundesamt",
        publisher=None,
        pages=None,
        url="https://www.destatis.de/DE/Home/",
        web_url=None,
        doi=None,
        document_id=None,
        citation_text="",
    )
    defaults.update(overrides)
    return SimpleNamespace(**defaults)


def make_entry(offset: int, end: int, marker: str = "(Destatis, 2024, o. S.)"):
    return SimpleNamespace(
        char_offset_start=offset,
        char_offset_end=end,
        in_text_marker=marker,
        paragraph_index=0,
    )


DRAFT_BODY = (
    "# Einleitung\n\n"
    "China ist ein wichtiger Handelspartner (Destatis, 2024, o. S.). "
    "Das zeigt die Außenhandelsstatistik.\n\n"
    "## Theorie\n\n"
    "Die Theorie nach Heckscher-Ohlin (Müller, 2020, S. 12) erklärt...\n\n"
    "## Empirie\n\n"
    "Weitere Daten (Destatis, 2024, o. S.) stützen das Modell.\n"
)


# --- _authors_to_apa_string -------------------------------------------------


class TestAuthorsJoin:
    def test_single_institutional(self):
        out = _authors_to_apa_string([{"family": "Destatis", "given": ""}])
        assert out == "Destatis"

    def test_single_person(self):
        out = _authors_to_apa_string([{"family": "Müller", "given": "Peter"}])
        assert out == "Müller, P."

    def test_two_authors_ampersand(self):
        out = _authors_to_apa_string(
            [{"family": "Smith", "given": "J."}, {"family": "Jones", "given": "A."}]
        )
        assert out == "Smith, J. & Jones, A."

    def test_three_authors_oxford(self):
        out = _authors_to_apa_string([
            {"family": "A", "given": "X"},
            {"family": "B", "given": "Y"},
            {"family": "C", "given": "Z"},
        ])
        assert out == "A, X., B, Y., & C, Z."

    def test_none_and_empty(self):
        assert _authors_to_apa_string(None) == ""
        assert _authors_to_apa_string([]) == ""

    def test_skips_familyless_entries(self):
        out = _authors_to_apa_string([{"family": "", "given": "Just a given"}])
        assert out == ""


# --- _fake_note -------------------------------------------------------------


class TestFakeNote:
    def test_local_doc_source_type(self):
        ref = make_ref(document_id="doc-xyz", url=None)
        n = _fake_note(ref)
        assert n.source_type == "document"

    def test_web_source_type(self):
        ref = make_ref()  # no document_id, url present
        n = _fake_note(ref)
        assert n.source_type == "web"

    def test_metadata_projection(self):
        ref = make_ref(year=2020, publisher="Vahlen", doi="10.1/abc")
        n = _fake_note(ref)
        assert n.source_metadata["publication_year"] == 2020
        assert n.source_metadata["publisher"] == "Vahlen"
        assert n.source_metadata["doi"] == "10.1/abc"
        assert "authors" in n.source_metadata

    def test_none_fields_dropped(self):
        ref = make_ref(publisher=None, doi=None, pages=None)
        n = _fake_note(ref)
        assert "publisher" not in n.source_metadata
        assert "doi" not in n.source_metadata


# --- _nearest_heading_slug --------------------------------------------------


class TestNearestHeading:
    def test_finds_preceding_h1(self):
        offset = DRAFT_BODY.index("(Destatis, 2024, o. S.)")
        assert _nearest_heading_slug(DRAFT_BODY, offset) == "einleitung"

    def test_finds_preceding_h2_theorie(self):
        offset = DRAFT_BODY.index("(Müller, 2020, S. 12)")
        assert _nearest_heading_slug(DRAFT_BODY, offset) == "theorie"

    def test_finds_empirie_heading(self):
        offset = DRAFT_BODY.rindex("(Destatis, 2024, o. S.)")
        assert _nearest_heading_slug(DRAFT_BODY, offset) == "empirie"

    def test_no_heading_fallback(self):
        body = "Plain text with (Destatis, 2024) cite."
        assert _nearest_heading_slug(body, 16) == "body"

    def test_umlauts_in_heading(self):
        body = "## Wirtschaftlicher Überblick\n\nText (X, 2024)."
        offset = body.index("(X, 2024)")
        assert _nearest_heading_slug(body, offset) == "wirtschaftlicher-ueberblick"


# --- _context_window --------------------------------------------------------


class TestContextWindow:
    def test_window_symmetric(self):
        body = "a" * 500 + " CITE " + "b" * 500
        start = body.index("CITE")
        out = _context_window(body, start, start + 4, radius=100)
        assert "CITE" in out
        # 100 chars of 'a' on the left, 100 chars of 'b' on the right
        assert out.count("a") >= 90
        assert out.count("b") >= 90

    def test_collapses_whitespace(self):
        body = "lots\n\n\nof   whitespace    CITE   here\n\nok."
        start = body.index("CITE")
        out = _context_window(body, start, start + 4, radius=50)
        assert "\n\n" not in out
        assert "  " not in out

    def test_clamps_at_boundaries(self):
        body = "CITE at the start."
        out = _context_window(body, 0, 4, radius=1000)
        assert out.startswith("CITE")

    def test_empty_body(self):
        assert _context_window("", 0, 0) == ""


# --- _writing_mission_settings_like ----------------------------------------


class TestSettingsProjection:
    def test_empty(self):
        assert _writing_mission_settings_like(None) == {}
        assert _writing_mission_settings_like({}) == {}

    def test_picks_writing_settings_provider(self):
        out = _writing_mission_settings_like(
            {"writing_settings": {"search_provider": "tavily"}}
        )
        assert out == {"comprehensive_settings": {"search_provider": "tavily"}}

    def test_falls_back_to_top_level(self):
        out = _writing_mission_settings_like({"search_provider": "linkup"})
        assert out == {"comprehensive_settings": {"search_provider": "linkup"}}


# --- reference_to_source_record --------------------------------------------


class TestReferenceToSourceRecord:
    def test_local_doc(self):
        ref = make_ref(
            entry_key="mueller-2020",
            authors=[{"family": "Müller", "given": "P."}],
            year=2020,
            title="Makroökonomie",
            publisher="Vahlen",
            url=None,
            document_id="doc-1",
        )
        entries = [make_entry(DRAFT_BODY.index("(Müller, 2020, S. 12)"), 0)]
        record = reference_to_source_record(ref, entries, DRAFT_BODY)
        assert record["source_id"] == "mueller-2020"
        assert record["discovery_tool"] == "Axiom Local Library (RAG)"
        assert record["scientific_tier"] in ("A", "B", "C")
        assert record["sections_used_in"] == ["theorie"]
        assert len(record["section_context_snippets"]) == 1
        assert "(Müller, 2020, S. 12)" in record["section_context_snippets"][0]["snippet"]

    def test_web_destatis(self):
        ref = make_ref()  # default: Destatis web
        cite_a = DRAFT_BODY.index("(Destatis, 2024, o. S.)")
        cite_b = DRAFT_BODY.rindex("(Destatis, 2024, o. S.)")
        entries = [
            make_entry(cite_a, cite_a + 23),
            make_entry(cite_b, cite_b + 23),
        ]
        record = reference_to_source_record(
            ref, entries, DRAFT_BODY, session_settings={"search_provider": "tavily"}
        )
        assert record["source_id"] == "destatis-2024"
        # Not Google Scholar / CrossRef → falls to the provider-labeled web search
        assert record["discovery_tool"] in (
            "Web Search (tavily)",
            "Web Search",
        )
        assert record["sections_used_in"] == ["einleitung", "empirie"]
        assert len(record["section_context_snippets"]) == 2

    def test_recognizes_scholar_host(self):
        ref = make_ref(url="https://scholar.google.com/citations?user=abc")
        record = reference_to_source_record(ref, [], DRAFT_BODY)
        assert record["discovery_tool"] == "Google Scholar"

    def test_uses_prerendered_citation_text_when_present(self):
        ref = make_ref(citation_text="Müller, P. (2020). *Vorgefertigter Text*. Verlag.")
        record = reference_to_source_record(ref, [], DRAFT_BODY)
        assert record["apa_citation"].startswith("Müller, P.")
        assert "Vorgefertigter Text" in record["apa_citation"]

    def test_no_occurrences(self):
        ref = make_ref()
        record = reference_to_source_record(ref, [], DRAFT_BODY)
        assert record["sections_used_in"] == []
        assert record["section_context_snippets"] == []

    def test_falls_back_to_id_when_entry_key_missing(self):
        ref = make_ref(entry_key=None, id="uuid-xyz")
        record = reference_to_source_record(ref, [], DRAFT_BODY)
        assert record["source_id"] == "uuid-xyz"
