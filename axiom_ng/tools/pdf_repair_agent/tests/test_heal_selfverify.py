"""#258: Heal-Selbstverifikation — Readback-Beweis Pflicht.

Ein Upload ohne Readback-Beweis ist strukturell unmöglich:

  1. surgery_exec verweigert No-Op-Pläne (labels komplett leer) bereits
     in der VALIDIERUNG und nimmt den Beweis (tree + nicht-leere Labels)
     als `heal_readback` in jede Op-Evidenz auf.
  2. repair_agent trägt den Beweis in jeden Report (`heal_readback`) und
     ENTFERNT ein unbewiesenes work.pdf — der Invoker lädt genau dieses
     Artefakt nach Exit 0 hoch; ohne Beweis bleibt es nicht bestehen.

Mutation-Balken:
  - Readback-Gate in repair_agent gekappt -> No-Op-Heil-Test ROT
    (work.pdf bliebe als falsches Artefakt liegen = Upload-Pfad offen)
  - No-Op-Verweigerung in surgery_exec gekappt -> Validierungs-Test ROT
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from config import load_config
from deepseek_client import MockClient
from repair_agent import run_agent
from tools import surgery_exec  # type: ignore[reportAttributeAccessIssue]


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


class TestSurgeryReadbackProof:
    def test_noop_plan_refused_at_validation(self, tmp_path):
        """Eine Heilung, die keine Seite benennt, ist ein No-Op — der
        Plan wird strukturell verweigert (kein Schreibpfad dafür)."""
        src = _missing_tree_pdf(tmp_path / "m.pdf")
        ok, err = surgery_exec.validate(
            {
                "operations": [
                    {
                        "op": "write_labels",
                        "source": str(src),
                        "backup": str(tmp_path / "b.pdf"),
                        "labels": [""] * 6,
                    }
                ]
            }
        )
        assert ok is False
        assert "No-Op" in err

    def test_applied_op_carries_heal_readback_evidence(self, tmp_path):
        src = _missing_tree_pdf(tmp_path / "m.pdf")
        labels = ["", "1", "2", "3", "4", "5"]
        res = surgery_exec.run_plan(
            {
                "operations": [
                    {
                        "op": "write_labels",
                        "source": str(src),
                        "backup": str(tmp_path / "b.pdf"),
                        "labels": labels,
                        "expected_after": labels,
                    }
                ]
            },
            apply=True,
        )
        assert res["applied"] is True
        op = res["operations"][0]
        assert op["heal_readback"]["tree"] is True
        assert op["heal_readback"]["labels_sample"] == ["1", "2", "3"]


class TestAgentGate:
    """Die finale Pforte: Exit-0-Läufe ohne Readback-Beweis dürfen kein
    work.pdf hinterlassen (der Invoker würde es als Heilung hochladen)."""

    def _cfg(self, tmp_path):
        cfg = load_config({})
        cfg.work_root = tmp_path / "runs"
        cfg.backup_root = tmp_path / "backup"  # Hermetizität: nie ~/.local
        cfg.zotero_storage_root = tmp_path / "storage"
        cfg.ensure_dirs()
        return cfg

    def test_healed_report_carries_readback_proof(self, tmp_path):
        """Fast-Path-Heilung: verdict healed NUR mit Beweis-Feld."""
        cfg = self._cfg(tmp_path)
        att = cfg.zotero_storage_root / "KEYHEAL1"
        att.mkdir(parents=True)
        _missing_tree_pdf(att / "book.pdf")
        rep = run_agent(
            "KEYHEAL1",
            apply=True,
            client=MockClient(['{"action":"report","reason":"x"}']),
            cfg=cfg,
        )
        assert rep["verdict"] == "healed", json.dumps(rep, default=str)[:400]
        assert rep["heal_readback"]["tree"] is True
        assert rep["heal_readback"]["labels_sample"] == ["1", "2", "3"]
        assert (cfg.work_root / "KEYHEAL1" / "work.pdf").exists()

    def test_unproven_run_removes_artifact_no_upload_path(self, tmp_path):
        """MUTATION-BALKEN (#258): kein Folio, keine Heilung, Modell
        haltet — der Lauf darf KEIN hochladbares work.pdf hinterlassen
        und keinen healed-Verdict führen. Gate gekappt -> Test ROT."""
        cfg = self._cfg(tmp_path)
        # Scan ohne Textschicht: Fast-Path verweigert (keine Textschicht),
        # Mock-Client haltet ohne Schreib-Step -> nichts Geheiltes.
        att = cfg.zotero_storage_root / "KEYHALT1"
        att.mkdir(parents=True)
        doc = pymupdf.open()
        for _ in range(4):
            doc.new_page()
        doc.save(att / "scan.pdf")
        doc.close()
        rep = run_agent(
            "KEYHALT1",
            apply=True,
            client=MockClient(['{"action":"report","reason":"nicht messbar"}']),
            cfg=cfg,
        )
        assert rep["verdict"] == "halt"
        assert rep["heal_readback"] is None
        assert not (cfg.work_root / "KEYHALT1" / "work.pdf").exists(), (
            "unbewiesenes work.pdf wäre der Upload-Pfad eines No-Op-Heils"
        )
        # Storage-Original unberührt (Arbeitskopie-Disziplin bleibt).
        assert (att / "scan.pdf").exists()

    def test_unproven_fast_path_removes_artifact_no_upload_path(self, tmp_path, monkeypatch):
        """MUTATION-BALKEN (#258) für den FAST-PATH: write_labels crasht
        nach dem Backup — die Op wird zurückgerollt (applied=False), der
        Readback-Beweis fehlt. Der Lauf muss halten UND das zurückgerollte
        work.pdf ENTFERNEN (Exit 0 + Artefakt wäre der Upload-Pfad).
        Fast-Path-unlink gekappt -> Test ROT."""
        from tools import pdf_kernel  # type: ignore[reportAttributeAccessIssue]

        def _boom(pdf, labels):
            raise RuntimeError("kernel write failed")

        monkeypatch.setattr(pdf_kernel, "write_page_labels", _boom)
        cfg = self._cfg(tmp_path)
        att = cfg.zotero_storage_root / "KEYFAIL1"
        att.mkdir(parents=True)
        _missing_tree_pdf(att / "book.pdf")
        rep = run_agent("KEYFAIL1", apply=True, client=None, cfg=cfg)
        assert rep["verdict"] == "halt", json.dumps(rep, default=str)[:400]
        assert "abgelehnt" in rep["final_step"]["reason"]
        assert rep["heal_readback"] is None
        assert not (cfg.work_root / "KEYFAIL1" / "work.pdf").exists(), (
            "unbewiesenes work.pdf wäre der Upload-Pfad eines No-Op-Heils"
        )
        # Storage-Original unberührt (Arbeitskopie-Disziplin bleibt).
        assert (att / "book.pdf").exists()

    def test_dry_run_keeps_artifact(self, tmp_path):
        """Ohne --apply gibt es keinen Upload-Pfad — die Pforte greift
        nicht (Dry-Run-Analysen dürfen die Arbeitskopie behalten)."""
        cfg = self._cfg(tmp_path)
        att = cfg.zotero_storage_root / "KEYDRY1"
        att.mkdir(parents=True)
        doc = pymupdf.open()
        for _ in range(4):
            doc.new_page()
        doc.save(att / "scan.pdf")
        doc.close()
        rep = run_agent(
            "KEYDRY1",
            apply=False,
            client=MockClient(['{"action":"report","reason":"x"}']),
            cfg=cfg,
        )
        assert rep["verdict"] == "halt"
        assert (cfg.work_root / "KEYDRY1" / "work.pdf").exists()
