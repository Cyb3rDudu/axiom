"""Tests for writing_markdown.py — section + fence + wordcount regexes."""

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

from services.writing_markdown import (  # noqa: E402
    DOC_FENCE_RE,
    REFS_FENCE_RE,
    SECTION_HEADING_RE,
    WORDCOUNT_BLOCK_RE,
    WORDCOUNT_TRAILER_RE,
    extract_document_body,
    extract_references_body,
    iter_section_heads,
    parse_declared_wordcount,
    replace_document_body,
)


class TestSectionHeadingRE:
    @pytest.mark.parametrize(
        "line,matches",
        [
            ("# 1. Einleitung", True),
            ("## 1. Einleitung", True),
            ("### 1. Einleitung", True),
            ("#### 4. Subsection", True),
            ("##### 5. Too deep", False),  # H5 rejected
            ("1. Einleitung", False),       # no # sigil
            ("# Einleitung", False),         # no number
            ("#1. Einleitung", False),       # missing space after #
        ],
    )
    def test_level_tolerance(self, line, matches):
        assert bool(SECTION_HEADING_RE.match(line)) is matches

    def test_iter_section_heads_returns_index_title_offset(self):
        body = (
            "# Paper Title\n\n"
            "## 1. Einleitung\n\nProse.\n\n"
            "## 2. Theorie\n\nMore prose.\n"
        )
        results = list(iter_section_heads(body))
        assert [r[0] for r in results] == [1, 2]
        assert [r[1] for r in results] == ["Einleitung", "Theorie"]
        # Offsets are monotonically increasing
        assert results[0][2] < results[1][2]


class TestDocFence:
    def test_extract_simple(self):
        text = "before\n\n```content-block:document\ndocument body\n```\n\nafter"
        assert extract_document_body(text) == "document body"

    def test_extract_multiline_body(self):
        text = "```content-block:document\nline 1\nline 2\nline 3\n```"
        assert extract_document_body(text) == "line 1\nline 2\nline 3"

    def test_no_fence_returns_none(self):
        assert extract_document_body("just plain text") is None
        assert extract_document_body("") is None
        assert extract_document_body(None) is None

    def test_replace_body_preserves_surroundings(self):
        text = (
            "```content-block:references\n[]\n```\n\n"
            "```content-block:document\nold body\n```\n\n"
            "Wortbilanz: 123"
        )
        updated = replace_document_body(text, "new body content")
        assert "new body content" in updated
        assert "old body" not in updated
        assert "content-block:references" in updated
        assert "Wortbilanz: 123" in updated

    def test_replace_body_noop_without_fence(self):
        text = "no fence here"
        assert replace_document_body(text, "x") == text


class TestRefsFence:
    def test_extract_references_body(self):
        text = (
            "```content-block:references\n"
            '[{"entry_key": "x"}]\n'
            "```"
        )
        body = extract_references_body(text)
        assert body == '[{"entry_key": "x"}]'

    def test_case_insensitive(self):
        text = "```content-block:References\n[]\n```"
        assert extract_references_body(text) == "[]"


class TestWordcountTrailer:
    @pytest.mark.parametrize(
        "line,expected",
        [
            ("Wortbilanz: 2910 insgesamt", 2910),
            ("**Wortbilanz: 2.910 Wörter**", 2910),
            ("Wortbilanz (exkl. Titelblatt): **2.910 Wörter**", 2910),
            ("Word count: 2910", 2910),
            ("**Word count (excl. title page and bibliography): 2,910 words**", 2910),
            ("Total: 2910 words", 2910),
            ("No trailer here", None),
            ("", None),
        ],
    )
    def test_parse_declared(self, line, expected):
        assert parse_declared_wordcount(line) == expected

    def test_block_captures_breakdown(self):
        trailer = (
            "**Wortbilanz: 2.910 Wörter**\n"
            "- 1. Einleitung: 410\n"
            "- 2. Theorie: 520\n"
        )
        m = WORDCOUNT_BLOCK_RE.search(trailer)
        assert m is not None
        full = m.group("full")
        assert "Einleitung: 410" in full
        assert "Theorie: 520" in full
