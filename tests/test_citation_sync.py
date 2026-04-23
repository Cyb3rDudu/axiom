"""Tests for the in-text citation sync parser + validator (#51/#55)."""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.citation_sync import (  # noqa: E402
    parse_in_text_citations,
    strip_references_block,
    validate_citations,
)


REG_DESTATIS = {
    "entry_key": "destatis-2024",
    "authors": [{"family": "Destatis", "given": ""}],
    "year": 2024,
    "title": "Außenhandel 2024",
    "url": "https://destatis.de",
    "reference_type": "web",
}
REG_MUELLER = {
    "entry_key": "mueller-2024",
    "authors": [{"family": "Müller", "given": "P."}],
    "year": 2024,
    "title": "China in der Weltwirtschaft",
    "publisher": "Vahlen",
    "reference_type": "document",
}
REG_SMITH = {
    "entry_key": "smith-2020",
    "authors": [{"family": "Smith", "given": "J."}, {"family": "Jones", "given": "A."}],
    "year": 2020,
    "title": "Trade policy",
    "container_title": "JIE",
    "reference_type": "document",
}


class TestParseInTextCitations:
    def test_apa_single_author(self):
        body = "China exportiert viel (Müller, 2024, S. 45)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].mode == "apa"
        assert markers[0].author_hint == "mueller"
        assert markers[0].year == 2024
        assert markers[0].page == "S. 45"

    def test_apa_with_ampersand(self):
        body = "Studies show X (Smith & Jones, 2020, p. 45)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].author_hint == "smith"
        assert markers[0].year == 2020

    def test_apa_institutional_author(self):
        body = "The outlook is stable (Destatis, 2024, o. S.)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].author_hint == "destatis"
        assert markers[0].year == 2024

    def test_apa_et_al(self):
        body = "Data show X (Müller et al., 2020, S. 12)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].author_hint == "mueller"
        assert markers[0].year == 2020

    def test_apa_no_page(self):
        body = "Context here (Müller, 2024)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].page is None

    def test_numbered_bracket(self):
        body = "See [1] for details and compare [2]."
        markers = parse_in_text_citations(body)
        assert len(markers) == 2
        assert markers[0].mode == "numbered"
        assert markers[0].key_hint == "1"
        assert markers[1].key_hint == "2"

    def test_doc_id_bracket(self):
        body = "See [f28769c8] for the original."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].key_hint == "f28769c8"

    def test_ignores_non_citations(self):
        body = "Some text [Wortstand: 500] and (Smith says X is) true."
        markers = parse_in_text_citations(body)
        # The "[Wortstand: 500]" shouldn't match numbered (contains colon+space)
        # The "(Smith says...)" shouldn't match APA (no year)
        assert markers == []

    def test_offsets_and_paragraph_index(self):
        body = "First paragraph (Müller, 2024, S. 1).\n\nSecond (Smith, 2020, p. 2)."
        markers = parse_in_text_citations(body)
        assert len(markers) == 2
        assert markers[0].paragraph_index == 0
        assert markers[1].paragraph_index == 1
        assert markers[0].char_offset_start < markers[1].char_offset_start


class TestValidateCitations:
    def test_all_resolved(self):
        body = "(Müller, 2024, S. 1) and (Destatis, 2024, o. S.)."
        report = validate_citations(body, [REG_MUELLER, REG_DESTATIS])
        assert report.orphan_markers == []
        assert report.dead_entries == []
        assert len(report.resolved) == 2

    def test_orphan_citation(self):
        body = "(Unbekannt, 2020, S. 5) is an orphan."
        report = validate_citations(body, [REG_MUELLER])
        assert len(report.orphan_markers) == 1
        assert report.orphan_markers[0].author_hint == "unbekannt"
        # Müller not cited → dead entry
        assert "mueller-2024" in report.dead_entries

    def test_dead_entry(self):
        body = "Only (Müller, 2024, S. 1)."
        report = validate_citations(body, [REG_MUELLER, REG_DESTATIS])
        assert report.dead_entries == ["destatis-2024"]
        assert report.orphan_markers == []

    def test_year_mismatch_is_orphan(self):
        body = "(Müller, 2019, S. 5)."  # registry has 2024
        report = validate_citations(body, [REG_MUELLER])
        assert len(report.orphan_markers) == 1
        assert "mueller-2024" in report.dead_entries

    def test_numbered_index_resolution(self):
        body = "See [1] and [2]."
        report = validate_citations(body, [REG_MUELLER, REG_DESTATIS])
        assert report.orphan_markers == []
        # Both resolved in order
        resolved_keys = [k for _, k in report.resolved]
        assert resolved_keys == ["mueller-2024", "destatis-2024"]

    def test_doc_id_resolution(self):
        entry = {**REG_MUELLER, "entry_key": "f28769c8"}
        body = "See [f28769c8] for details."
        report = validate_citations(body, [entry])
        assert report.orphan_markers == []
        assert report.resolved[0][1] == "f28769c8"

    def test_multiple_authors_matched_on_head(self):
        body = "(Smith & Jones, 2020, p. 45)."
        report = validate_citations(body, [REG_SMITH])
        assert report.orphan_markers == []
        assert report.resolved[0][1] == "smith-2020"


class TestStripReferencesBlock:
    def test_strips_references_fence(self):
        text = (
            "Body paragraph (Müller, 2024, S. 1).\n\n"
            "```content-block:references\n"
            '[{"entry_key": "mueller-2024", "title": "X"}]\n'
            "```\n"
        )
        cleaned = strip_references_block(text)
        assert "content-block:references" not in cleaned
        assert "(Müller, 2024, S. 1)" in cleaned

    def test_leaves_other_fences_alone(self):
        text = "```content-block:document\nContent\n```"
        assert strip_references_block(text) == text
