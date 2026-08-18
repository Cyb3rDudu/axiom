"""#184 review W5 — pure-logic tests for the fix-service plan world.

validate_plan / label_at / candidates_1based are the mechanical core that
turns a judge plan into healed bytes; these tests run WITHOUT pymupdf,
requests or compute_core (service.py keeps those imports function-local).
Run: python3 service_test.py  (or pytest service_test.py)
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import service  # path shim above


def test_validate_plan_rejects_overlap() -> None:
    plan = {"sections": [
        {"from_page": 11, "to_page": 67, "start_label": 2},
        {"from_page": 60, "to_page": 90, "start_label": 51},  # overlap
    ]}
    try:
        service.validate_plan(plan)
    except ValueError:
        return
    raise AssertionError("überlappung muss abgelehnt werden")


def test_validate_plan_rejects_inverted_range() -> None:
    # descending from_page across sections is NORMALIZED by sorting (pinned
    # below); the reject case is an inverted section: to_page < from_page
    plan = {"sections": [
        {"from_page": 50, "to_page": 40, "start_label": 1},
    ]}
    try:
        service.validate_plan(plan)
    except ValueError:
        return
    raise AssertionError("invertierte sektion (to < from) muss abgelehnt werden")


def test_validate_plan_rejects_missing_int() -> None:
    for bad in ({"from_page": "11", "to_page": 67, "start_label": 2},
                {"from_page": 11, "to_page": 67},
                {"to_page": 67, "start_label": 2}):
        try:
            service.validate_plan({"sections": [bad]})
        except ValueError:
            continue
        raise AssertionError(f"fehlerhafter sektions-eintrag muss abgelehnt werden: {bad}")


def test_validate_plan_normalizes_and_defaults() -> None:
    plan = {"sections": [
        {"from_page": 40, "to_page": 50, "start_label": 9},
        {"from_page": 11, "to_page": 20, "start_label": 1},  # unsorted input
    ]}
    out = service.validate_plan(plan)
    assert [s["from_page"] for s in out["sections"]] == [11, 40]  # sorted
    assert out["gaps"] == [] and out["confidence"] == 0  # defaults


def test_label_at_boundaries() -> None:
    plan = {"sections": [
        {"from_page": 11, "to_page": 13, "start_label": 2},
        {"from_page": 20, "to_page": 22, "start_label": 30},
    ]}
    assert service.label_at(plan, 11) == 2  # section start
    assert service.label_at(plan, 12) == 3  # +1/page
    assert service.label_at(plan, 13) == 4  # section end
    assert service.label_at(plan, 10) is None  # before (cover)
    assert service.label_at(plan, 14) is None  # gap between sections
    assert service.label_at(plan, 22) == 32  # second section end
    assert service.label_at(plan, 23) is None


def test_candidates_1based_shifts_and_filters() -> None:
    cands = {10: "11", 11: "12", 12: "xii", 13: " 14 ", 14: ""}
    out = service.candidates_1based(cands)
    # 0-based key 10 -> 1-based page 11; non-arabic readings dropped;
    # values stripped
    assert out == {11: "11", 12: "12", 14: "14"}
    # the historical bug this pins: judging with 0-based keys builds every
    # section one page early (Controlling: 573/575 contradictions)
    assert 10 not in out and 13 not in out


if __name__ == "__main__":
    fails = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except AssertionError as exc:
                fails += 1
                print(f"FAIL {name}: {exc}")
    sys.exit(1 if fails else 0)
