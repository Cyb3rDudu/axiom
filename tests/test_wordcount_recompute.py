"""Tests for the deterministic Wortbilanz recomputer."""

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

from services.writing_response_audit import (  # noqa: E402
    count_document_words,
    recompute_wortbilanz,
)


TWO_WORDS_PER_SECTION = """\
# 1. Einleitung

One two

# 2. Theorie

Three four

# 3. Schluss

Five six
"""


DRAFT_WITH_HALLUCINATED_WORTBILANZ = f"""\
```content-block:document
{TWO_WORDS_PER_SECTION}
```

**Wortbilanz (exkl. Titelblatt und Literaturverzeichnis): 2.910 Wörter**
- 1. Einleitung: 410
- 2. Theorie: 520
- 3. Schluss: 330
"""


class TestCountDocumentWords:
    def test_per_section_split(self):
        total, sections = count_document_words(TWO_WORDS_PER_SECTION)
        assert total == 6
        assert [title for title, _ in sections] == [
            "1. Einleitung",
            "2. Theorie",
            "3. Schluss",
        ]
        assert [w for _, w in sections] == [2, 2, 2]

    def test_figure_markdown_excluded(self):
        body = (
            "# 1. Foo\n\nOne two three\n"
            "![Abbildung 1: Lorem ipsum dolor sit amet consectetur](/fig.png)\n"
            "*Abbildung 1: Lorem ipsum dolor sit amet consectetur adipiscing elit.*\n"
        )
        total, _ = count_document_words(body)
        assert total == 3

    def test_section_counters_stripped(self):
        body = "# 1. Foo\n\nAlpha beta gamma [520]\n"
        total, _ = count_document_words(body)
        assert total == 3

    def test_empty_body(self):
        assert count_document_words("") == (0, [])

    def test_preamble_counted_separately(self):
        body = "Preamble prose here.\n\n# 1. First\n\nInside first section."
        total, sections = count_document_words(body)
        titles = [t for t, _ in sections]
        assert "preamble" in titles
        assert "1. First" in titles


class TestRecomputeWortbilanz:
    def test_replaces_hallucinated_numbers(self):
        updated, tele = recompute_wortbilanz(DRAFT_WITH_HALLUCINATED_WORTBILANZ)
        assert "2.910" not in updated
        # Real count is 6 words total
        assert "6 Wörter" in updated
        assert tele["declared"] == 2910
        assert tele["actual"] == 6
        assert tele["delta"] == 2904

    def test_preserves_per_section_breakdown_with_real_numbers(self):
        updated, _ = recompute_wortbilanz(DRAFT_WITH_HALLUCINATED_WORTBILANZ)
        assert "1. Einleitung: 2" in updated
        assert "2. Theorie: 2" in updated
        assert "3. Schluss: 2" in updated
        # Old hallucinated section counts gone
        assert "1. Einleitung: 410" not in updated

    def test_appends_when_no_wortbilanz_emitted(self):
        no_bilanz = f"```content-block:document\n{TWO_WORDS_PER_SECTION}\n```\n"
        # Default language detection: no 'Wortbilanz' token → English render
        updated, tele = recompute_wortbilanz(no_bilanz)
        assert ("**Wortbilanz" in updated) or ("**Word count" in updated)
        assert ("6 Wörter" in updated) or ("6 words" in updated)
        assert tele["declared"] is None
        assert tele["actual"] == 6

    def test_explicit_language_override(self):
        no_bilanz = f"```content-block:document\n{TWO_WORDS_PER_SECTION}\n```\n"
        updated_en, _ = recompute_wortbilanz(no_bilanz, language_code="en")
        assert "**Word count" in updated_en
        assert "6 words" in updated_en
        updated_de, _ = recompute_wortbilanz(no_bilanz, language_code="de")
        assert "**Wortbilanz" in updated_de
        assert "6 Wörter" in updated_de

    def test_noop_without_document_block(self):
        content = "Just a chat message, no draft."
        updated, tele = recompute_wortbilanz(content)
        assert updated == content
        assert tele is None

    def test_idempotent(self):
        once, _ = recompute_wortbilanz(DRAFT_WITH_HALLUCINATED_WORTBILANZ)
        twice, _ = recompute_wortbilanz(once)
        assert once == twice

    def test_handles_figure_captions_in_count(self):
        body_with_fig = (
            "```content-block:document\n"
            "# 1. Foo\n\nAlpha beta gamma.\n\n"
            "![Abbildung 1: Some chart](/api/documents/images/doc/1.png)\n"
            "*Abbildung 1: Some chart from source.*\n\n"
            "# 2. Bar\n\nDelta epsilon.\n"
            "```\n"
            "**Wortbilanz: 999 Wörter**\n"
        )
        updated, tele = recompute_wortbilanz(body_with_fig)
        # Prose: "Alpha beta gamma" (3) + "Delta epsilon" (2) = 5
        # Figure alt/caption should NOT count toward words
        assert tele["actual"] == 5
        assert "999" not in updated
