#!/usr/bin/env python3
"""#268 caption-matcher inventory: coverage claim over the checked-in forms.

Runs the REAL matcher (axiom_ng_runner.runner._iter_figure_captions) against
caption_inventory_forms.jsonl — every line is one real (anonymized)
production form labelled caption or prose — and reports how many captions
matched and how many prose forms were falsely paired. The claim in the issue
is reproducible here:

    pattern matches N of N inventoried caption forms, 0 of M prose forms

Exit non-zero when any caption form is missed or any prose form pairs, so the
script doubles as a regression gate (the same shapes the pytest matrix pins,
kept in one place so the DoD number can be re-derived at any time).

Usage: python scripts/caption_inventory.py [--verbose]
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

# Make the runner package importable when run from the repo (or the venv
# install — both resolve the same module).
sys.path.insert(0, str(Path(__file__).resolve().parent.parent.parent))

from axiom_ng_runner.runner import _iter_figure_captions

FORMS = Path(__file__).with_name("caption_inventory_forms.jsonl")


def load() -> list[dict[str, str]]:
    with FORMS.open(encoding="utf-8") as fh:
        return [json.loads(line) for line in fh if line.strip()]


def matched(form: str) -> bool:
    return next(_iter_figure_captions(form), None) is not None


def main() -> int:
    verbose = "--verbose" in sys.argv
    forms = load()
    captions = [f for f in forms if f["label"] == "caption"]
    prose = [f for f in forms if f["label"] == "prose"]

    hit = [f for f in captions if matched(f["form"])]
    missed = [f for f in captions if f not in hit]
    fp = [f for f in prose if matched(f["form"])]

    print(f"caption forms matched: {len(hit)}/{len(captions)}")
    print(f"prose forms falsely paired: {len(fp)}/{len(prose)}")
    for f in missed:
        print(f"  MISS [{f['axis']}] {f['form']!r}")
    for f in fp:
        print(f"  FALSE-POS [{f['axis']}] {f['form']!r}")
    if verbose:
        for f in hit:
            print(f"  ok [{f['axis']}] {f['form']!r}")

    if missed or fp:
        print("FAIL: coverage claim not met")
        return 1
    print(f"OK: pattern matches {len(hit)} of {len(captions)} inventoried "
          f"caption forms, 0 of {len(prose)} prose forms")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
