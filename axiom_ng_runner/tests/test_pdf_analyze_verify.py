"""#184/verify — the pure plan-check decision core (no PDF stack needed).

Pins review E1: a plan key that is NOT a folio-verified page is a kill
(MISMATCH), not a vacuous pass — the old code skipped unknown keys, so an
off-by-one plan ({12:'4'} against 0-based truth) and 0-based plans passed
without being checked at all.
"""
from axiom_ng_runner.scripts.pdf_analyze import check_plan_against_folio

# folio truth: 0-based physical page -> printed folio (Altenburger shape:
# physical 11 prints folio 4 — dudu's verified gold page)
TRUTH = {10: "3", 11: "4", 12: "5", 154: "151"}


def test_correct_plan_passes():
    r = check_plan_against_folio({"11": "3", "12": "4", "13": "5"}, TRUTH)  # 1-based keys
    assert r["accepted"] and not r["killed"] and r["geprueft_gegen"] == 4


def test_contradiction_kills():
    r = check_plan_against_folio({"12": "99"}, TRUTH)
    assert not r["accepted"]
    assert r["killed"][0]["grund"] == "widerspricht folio"
    assert r["killed"][0]["folio_wahrheit"] == "4"


def test_unknown_page_kills_not_passes():  # E1 — the old code skipped these
    r = check_plan_against_folio({"999": "7"}, TRUTH)
    assert not r["accepted"]
    assert r["killed"][0]["grund"] == "seite nicht folio-verifiziert"


def test_convention_mismatch_kills():  # author used 0-based keys (folio of
    # physical 12 is "5"), checker checks the 1-based page (folio "4") — the
    # mismatch surfaces as a contradiction kill, never a vacuous pass
    r = check_plan_against_folio({"12": "5"}, TRUTH)
    assert not r["accepted"]
    assert r["killed"][0]["grund"] == "widerspricht folio"
    assert r["killed"][0]["folio_wahrheit"] == "4"


def test_non_numeric_key_kills():
    r = check_plan_against_folio({"xii": "4"}, TRUTH)
    assert not r["accepted"]
    assert r["killed"][0]["grund"] == "plan-key ist keine Seitenzahl"


def test_empty_truth_kills_everything():
    r = check_plan_against_folio({"12": "4"}, {})
    assert not r["accepted"] and r["killed"][0]["grund"] == "seite nicht folio-verifiziert"
