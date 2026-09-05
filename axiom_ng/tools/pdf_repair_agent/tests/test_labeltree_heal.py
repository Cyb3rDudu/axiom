"""#253: deterministische Stelle-1-Heilung der Label-Tree-Defektklasse.

Mutation-bars:
- Klassifikations-Regel gekappt (class immer None) -> Would-Heal-Test rot
- Darstellbarkeits-Gate gekappt -> nicht-darstellbares Mapping würde
  fälschlich heilen -> rot
"""
import json
import sys
from pathlib import Path

import pymupdf

HERE = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(HERE))

from tools import labeltree_heal


def _missing_tree_pdf(path: Path, n=6, folio_from=1):
    doc = pymupdf.open()
    for i in range(n):
        pg = doc.new_page()
        if i == 0:
            pg.insert_text((72, 72), "Title Page: A Study")
        else:
            pg.insert_text((72, 72), f"chapter text {i} " + "lorem ipsum dolor " * 30)
            pg.insert_text((72, 800), str(folio_from + i - 1))
    doc.save(path)
    doc.close()
    return path


def _empty_stump_pdf(path: Path, n=4):
    p = _missing_tree_pdf(path, n)
    doc = pymupdf.open(p)
    doc.set_page_labels([{"startpage": 0, "prefix": ""}])  # leerer Strunk
    doc.saveIncr()
    doc.close()
    return p


class TestClassification:
    def test_missing_tree_with_textlayer_is_class(self, tmp_path):
        p = _missing_tree_pdf(tmp_path / "m.pdf")
        d = labeltree_heal.diagnose(p)
        assert d["class"] == "labeltree-missing"
        assert d["tree_state"] == "missing"
        assert d["text_layer"] is True

    def test_empty_stump_is_class(self, tmp_path):
        p = _empty_stump_pdf(tmp_path / "s.pdf")
        assert labeltree_heal.label_tree_state(p) == "empty"
        assert labeltree_heal.diagnose(p)["class"] == "labeltree-missing"

    def test_present_tree_is_not_class(self, tmp_path):
        p = _missing_tree_pdf(tmp_path / "ok.pdf")
        doc = pymupdf.open(p)
        doc.set_page_labels([{"startpage": 0, "prefix": "", "style": "D"}])
        doc.saveIncr()
        doc.close()
        assert labeltree_heal.diagnose(p)["class"] is None

    def test_no_textlayer_is_not_class(self, tmp_path):
        p = tmp_path / "scan.pdf"
        doc = pymupdf.open()
        for _ in range(4):
            doc.new_page()  # blank pages: no text layer
        doc.save(p)
        doc.close()
        assert labeltree_heal.diagnose(p)["class"] is None

    def test_would_heal_labels_from_folios(self, tmp_path):
        p = _missing_tree_pdf(tmp_path / "w.pdf")
        w = labeltree_heal.would_heal(p)
        assert w["would_heal"] is True, w
        assert w["op"] == "write_labels"
        assert w["labels"] == ["", "1", "2", "3", "4", "5"]

    def test_non_representable_mapping_no_heal(self, tmp_path):
        """Folios mit Sprung im Körper -> Mapping nicht darstellbar -> kein
        would-heal (fällt an den Agenten, nie falsch befüllt)."""
        p = tmp_path / "jump.pdf"
        doc = pymupdf.open()
        for i in range(4):
            pg = doc.new_page()
            pg.insert_text((72, 72), f"body {i} " + "lorem " * 20)
            # folios 2,3,8,9 — Sprung nach Seite 2
            pg.insert_text((72, 800), str([2, 3, 8, 9][i]))
        doc.save(p)
        doc.close()
        w = labeltree_heal.would_heal(p)
        assert w["would_heal"] is False
        assert w["reason"] == "folio-mapping-not-representable"


class TestRunAgentFastPath:
    """run_agent heilt die Klasse deterministisch — ohne Modell, ohne
    Stelle-2/3-Vorbedingung (client=None reicht als Beweis: der Fast-Path
    läuft VOR der NO-MODEL-Prüfung)."""

    def _cfg(self, tmp_path):
        from config import Config

        return Config(
            zotero_storage_root=tmp_path / "storage",
            rag_api_base="http://127.0.0.1:9",
            database_url="",
            deepseek_api_key="",
            deepseek_base_url="",
            model="",
            backup_root=tmp_path / "backups",
            work_root=tmp_path / "runs",
            budget_max_ops=10,
            budget_max_seconds=0,
            lang_profiles=["de"],
            probe_write=False,
            provenance={},
        )

    def test_dry_run_reports_would_heal(self, tmp_path):
        cfg = self._cfg(tmp_path)
        (cfg.zotero_storage_root / "K1").mkdir(parents=True)
        _missing_tree_pdf(cfg.zotero_storage_root / "K1" / "doc.pdf")
        import repair_agent as ra

        report = ra.run_agent("K1", apply=False, client=None, cfg=cfg)
        assert report["verdict"] == "report"
        assert report["catalog_rule"].startswith("labeltree-missing")
        assert report["final_step"]["plan_class"] == "labeltree-missing"

    def test_apply_heals_without_model(self, tmp_path):
        cfg = self._cfg(tmp_path)
        (cfg.zotero_storage_root / "K2").mkdir(parents=True)
        _missing_tree_pdf(cfg.zotero_storage_root / "K2" / "doc.pdf")
        import repair_agent as ra

        report = ra.run_agent("K2", apply=True, client=None, cfg=cfg)
        assert report["verdict"] == "healed", json.dumps(report, default=str)[:400]
        healed = cfg.work_root / "K2" / "work.pdf"
        got = pymupdf.open(healed)
        labels = [got[i].get_label() for i in range(got.page_count)]
        got.close()
        assert labels == ["", "1", "2", "3", "4", "5"], labels
