"""Unit tests for the legacy-usage witness (ADR 0001 §6, #296).

Twin of the Go internal/deprecate tests: warn exactly once per process
per name, count every call, silenceable for tests.
"""

from __future__ import annotations

import logging

import axiom_ng_runner.deprecations as dep

LOGGER = "axiom_ng_runner.deprecations"


def test_warns_once_counts_always(caplog):
    dep._reset()
    with caplog.at_level(logging.WARNING, logger=LOGGER):
        for _ in range(100):
            dep.use("axiom-ng")
    warns = [r for r in caplog.records if "axiom-ng" in r.message]
    assert len(warns) == 1, "warning must fire exactly once per process per name"
    assert dep.counts() == {"axiom-ng": 100}, "counter must count every call"


def test_second_name_warns_again(caplog):
    dep._reset()
    with caplog.at_level(logging.WARNING, logger=LOGGER):
        dep.use("axiom-ng")
        dep.use("axiom-fixer")
    assert len(caplog.records) == 2, "once per NAME, not once globally"


def test_silent_suppresses_warning_only(caplog):
    dep._reset()
    dep.set_silent(True)
    try:
        with caplog.at_level(logging.WARNING, logger=LOGGER):
            dep.use("old-name")
    finally:
        dep.set_silent(False)
    assert caplog.records == [], "silent mode must not log"
    assert dep.counts() == {"old-name": 1}, "silent mode must keep counting"
