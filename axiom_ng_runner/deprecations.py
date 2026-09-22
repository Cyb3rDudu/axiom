"""Central legacy-usage witness (ADR 0001 §6, #296).

Twin of the Go ``internal/deprecate`` package: legacy entrypoints stay
functional through 0.2.x; their use is warned about exactly once per
process and counted on every call. Counters are exported via
``/v1/capabilities`` (``deprecations``) as the data basis for the 0.3.x+
removal decision. F02 ships the mechanism only — the legacy
entrypoints arrive with F05/F10 and call :func:`use` from their
aliases.
"""

from __future__ import annotations

import logging
import threading

_log = logging.getLogger("axiom_ng_runner.deprecations")
_lock = threading.Lock()
_warned: set[str] = set()
_counts: dict[str, int] = {}
_silent = False


def set_silent(v: bool) -> None:
    """Suppress the warning line (tests); counting continues either way."""
    global _silent
    with _lock:
        _silent = v


def use(name: str) -> None:
    """Record one use of legacy identifier ``name``.

    Warns exactly once per process per name (once-semantics, not per
    call); increments the counter on every call.
    """
    global _warned, _counts
    with _lock:
        first = name not in _warned
        _warned.add(name)
        _counts[name] = _counts.get(name, 0) + 1
        silent = _silent
    if first and not silent:
        _log.warning(
            "deprecated name %r used (ADR 0001: docs/adr/0001-canonical-naming.md)",
            name,
        )


def counts() -> dict[str, int]:
    """Copy of the per-name usage counters (capabilities export)."""
    with _lock:
        return dict(_counts)


def _reset() -> None:
    """Clear all state (test isolation only)."""
    global _warned, _counts
    with _lock:
        _warned = set()
        _counts = {}
