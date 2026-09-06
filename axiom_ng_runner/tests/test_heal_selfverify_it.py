"""#258 IT: the two production heal-loop files heal END-TO-END with a
readback proof, and preflight goes GREEN on the healed copy.

Both fixtures are the REAL production files (pinned copies):
  - plantin_2018_infrastructure_studies.pdf — running-head folios at
    ~75-76% page height; owner-verified offset print = pdf - 1
  - intoto_2019_torres_arias_stump.pdf — catalog carries an EMPTY tree
    stump (<</Nums[0<</P()>>>>): the heal must REPLACE the key, the
    USENIX folio runs (1393..1410) recover via the #254 harvest discipline

Custody chain per fixture (the issue's acceptance):
    would_heal (folio harvest = #254 discipline) -> surgery write ->
    heal_readback proof (tree present, labels non-empty) -> preflight
    GREEN on the healed copy.

All paths resolve REPO-RELATIVE (#233 hermeticity).

Mutation bars:
  - readback gate cut (surgery/repair_agent) -> proof asserts RED
  - folio harvest narrowed (old 18%/82% bands)  -> Plantin RED
  - single-run-only harvest (old bare-cell)    -> in-toto RED
"""
import shutil
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent  # repo root
FIXER_ROOT = REPO_ROOT / "axiom_ng" / "tools" / "pdf_repair_agent"
sys.path.insert(0, str(FIXER_ROOT))  # fixer tools (pymupdf-only imports)

from tools import folio_harvest, labeltree_heal, surgery_exec

TESTDATA = Path(__file__).parent / "testdata"
PLANTIN = TESTDATA / "plantin_2018_infrastructure_studies.pdf"
INTOTO = TESTDATA / "intoto_2019_torres_arias_stump.pdf"


def _heal(src: Path, tmp_path: Path) -> dict:
    """The production fast path on a working copy: plan -> apply -> proof."""
    work = tmp_path / "work.pdf"
    shutil.copy2(src, work)
    verdict = labeltree_heal.would_heal(work)
    assert verdict["would_heal"] is True, verdict.get("reason")
    labels = verdict["labels"]
    res = surgery_exec.run_plan(
        {
            "class": "labeltree-missing",
            "operations": [
                {
                    "op": "write_labels",
                    "source": str(work),
                    "backup": str(tmp_path / "backup.pdf"),
                    "labels": labels,
                    "expected_after": labels,
                }
            ],
        },
        apply=True,
    )
    assert res["applied"] is True, str(res)[:400]
    return {"work": work, "labels": labels, "res": res}


def _assert_green(work: Path) -> None:
    from axiom_ng_runner.compute_core.pdf_health import preflight

    pf = preflight(str(work))
    assert pf.ok is True, f"healed copy must preflight GREEN, got {pf.finding} — {pf.reason}"


def test_plantin_heals_via_running_head_folios(tmp_path):
    """Owner-verified: print = pdf - 1 — the +1 folio run sits in the
    mid-page running head (~75%), invisible to the old narrow bands."""
    assert PLANTIN.exists(), "plantin fixture must be pinned in testdata"
    out = _heal(PLANTIN, tmp_path)
    # labels: page 0 unnamed, then the closed +1 run 1..25 (print = pdf-1)
    assert out["labels"] == [""] + [str(i) for i in range(1, 26)]
    op = out["res"]["operations"][0]
    assert op["heal_readback"]["tree"] is True
    assert op["heal_readback"]["labels_sample"] == ["1", "2", "3"]
    _assert_green(out["work"])


def test_intoto_stump_is_replaced_and_heals(tmp_path):
    """The existing-but-empty tree stump must be REPLACED (key overwrite,
    not set-if-absent): tree state empty -> present, USENIX folio runs
    1393..1410 recovered through the #254 multi-form harvest."""
    assert INTOTO.exists(), "in-toto fixture must be pinned in testdata"
    work = tmp_path / "stump.pdf"
    shutil.copy2(INTOTO, work)
    assert labeltree_heal.label_tree_state(work) == "empty"

    out = _heal(INTOTO, tmp_path)
    # folio runs: pages 2-7 -> 1393-1398, 10-12 -> 1401-1403,
    # 14-19 -> 1405-1410 — one consistent +1 mapping from page 1
    assert out["labels"] == [""] + [str(1392 + i) for i in range(1, 19)]
    assert labeltree_heal.label_tree_state(out["work"]) == "present"
    op = out["res"]["operations"][0]
    assert op["heal_readback"]["tree"] is True
    _assert_green(out["work"])


def test_vendored_harvest_stays_in_sync_with_page_trust():
    """#258 sync guard: the fixer's vendored harvest mirror must be
    BEHAVIOR-IDENTICAL to the source of truth (page_trust) on both
    production fixtures — drift here re-opens the heal-loop (repairable
    by preflight but not healable by the fixer)."""
    import pymupdf
    from axiom_ng_runner.compute_core import page_trust

    for fx in (PLANTIN, INTOTO):
        d1 = pymupdf.open(str(fx))
        try:
            src = page_trust.extract_folio_candidates(d1)
        finally:
            d1.close()
        d2 = pymupdf.open(str(fx))
        try:
            mirror = folio_harvest.extract_folio_candidates(d2)
        finally:
            d2.close()
        assert src == mirror, (
            f"vendored folio_harvest drifted from page_trust on {fx.name}"
        )
