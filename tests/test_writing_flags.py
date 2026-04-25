"""Tests for WritingFlags resolution snapshot."""

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

from services.writing_flags import WritingFlags  # noqa: E402


class TestAllOff:
    def test_explicit_default(self):
        flags = WritingFlags.all_off()
        assert flags.structured_bibliography is False
        assert flags.wordcount_fix is False
        assert flags.sources_always is False
        assert flags.transparent_continuation is False
        assert flags.rag_figures is False

    def test_immutable(self):
        flags = WritingFlags.all_off()
        with pytest.raises(Exception):
            flags.structured_bibliography = True  # type: ignore[misc]


class TestResolve:
    def test_none_settings_resolves_to_all_off(self):
        # No env opt-ins, no user opt-ins → all flags off
        flags = WritingFlags.resolve(None)
        assert isinstance(flags, WritingFlags)

    def test_empty_settings_resolves_cleanly(self):
        flags = WritingFlags.resolve({})
        assert isinstance(flags, WritingFlags)

    def test_resolution_consults_feature_flags_layer(self, monkeypatch):
        from services import feature_flags

        monkeypatch.setattr(feature_flags, "structured_bibliography_enabled", lambda _s: True)
        monkeypatch.setattr(feature_flags, "wordcount_fix_enabled", lambda _s: True)
        monkeypatch.setattr(feature_flags, "sources_always_enabled", lambda _s: False)
        monkeypatch.setattr(feature_flags, "transparent_continuation_enabled", lambda _s: True)
        monkeypatch.setattr(feature_flags, "rag_figures_enabled", lambda _s: False)

        flags = WritingFlags.resolve({"any": "settings"})
        assert flags.structured_bibliography is True
        assert flags.wordcount_fix is True
        assert flags.sources_always is False
        assert flags.transparent_continuation is True
        assert flags.rag_figures is False
