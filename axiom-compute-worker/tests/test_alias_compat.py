"""axiom_ng_runner compat alias (ADR 0001 §4, F10 #304).

The alias must be RELIABLE for foreign deployments (remote GPU carriers):
``python -m axiom_ng_runner``, the ``axiom-runner`` console script and
``import axiom_ng_runner`` all delegate to the canonical
axiom-compute-worker, warn EXACTLY ONCE per process, and count every use
in the deprecation witness exported via /v1/capabilities.
"""

from __future__ import annotations

import importlib
import logging
import subprocess
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent


def test_alias_import_warns_once_and_reexports(caplog):
    # Fresh interpreter state per test run makes _warned stale across tests;
    # reset via the canonical witness so the once-semantics is observable.
    from axiom_compute_worker import deprecations

    deprecations._reset()
    with caplog.at_level(logging.WARNING, logger="axiom_compute_worker.deprecations"):
        import axiom_ng_runner  # noqa: F401

    # a second import statement is a sys.modules no-op — the once-semantics
    # holds regardless of how often the alias is imported.
    import axiom_ng_runner  # noqa: F401
    assert caplog.records, "alias import must warn"
    msgs = [r for r in caplog.records if "axiom_ng_runner" in r.message]
    assert len(msgs) == 1, f"exactly one warning per process, got {len(msgs)}"
    # delegation, not a fork: public identity re-exported from the canonical
    assert axiom_ng_runner.CANONICAL_NAME == "axiom-compute-worker"
    assert axiom_ng_runner.CONTRACT_VERSION == "1.0"
    # counted on every import (warn-once ≠ count-once)
    assert deprecations.counts().get("axiom_ng_runner", 0) >= 1


def test_alias_counter_probe_visible_in_capabilities():
    """The DoD counter probe: using the alias moves the exported
    deprecations counter that /v1/capabilities serves (order-independent:
    the delta moves, no absolute count)."""
    import axiom_ng_runner  # noqa: F401  (may be cached — no count)
    from axiom_compute_worker.app import _capabilities

    before = _capabilities().deprecations.get("axiom_ng_runner", 0)
    importlib.reload(axiom_ng_runner)  # a REAL alias use
    after = _capabilities().deprecations
    assert after.get("axiom_ng_runner", 0) == before + 1, (
        "every alias use must count in the capabilities export"
    )


def test_alias_module_entrypoint_delegates():
    """`python -m axiom_ng_runner` boots the REAL service (the remote-
    carrier entrypoint): warn-once on stderr, /v1/health serves under the
    canonical identity. A broken alias (ImportError) exits non-zero with a
    traceback instead."""
    import socket
    import urllib.request

    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()

    import os

    proc = subprocess.Popen(
        [sys.executable, "-m", "axiom_ng_runner"],
        cwd=str(PROJECT_ROOT),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=dict(os.environ, AXIOM_PROCESSOR_PORT=str(port), AXIOM_PROCESSOR_WARMUP="0"),
    )
    try:
        deadline = __import__("time").monotonic() + 30
        body = None
        while __import__("time").monotonic() < deadline:
            try:
                with urllib.request.urlopen(
                    f"http://127.0.0.1:{port}/v1/capabilities", timeout=2
                ) as r:
                    body = r.read().decode()
                    break
            except OSError:
                if proc.poll() is not None:
                    break
                __import__("time").sleep(0.2)
        assert body is not None, (
            f"alias entrypoint never served: rc={proc.poll()} "
            f"stderr={proc.stderr.read()[-400:] if proc.stderr else ''}"
        )
        assert '"canonical_name":"axiom-compute-worker"' in body.replace(" ", ""), body
    finally:
        proc.terminate()
        proc.wait(timeout=10)


def test_canonical_entrypoint_no_warning():
    """The canonical entrypoint is warning-free (the alias is the legacy
    path, not the other way around)."""
    from axiom_compute_worker import deprecations

    deprecations._reset()
    import axiom_compute_worker  # noqa: F401

    assert deprecations.counts() == {}
