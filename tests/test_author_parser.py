"""Tests for author_parser.py — single source of truth."""

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

from services.author_parser import parse_authors  # noqa: E402


class TestEmpty:
    @pytest.mark.parametrize("raw", [None, "", "   ", []])
    def test_empty_inputs(self, raw):
        assert parse_authors(raw) == []


class TestSingleAuthor:
    def test_family_given_comma_form(self):
        assert parse_authors("Müller, Peter") == [
            {"family": "Müller", "given": "Peter"}
        ]

    def test_given_family_form(self):
        assert parse_authors("Peter Müller") == [
            {"family": "Müller", "given": "Peter"}
        ]

    def test_institutional_single_token(self):
        assert parse_authors("Destatis") == [
            {"family": "Destatis", "given": ""}
        ]

    def test_multi_initial_given(self):
        assert parse_authors("Müller, P. U.") == [
            {"family": "Müller", "given": "P. U."}
        ]


class TestMultipleAuthors:
    def test_semicolon_separated(self):
        result = parse_authors("Müller, Peter; Schmidt, Anna")
        assert result == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "Anna"},
        ]

    def test_and_separated(self):
        result = parse_authors("Peter Müller and Anna Schmidt")
        assert result == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "Anna"},
        ]

    def test_ampersand_separated(self):
        result = parse_authors("Smith, J. & Jones, A.")
        assert result == [
            {"family": "Smith", "given": "J."},
            {"family": "Jones", "given": "A."},
        ]

    def test_mixed_separators(self):
        # ; dominates when mixed; other separators inside a chunk still work
        result = parse_authors("Smith, J.; Jones, A. & Lee, B.")
        families = [a["family"] for a in result]
        assert "Smith" in families
        assert "Jones" in families
        assert "Lee" in families

    def test_hyphenated_surname_preserved(self):
        result = parse_authors("Hotz-Hart, B. & Rohner, A.")
        assert result[0]["family"] == "Hotz-Hart"


class TestStructuredInput:
    def test_list_of_dicts_normalises(self):
        result = parse_authors(
            [
                {"family": "X", "given": "Y"},
                {"family": "Z"},  # no given
            ]
        )
        assert result == [
            {"family": "X", "given": "Y"},
            {"family": "Z", "given": ""},
        ]

    def test_list_drops_entries_without_family(self):
        result = parse_authors(
            [{"family": "", "given": "Y"}, {"family": "Z"}]
        )
        assert result == [{"family": "Z", "given": ""}]

    def test_list_of_strings_parsed_individually(self):
        result = parse_authors(["Peter Müller", "Anna Schmidt"])
        assert result == [
            {"family": "Müller", "given": "Peter"},
            {"family": "Schmidt", "given": "Anna"},
        ]


class TestUnparseableInput:
    def test_weird_types_return_empty(self):
        assert parse_authors(42) == []
        assert parse_authors(3.14) == []

    def test_dict_input_ignored(self):
        # Single dict is NOT a list; should return []
        assert parse_authors({"family": "X"}) == []
