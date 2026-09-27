"""F01 remainder redeemed (F10 #304): the pinned compute-worker block in
the Go baseline's ``api_inventory.txt`` is cross-witnessed against the
REAL FastAPI router.

The Go suite cannot import the Python app, so its :8112 route list stays a
freeze-generation pin — but since F10 the pin has teeth in BOTH directions:
this test derives the METHOD+PATH set from the real ``app`` and compares it
against the pinned block. A route added/renamed/removed on either side goes
red in the other suite (the response-class annotations stay Go-side — they
are not machine-derivable without invoking every handler).

Skips when the Go fixture is absent (artifact-only checkouts).
"""

from __future__ import annotations

from pathlib import Path

import pytest

from axiom_compute_worker.app import app

FIXTURE = (
    Path(__file__).resolve().parents[2]
    / "axiom_ng"
    / "internal"
    / "baseline"
    / "fixtures"
    / "api_inventory.txt"
)


def _pinned_routes() -> set[tuple[str, str]]:
    lines = FIXTURE.read_text(encoding="utf-8").splitlines()
    start = next(
        i for i, ln in enumerate(lines) if ln.startswith("# compute worker")
    )
    out: set[tuple[str, str]] = set()
    for ln in lines[start + 1 :]:
        if not ln.strip():
            continue
        method, path = ln.split()[:2]
        out.add((method, path))
    return out


# FastAPI's framework-added documentation routes — present on the real
# router but never part of the frozen contract surface (the F01 pin
# deliberately excluded them; they are disabled in production shapes).
_FRAMEWORK_ROUTES = {"/docs", "/docs/oauth2-redirect", "/openapi.json", "/redoc"}


def _real_routes() -> set[tuple[str, str]]:
    out: set[tuple[str, str]] = set()
    for r in app.routes:
        methods = sorted(getattr(r, "methods", None) or [])
        path = getattr(r, "path", None)
        if path is None or path in _FRAMEWORK_ROUTES:
            continue
        # HEAD/OPTIONS are framework-added shadows of GET — the Go pin lists
        # the contract surface only.
        for m in methods:
            if m in ("HEAD", "OPTIONS"):
                continue
            out.add((m, path))
    return out


@pytest.mark.skipif(not FIXTURE.is_file(), reason="Go baseline fixture not present")
def test_pinned_inventory_block_matches_real_router():
    pinned = _pinned_routes()
    real = _real_routes()
    assert pinned, "fixture block empty — parser or fixture drifted"
    assert real, "FastAPI app exposes no routes — derivation broke"
    assert pinned == real, (
        f"api_inventory.txt compute-worker block drifted from the real "
        f"router: pinned-only={sorted(pinned - real)} "
        f"real-only={sorted(real - pinned)}"
    )
