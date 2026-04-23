"""Tests for the feature-flag resolver used by structured bibliography."""

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

from services.feature_flags import (  # noqa: E402
    STRUCTURED_BIBLIOGRAPHY_ENV,
    resolve_flag_for_logging,
    structured_bibliography_enabled,
)


class TestStructuredBibliographyFlag:
    def test_default_off(self, monkeypatch):
        monkeypatch.delenv(STRUCTURED_BIBLIOGRAPHY_ENV, raising=False)
        assert structured_bibliography_enabled(None) is False
        assert structured_bibliography_enabled({}) is False
        assert structured_bibliography_enabled({"writing_settings": {}}) is False

    def test_user_opt_in_without_env(self, monkeypatch):
        monkeypatch.delenv(STRUCTURED_BIBLIOGRAPHY_ENV, raising=False)
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        assert structured_bibliography_enabled(settings) is True

    def test_env_kill_switch_overrides_user_opt_in(self, monkeypatch):
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, "false")
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        assert structured_bibliography_enabled(settings) is False

    def test_env_true_alone_does_not_enable(self, monkeypatch):
        """Opt-in is per-user. Setting the env true on its own stays dark."""
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, "true")
        assert structured_bibliography_enabled(None) is False
        assert structured_bibliography_enabled({"writing_settings": {}}) is False

    def test_env_true_with_user_opt_in_enables(self, monkeypatch):
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, "true")
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        assert structured_bibliography_enabled(settings) is True

    def test_resolve_reports_both_inputs(self, monkeypatch):
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, "true")
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        out = resolve_flag_for_logging(settings)
        assert out == {"env": True, "user": True, "resolved": True}

    def test_non_boolean_env_ignored(self, monkeypatch):
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, "maybe")
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        # Garbage env value treated as absent → user flag wins
        assert structured_bibliography_enabled(settings) is True

    @pytest.mark.parametrize("falsy", ["0", "FALSE", "no", "off"])
    def test_various_falsy_strings(self, monkeypatch, falsy):
        monkeypatch.setenv(STRUCTURED_BIBLIOGRAPHY_ENV, falsy)
        settings = {"writing_settings": {"structured_bibliography_enabled": True}}
        assert structured_bibliography_enabled(settings) is False
