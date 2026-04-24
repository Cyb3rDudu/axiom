"""Parser robustness fixes (#75).

Four regressions this pins down:

1. bibliography_migrator — multi-initial author names no longer fragment the title.
2. citation_sync — duplicate (family, year) surfaces `ambiguous` diagnostic.
3. citation_sync — numbered marker regex rejects lowercase-only placeholders.
4. api.writing — strip_inline_section stops at the next level-1/2 heading.
"""

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

from services.bibliography_migrator import migrate_markdown_bibliography  # noqa: E402
from services.citation_sync import (  # noqa: E402
    parse_in_text_citations,
    validate_citations,
)
from api.writing import (  # noqa: E402
    _strip_inline_bibliography,
    _strip_inline_portfolio,
)


# ---------------------------------------------------------------------------
# #75.1: bibliography migrator — multi-initial authors
# ---------------------------------------------------------------------------


class TestMigratorMultiInitialAuthors:
    def test_two_initials_no_italic_title(self):
        md = (
            "## Literaturverzeichnis\n\n"
            "- Müller, P. U. (2024). Title without italic. Vahlen.\n"
        )
        preview = migrate_markdown_bibliography(md)
        assert len(preview.entries) == 1
        entry = preview.entries[0]
        assert entry.authors[0]["family"] == "Müller"
        # Given initials survive the split (used to be clobbered into the title)
        assert "P." in entry.authors[0]["given"]
        assert "U" in entry.authors[0]["given"]
        assert entry.year == 2024
        # The load-bearing check: title no longer carries the initial run
        assert entry.title == "Title without italic"
        assert entry.publisher == "Vahlen"

    def test_three_initials_journal_article(self):
        md = (
            "## Literaturverzeichnis\n\n"
            "- Smith, J. K. L. (2020). Trade policy analysis. Journal of Econ, 42, 1-20.\n"
        )
        preview = migrate_markdown_bibliography(md)
        assert len(preview.entries) == 1
        e = preview.entries[0]
        assert e.year == 2020
        assert e.title == "Trade policy analysis"

    def test_single_initial_still_works(self):
        md = "## Literaturverzeichnis\n\n- Müller, P. (2024). *Makroökonomie*. Vahlen.\n"
        preview = migrate_markdown_bibliography(md)
        assert len(preview.entries) == 1
        assert preview.entries[0].title == "Makroökonomie"
        assert preview.entries[0].publisher == "Vahlen"


# ---------------------------------------------------------------------------
# #75.2: citation_sync — duplicate (family, year) is ambiguous, not arbitrary
# ---------------------------------------------------------------------------


class TestDuplicateFamilyYearAmbiguous:
    def test_two_works_same_author_same_year_surfaces_ambiguous(self):
        body = "Daten zeigen X (Müller, 2024, S. 12)."
        registry = [
            {
                "entry_key": "mueller-2024-a",
                "authors": [{"family": "Müller", "given": "P."}],
                "year": 2024,
                "title": "Werk A",
            },
            {
                "entry_key": "mueller-2024-b",
                "authors": [{"family": "Müller", "given": "P."}],
                "year": 2024,
                "title": "Werk B",
            },
        ]
        report = validate_citations(body, registry)
        assert len(report.ambiguous) == 1
        marker, candidates = report.ambiguous[0]
        assert sorted(candidates) == ["mueller-2024-a", "mueller-2024-b"]
        # Ambiguous markers don't count as orphans (they have candidates)
        assert report.orphan_markers == []
        # Neither entry is a dead entry (both resolved via candidate)
        assert report.dead_entries == []
        # Report surfaces the diagnostic via has_warnings
        assert report.has_warnings is True

    def test_single_match_still_resolves(self):
        body = "(Müller, 2024, S. 12)."
        registry = [
            {
                "entry_key": "mueller-2024",
                "authors": [{"family": "Müller", "given": "P."}],
                "year": 2024,
                "title": "Werk",
            }
        ]
        report = validate_citations(body, registry)
        assert report.ambiguous == []
        assert len(report.resolved) == 1
        assert report.resolved[0][1] == "mueller-2024"

    def test_report_to_dict_exposes_ambiguous(self):
        body = "(Müller, 2024, S. 12)."
        registry = [
            {"entry_key": "a", "authors": [{"family": "Müller"}], "year": 2024, "title": "A"},
            {"entry_key": "b", "authors": [{"family": "Müller"}], "year": 2024, "title": "B"},
        ]
        d = validate_citations(body, registry).to_dict()
        assert d["ambiguous_count"] == 1
        assert d["ambiguous_samples"][0]["candidates"] == ["a", "b"]
        assert d["has_warnings"] is True


# ---------------------------------------------------------------------------
# #75.3: numbered marker regex — lowercase placeholders rejected
# ---------------------------------------------------------------------------


class TestNumberedMarkerFalsePositives:
    @pytest.mark.parametrize(
        "placeholder",
        [
            "[wortstand]",
            "[abschnitt]",
            "[kapitel]",
            "[bedingung]",
        ],
    )
    def test_lowercase_only_placeholders_rejected(self, placeholder):
        body = f"Body with {placeholder} scaffolding."
        markers = parse_in_text_citations(body)
        assert markers == [], f"false positive on {placeholder}"

    @pytest.mark.parametrize(
        "valid",
        [
            "[1]",
            "[42]",
            "[f28769c8]",   # doc-id hex
            "[a3b1c9d0]",   # doc-id hex
            "[mueller-2024]",  # slug
            "[todo-42]",    # slug with digit
        ],
    )
    def test_valid_markers_still_match(self, valid):
        body = f"See {valid} for reference."
        markers = parse_in_text_citations(body)
        assert len(markers) == 1
        assert markers[0].marker == valid


# ---------------------------------------------------------------------------
# #75.4: strip_inline_section stops at next level-1/2 heading, eats sub-sections
# ---------------------------------------------------------------------------


class TestStripInlineSectionBounded:
    def test_strips_sub_sections_inside_bibliography(self):
        md = (
            "# Draft\n\nBody.\n\n"
            "## Literaturverzeichnis\n\n"
            "### Primärliteratur\n- Müller (2024).\n\n"
            "### Sekundärliteratur\n- Smith (2020).\n"
        )
        out = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in out
        assert "Primärliteratur" not in out
        assert "Sekundärliteratur" not in out

    def test_does_not_eat_following_level_2_heading(self):
        md = (
            "## Literaturverzeichnis\n\n- Müller (2024).\n\n"
            "## Anhang\n\nAppendix body.\n"
        )
        out = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in out
        assert "## Anhang" in out
        assert "Appendix body" in out

    def test_does_not_eat_following_level_1_heading(self):
        md = (
            "## Literaturverzeichnis\n\n- Müller (2024).\n\n"
            "# Next Chapter\n\nContent.\n"
        )
        out = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in out
        assert "# Next Chapter" in out
        assert "Content." in out

    def test_strips_portfolio_with_sub_sections(self):
        md = (
            "# Draft\n\nBody.\n\n"
            "## Literaturportfolio\n\n"
            "### Tabelle\n\n| Quelle | … |\n\n"
            "### Compliance\n\n🟢 grün\n"
        )
        out = _strip_inline_portfolio(md)
        assert "Literaturportfolio" not in out
        assert "Tabelle" not in out
        assert "Compliance" not in out

    def test_empty_input_unchanged(self):
        assert _strip_inline_bibliography("# Just body.\n") == "# Just body.\n"
