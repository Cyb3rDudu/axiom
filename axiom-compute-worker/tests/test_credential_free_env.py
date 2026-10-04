"""DM07 #316 — the compute worker's credential-free startup guard.

The worker talks HTTP only; a worker environment carrying one of the
known credential variable names refuses to boot, naming KEYS, never
values (the no-env-dump rule).
"""

from __future__ import annotations

import os

import pytest

from axiom_compute_worker.config import CREDENTIAL_ENV_NAMES, assert_credential_free_env


def _scrub() -> None:
    for name in CREDENTIAL_ENV_NAMES:
        os.environ.pop(name, None)


def test_clean_env_boots(monkeypatch):
    _scrub()
    assert_credential_free_env()  # no exception: the contract shape


def test_credential_present_refuses_with_name_not_value(monkeypatch):
    _scrub()
    secret_value = "postgresql://u:do-not-print@h/db"
    monkeypatch.setenv("AXIOM_DATABASE_URL", secret_value)
    with pytest.raises(SystemExit) as exc:
        assert_credential_free_env()
    msg = str(exc.value)
    assert "AXIOM_DATABASE_URL" in msg
    assert "do-not-print" not in msg
    assert "credential-free" in msg


def test_every_denied_name_is_checked(monkeypatch):
    for name in CREDENTIAL_ENV_NAMES:
        _scrub()
        monkeypatch.setenv(name, "x")
        with pytest.raises(SystemExit):
            assert_credential_free_env()
