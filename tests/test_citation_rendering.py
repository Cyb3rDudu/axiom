"""Tests for citation rendering (#51/#57).

Pin the Markdown projection for the three built-in profiles against the
structured-entry dict shape the writer will emit.
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

from services.citation_rendering import render_bibliography, render_entry  # noqa: E402


BOOK = {
    "entry_key": "mueller-2024",
    "authors": [{"family": "Müller", "given": "Peter"}],
    "year": 2024,
    "title": "China in der Weltwirtschaft",
    "publisher": "Vahlen",
    "reference_type": "document",
}

JOURNAL_ARTICLE = {
    "entry_key": "smith-2020",
    "authors": [{"family": "Smith", "given": "J."}, {"family": "Jones", "given": "A."}],
    "year": 2020,
    "title": "Trade policy in a fragmented world",
    "container_title": "Journal of International Economics",
    "pages": "45-62",
    "reference_type": "document",
}

WEB_SOURCE = {
    "entry_key": "destatis-2024",
    "authors": [{"family": "Destatis", "given": ""}],
    "year": 2024,
    "title": "Außenhandel 2024",
    "container_title": "Statistisches Bundesamt",
    "url": "https://www.destatis.de/DE/Home/",
    "accessed_at": "2026-04-24",
    "reference_type": "web",
}


class TestKmuApa6:
    def test_book(self):
        out = render_entry(BOOK, "kmu_apa6")
        assert "Müller, P." in out
        assert "(2024)" in out
        assert "*China in der Weltwirtschaft*" in out
        assert "Vahlen." in out

    def test_journal_article(self):
        out = render_entry(JOURNAL_ARTICLE, "kmu_apa6")
        assert "Smith, J., & Jones, A." in out
        assert "*Journal of International Economics*" in out
        assert "45-62" in out

    def test_web_source_with_accessed_date(self):
        out = render_entry(WEB_SOURCE, "kmu_apa6")
        assert "Destatis" in out
        assert "(2024)" in out
        assert "Abgerufen am 24.04.2026" in out
        assert "https://www.destatis.de/DE/Home/" in out

    def test_missing_year_uses_oj(self):
        out = render_entry({"authors": [{"family": "X", "given": ""}], "title": "T", "publisher": "Pub"}, "kmu_apa6")
        assert "(o. J.)" in out

    def test_missing_authors_uses_oa(self):
        out = render_entry({"year": 2020, "title": "T", "publisher": "Pub"}, "kmu_apa6")
        assert out.startswith("o. A.")


class TestApa7En:
    def test_book(self):
        out = render_entry(BOOK, "apa7_en")
        assert "Müller, P." in out
        assert "(2024)" in out
        assert "*China in der Weltwirtschaft*" in out

    def test_web_source_uses_retrieved(self):
        out = render_entry(WEB_SOURCE, "apa7_en")
        assert "Retrieved" in out
        assert "https://www.destatis.de/DE/Home/" in out

    def test_missing_year_uses_nd(self):
        out = render_entry({"authors": [{"family": "X", "given": ""}], "title": "T", "publisher": "Pub"}, "apa7_en")
        assert "(n.d.)" in out


class TestNumbered:
    def test_entry_with_index(self):
        out = render_entry(BOOK, "numbered", index=1)
        assert out.startswith("[1] ")
        assert "2024" in out
        assert "Müller" in out

    def test_entry_without_index(self):
        out = render_entry(BOOK, "numbered")
        assert not out.startswith("[")


class TestRenderBibliography:
    def test_empty_returns_empty(self):
        assert render_bibliography([], "kmu_apa6") == ""

    def test_kmu_heading(self):
        out = render_bibliography([BOOK], "kmu_apa6")
        assert out.startswith("## Literaturverzeichnis")
        assert "- Müller" in out  # bullet list for APA variants

    def test_apa7_heading(self):
        out = render_bibliography([BOOK], "apa7_en")
        assert out.startswith("## References")

    def test_numbered_preserves_input_order(self):
        out = render_bibliography([BOOK, JOURNAL_ARTICLE, WEB_SOURCE], "numbered")
        lines = [l for l in out.splitlines() if l.startswith("[")]
        assert lines[0].startswith("[1] ")
        assert lines[1].startswith("[2] ")
        assert lines[2].startswith("[3] ")
        # Order preserved
        assert "China in der Weltwirtschaft" in lines[0]
        assert "Trade policy" in lines[1]
        assert "Außenhandel 2024" in lines[2]

    def test_apa_sorts_by_author_year(self):
        # APA sort: alphabetical by family then year
        out = render_bibliography(
            [WEB_SOURCE, BOOK, JOURNAL_ARTICLE],  # deliberately out of order
            "kmu_apa6",
        )
        lines = [l for l in out.splitlines() if l.startswith("-")]
        # Destatis < Müller < Smith
        assert "Destatis" in lines[0]
        assert "Müller" in lines[1]
        assert "Smith" in lines[2]

    def test_include_heading_false(self):
        out = render_bibliography([BOOK], "kmu_apa6", include_heading=False)
        assert not out.startswith("##")
