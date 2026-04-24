"""Tests for text_utils.py — umlauts, slug, family-name matching."""

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

from services.text_utils import (  # noqa: E402
    GERMAN_UMLAUTS,
    normalise_family_name,
    slugify_ascii,
)


class TestSlugify:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("Müller", "mueller"),
            ("Grüße aus Köln", "gruesse-aus-koeln"),
            ("Hotz-Hart", "hotz-hart"),
            ("  Spaces   galore  ", "spaces-galore"),
            ("straße", "strasse"),
            ("ABC-2024", "abc-2024"),
            # All punctuation → fallback
            ("!!!", "ref"),
            ("", "ref"),
        ],
    )
    def test_produces_ascii_slugs(self, raw, expected):
        assert slugify_ascii(raw) == expected

    def test_fallback_parameter(self):
        assert slugify_ascii("", fallback="empty") == "empty"
        assert slugify_ascii("---", fallback="xxx") == "xxx"

    def test_custom_separator(self):
        assert slugify_ascii("Foo Bar Baz", separator="_") == "foo_bar_baz"

    def test_no_separator(self):
        assert slugify_ascii("Hotz-Hart 2024", separator="") == "hotzhart2024"


class TestNormaliseFamilyName:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("Müller", "mueller"),
            ("Hotz-Hart", "hotzhart"),
            ("de la Cruz", "delacruz"),
            ("O'Reilly", "oreilly"),
            ("", ""),
            ("  ", ""),
        ],
    )
    def test_collapses_to_matching_form(self, raw, expected):
        assert normalise_family_name(raw) == expected

    def test_umlaut_variants_match_each_other(self):
        assert normalise_family_name("Müller") == normalise_family_name("Mueller")
        assert normalise_family_name("Größe") == normalise_family_name("Groesse")


class TestGermanUmlauts:
    def test_covers_all_six_lowercase_and_uppercase(self):
        # ä ö ü Ä Ö Ü ß  — seven but uppercase ß is modern addition; we
        # don't need to handle it for our use case.
        assert "ä".translate(GERMAN_UMLAUTS) == "ae"
        assert "ö".translate(GERMAN_UMLAUTS) == "oe"
        assert "ü".translate(GERMAN_UMLAUTS) == "ue"
        assert "Ä".translate(GERMAN_UMLAUTS) == "ae"
        assert "Ö".translate(GERMAN_UMLAUTS) == "oe"
        assert "Ü".translate(GERMAN_UMLAUTS) == "ue"
        assert "ß".translate(GERMAN_UMLAUTS) == "ss"
