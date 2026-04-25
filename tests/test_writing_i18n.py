"""Tests for writing_i18n.py translation helper."""

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

from services.writing_i18n import (  # noqa: E402
    DEFAULT_LANGUAGE,
    MESSAGES,
    SUPPORTED_LANGUAGES,
    has_translation,
    normalize_language_code,
    t,
)


class TestNormaliseLanguageCode:
    @pytest.mark.parametrize(
        "raw,expected",
        [
            ("de", "de"),
            ("en", "en"),
            ("DE", "de"),
            ("de-DE", "de"),
            ("de_AT", "de"),
            ("en-US", "en"),
            ("fr", "en"),  # unsupported → fallback
            ("", "en"),
            (None, "en"),
        ],
    )
    def test_normalisation(self, raw, expected):
        assert normalize_language_code(raw) == expected


class TestTranslate:
    def test_returns_german_template_for_de(self):
        out = t("continuation.to_write_header", "de")
        assert "Zu schreiben" in out

    def test_returns_english_template_for_en(self):
        out = t("continuation.to_write_header", "en")
        assert "remain to write" in out

    def test_falls_back_to_english_for_unsupported_lang(self):
        out = t("continuation.to_write_header", "fr")
        assert out == t("continuation.to_write_header", "en")

    def test_substitutes_placeholders(self):
        out = t("continuation.locked_sections", "en", last_done=3)
        assert "1–3" in out
        assert "{last_done}" not in out

    def test_de_and_en_variants_use_same_placeholders(self):
        for key, entry in MESSAGES.items():
            if "de" in entry and "en" in entry:
                import re
                de_ph = set(re.findall(r"\{(\w+)\}", entry["de"]))
                en_ph = set(re.findall(r"\{(\w+)\}", entry["en"]))
                assert de_ph == en_ph, (
                    f"placeholder mismatch in {key}: de={de_ph} en={en_ph}"
                )

    def test_missing_key_raises(self):
        with pytest.raises(KeyError):
            t("nonexistent.key", "de")


class TestHasTranslation:
    def test_existing_pairs(self):
        for lang in SUPPORTED_LANGUAGES:
            assert has_translation("continuation.to_write_header", lang)
            assert has_translation("figures.no_hits", lang)

    def test_missing_key(self):
        assert has_translation("nonexistent.key", "de") is False

    def test_all_messages_cover_supported_languages(self):
        """Every message must have entries for each supported language
        (otherwise the fallback to DEFAULT quietly hides missing
        translations)."""
        for key, entry in MESSAGES.items():
            for lang in SUPPORTED_LANGUAGES:
                assert lang in entry, f"missing {lang} for key {key}"


class TestDefaults:
    def test_default_language_is_supported(self):
        assert DEFAULT_LANGUAGE in SUPPORTED_LANGUAGES

    def test_supported_languages_contains_at_least_de_en(self):
        assert "de" in SUPPORTED_LANGUAGES
        assert "en" in SUPPORTED_LANGUAGES
