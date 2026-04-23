"""Tests for the inline-Markdown → structured migration parser (#51/#54)."""

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

from services.bibliography_migrator import (  # noqa: E402
    extract_bibliography_section,
    migrate_markdown_bibliography,
)


APA_SECTION = """
# Draft body

Some text (Müller, 2024, S. 45) and more (Destatis, 2024, o. S.).

## Literaturverzeichnis

- Müller, P. (2024). *China in der Weltwirtschaft* (2. Aufl.). Vahlen.
- Destatis. (2024). *Außenhandel 2024*. Statistisches Bundesamt. Abgerufen am 24.04.2026, von https://www.destatis.de/DE/Home/
- Smith, J., & Jones, A. (2020). Trade policy in a fragmented world. *Journal of International Economics, 112*(3), 45-62.
"""

NUMBERED_SECTION = """
# Draft body

See [1] and [2].

## References

[1] Müller, P. (2024). *China in der Weltwirtschaft*. Vahlen.
[2] Destatis. (2024). *Außenhandel 2024*. https://destatis.de
"""


class TestExtractSection:
    def test_finds_literaturverzeichnis(self):
        section = extract_bibliography_section(APA_SECTION)
        assert section is not None
        assert "Müller" in section
        assert "Smith" in section

    def test_finds_references(self):
        section = extract_bibliography_section(NUMBERED_SECTION)
        assert section is not None
        assert "Destatis" in section

    def test_returns_none_when_no_section(self):
        assert extract_bibliography_section("# Just some heading\n\nBody.") is None

    def test_stops_at_next_heading(self):
        md = (
            "## Literaturverzeichnis\n\n"
            "- Müller, P. (2024). *Title*. Publisher.\n\n"
            "## Appendix\n\n"
            "- Not a reference.\n"
        )
        section = extract_bibliography_section(md)
        assert section is not None
        assert "Müller" in section
        assert "Appendix" not in section
        assert "Not a reference" not in section


class TestApaMigration:
    def test_parses_apa_book(self):
        preview = migrate_markdown_bibliography(APA_SECTION)
        assert len(preview.entries) == 3
        mueller = next(e for e in preview.entries if "mueller" in e.entry_key)
        assert mueller.year == 2024
        assert mueller.title == "China in der Weltwirtschaft"
        assert mueller.publisher == "Vahlen"
        assert mueller.authors[0]["family"] == "Müller"
        assert mueller.confidence == "high"

    def test_parses_web_source(self):
        preview = migrate_markdown_bibliography(APA_SECTION)
        destatis = next(e for e in preview.entries if "destatis" in e.entry_key)
        assert destatis.year == 2024
        assert destatis.url == "https://www.destatis.de/DE/Home/"
        assert destatis.reference_type == "web"

    def test_parses_multi_author_journal(self):
        preview = migrate_markdown_bibliography(APA_SECTION)
        smith = next(e for e in preview.entries if "smith" in e.entry_key)
        assert smith.year == 2020
        assert len(smith.authors) == 2
        assert smith.authors[0]["family"] == "Smith"
        assert smith.authors[1]["family"] == "Jones"
        assert smith.container_title and "Journal of International Economics" in smith.container_title


class TestNumberedMigration:
    def test_parses_numbered_entries(self):
        preview = migrate_markdown_bibliography(NUMBERED_SECTION, profile_hint="numbered")
        assert len(preview.entries) == 2
        assert preview.entries[0].title == "China in der Weltwirtschaft"
        assert preview.entries[1].url == "https://destatis.de"


class TestRobustness:
    def test_unparsable_lines_surfaced(self):
        md = (
            "## Literaturverzeichnis\n\n"
            "- Müller, P. (2024). *Good Entry*. Publisher.\n"
            "- this is not a well-formed entry at all\n"
        )
        preview = migrate_markdown_bibliography(md)
        assert len(preview.entries) == 1
        assert len(preview.unparsable) == 1
        assert "not a well-formed" in preview.unparsable[0]

    def test_no_section_returns_empty(self):
        preview = migrate_markdown_bibliography("# Body only\n\nNo bibliography here.")
        assert preview.entries == []
        assert preview.unparsable == []

    def test_entry_key_uniqueness(self):
        # Two entries with the same author+year+title hash should get
        # suffixed keys, not collide
        md = (
            "## Literaturverzeichnis\n\n"
            "- Smith, J. (2024). *Title One*. Pub.\n"
            "- Smith, J. (2024). *Title One*. Pub.\n"
        )
        preview = migrate_markdown_bibliography(md)
        keys = [e.entry_key for e in preview.entries]
        assert len(keys) == len(set(keys)), f"duplicate keys: {keys}"
