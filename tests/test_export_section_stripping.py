"""Tests for the DOCX-export section-stripping helpers (#67).

Both strippers need to match inline headings regardless of whether the
section is the last one in the document or followed by another heading.
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

from api.writing import (  # noqa: E402
    _strip_inline_bibliography,
    _strip_inline_portfolio,
    _strip_inline_section,
)


class TestStripInlineSection:
    def test_strips_tail_section(self):
        md = "# Body\n\nContent\n\n## Literaturverzeichnis\n\n- Müller, P. (2024)."
        out = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in out
        assert "Content" in out

    def test_strips_mid_section_stops_at_next_heading(self):
        md = (
            "# Body\n\n"
            "## Literaturverzeichnis\n\n- Müller, P. (2024).\n\n"
            "## Anhang\n\nAppendix body.\n"
        )
        out = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in out
        # Anhang heading survives
        assert "## Anhang" in out
        assert "Appendix body" in out

    def test_returns_unchanged_when_missing(self):
        md = "# Body\n\nJust prose.\n"
        assert _strip_inline_bibliography(md) == md

    def test_english_heading(self):
        md = "# Body\n\n## References\n\n- Smith (2020).\n"
        assert "References" not in _strip_inline_bibliography(md)

    def test_portfolio_stripper(self):
        md = (
            "# Body\n\n"
            "## Literaturportfolio\n\nbla bla\n\n"
            "## Anhang\n\nAppendix.\n"
        )
        out = _strip_inline_portfolio(md)
        assert "Literaturportfolio" not in out
        assert "## Anhang" in out

    def test_portfolio_english_variant(self):
        md = "## Literature Portfolio\n\n- table -\n"
        assert _strip_inline_portfolio(md).strip() == ""

    def test_both_strippers_are_independent(self):
        md = (
            "Body.\n\n"
            "## Literaturverzeichnis\n\nrefs\n\n"
            "## Literaturportfolio\n\ntable\n"
        )
        # Bibliography stripper kills verzeichnis but leaves portfolio
        bibliography_only = _strip_inline_bibliography(md)
        assert "Literaturverzeichnis" not in bibliography_only
        assert "Literaturportfolio" in bibliography_only
        # Portfolio stripper kills portfolio but leaves verzeichnis
        portfolio_only = _strip_inline_portfolio(md)
        assert "Literaturverzeichnis" in portfolio_only
        assert "Literaturportfolio" not in portfolio_only

    def test_generic_stripper_accepts_arbitrary_pattern(self):
        md = "Body\n\n## MyCustom\n\ncontent\n"
        out = _strip_inline_section(md, "MyCustom")
        assert "MyCustom" not in out
