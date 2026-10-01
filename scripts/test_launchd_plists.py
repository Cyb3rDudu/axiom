#!/usr/bin/env python3
"""test_launchd_plists.py — structural gate for the launchd templates.

Enforces the QoS scheduling policy from the 2026-09-21 diagnostics
(docs/diagnostics/2026-09-21-ocrmypdf-background-qos.md): every service
that spawns CPU-bound work must ship ProcessType=Standard; Background is
allowed only for the documented no-CPU-children exception (carrier-bridge).

Also pins the installer contract: Standard*Path keeps the __HOME__
placeholder (scripts/install_services.sh substitutes it; launchd never
expands $HOME there).

Runs on any host with python3 (plistlib — no macOS tools needed, so CI
on Linux can enforce it). Exit 0 = policy holds.
"""

import sys
from pathlib import Path
import plistlib

LAUNCHD = Path(__file__).resolve().parent.parent / "deploy" / "launchd"

# label suffix -> must be Standard (CPU-bound or spawns CPU-bound children)
STANDARD_REQUIRED = {
    "com.axiom.rag",
    "com.axiom.compute-worker",
    "com.axiom.rag-dispatch-gpu0",
    "com.axiom.rag-dispatch-gpu1",
    "com.axiom.rag-dispatch-gpu2",
}

# The deliberate Background exception: pure network forwarder, no
# CPU-bound children (README §Scheduling (QoS)).
BACKGROUND_ALLOWED = {"com.axiom.carrier-bridge"}


def main() -> int:
    plists = sorted(LAUNCHD.glob("*.plist"))
    if not plists:
        print("FAIL: no plists found under deploy/launchd")
        return 1
    failures: list[str] = []
    seen: set[str] = set()
    for path in plists:
        with path.open("rb") as fh:
            try:
                doc = plistlib.load(fh)
            except Exception as exc:  # noqa: BLE001 - report and fail
                failures.append(f"{path.name}: does not parse ({exc})")
                continue
        label = doc.get("Label", "")
        seen.add(label)
        ptype = doc.get("ProcessType", "")
        if not ptype:
            failures.append(f"{path.name}: no ProcessType key")
            continue
        if label in STANDARD_REQUIRED and ptype != "Standard":
            failures.append(
                f"{path.name} ({label}): ProcessType={ptype}, want Standard "
                "(CPU-bound children inherit the service coalition — "
                "diagnostics 2026-09-21)"
            )
        if ptype == "Background" and label not in BACKGROUND_ALLOWED:
            failures.append(
                f"{path.name} ({label}): ProcessType=Background without a "
                "documented exception (README §Scheduling (QoS))"
            )
        for key in ("StandardOutPath", "StandardErrorPath"):
            val = doc.get(key, "")
            if val and "__HOME__" not in val:
                failures.append(f"{path.name}: {key} lost the __HOME__ placeholder: {val}")
    missing = STANDARD_REQUIRED - seen
    for label in sorted(missing):
        failures.append(f"required service template missing: {label}.plist")
    if failures:
        for f in failures:
            print(f"FAIL: {f}")
        return 1
    print(f"launchd plists: {len(plists)} templates, QoS policy holds "
          f"({len(STANDARD_REQUIRED)} Standard, {len(BACKGROUND_ALLOWED)} documented Background)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
