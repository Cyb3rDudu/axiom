"""T4 surgery_exec — Fixture-Tests: PLAN-Validierung, Backup, Read-Back,
Rollback. KEIN Projekt-Kontakt: arbeitet auf Sandbox-Fixtures + runs/."""

from __future__ import annotations

import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from tools import (  # noqa: E402
  pdf_kernel,  # type: ignore[reportAttributeAccessIssue]
  surgery_exec,  # type: ignore[reportAttributeAccessIssue]
)

FIX = PKG / "fixtures"


def _working_copy() -> Path:
  """Sandbox-Kopie der forensik.pdf (labels ['','',3..16])."""
  dst = PKG / "runs" / "_surgery_test.pdf"
  dst.parent.mkdir(parents=True, exist_ok=True)
  shutil.copy2(FIX / "forensik.pdf", dst)
  return dst


def _heal_labels() -> list[str]:
  return [""] * 2 + [str(i) for i in range(1, 15)]


def _plan(
  src: Path, labels: list[str], expected=None, backup: str | None = None
) -> dict:
  return {
    "operations": [
      {
        "op": "write_labels",
        "source": str(src),
        "backup": backup if backup is not None else str(PKG / "runs" / "bak.pdf"),
        "labels": labels,
        "expected_after": expected,
      }
    ]
  }


def test_dry_run_schreibt_nichts():
  src = _working_copy()
  before = pdf_kernel.read_page_labels(src)
  r = surgery_exec.run_plan(_plan(src, _heal_labels()), apply=False)
  assert r["valid"] and r["dry_run"] and not r["applied"]
  after = pdf_kernel.read_page_labels(src)
  assert before == after, "Dry-Run darf Datei NICHT verändern"


def test_apply_heilt_und_backupt():
  src = _working_copy()
  labels = _heal_labels()
  r = surgery_exec.run_plan(_plan(src, labels, labels), apply=True)
  assert r["applied"] and r["operations"][0]["match_expected"]
  assert read_labels(src) == labels
  # Backup existiert und entspricht dem Original.
  assert Path(str(PKG / "runs" / "bak.pdf")).exists()


def test_laenge_falsch_ablehnung():
  """labels-Länge != page_count => Plan ungültig (kein Schreiben)."""
  src = _working_copy()
  r = surgery_exec.run_plan(_plan(src, ["1", "2"]), apply=True)
  assert r["valid"] is False
  assert "labels-Länge" in r["error"]


def test_c2_backup_fehlt_ablehnung():
  """C2-Regression: ohne backup-Pfad wäre copy2(src, '.') CWD-Müll und
  der Rollback hätte ein Verzeichnis als Quelle — Plan MUSS ungültig sein."""
  src = _working_copy()
  r = surgery_exec.run_plan(_plan(src, _heal_labels(), backup=""), apply=True)
  assert r["valid"] is False
  assert "backup" in r["error"]
  # Datei unangetastet.
  assert read_labels(src) == read_labels(FIX / "forensik.pdf")


def test_c2_backup_gleich_source_ablehnung():
  """C2-Regression: backup == source wäre keine Kopie, sondern die
  Änderung selbst — ungültig."""
  src = _working_copy()
  r = surgery_exec.run_plan(_plan(src, _heal_labels(), backup=str(src)), apply=True)
  assert r["valid"] is False
  assert "backup == source" in r["error"]


def test_c3_nicht_darstellbare_labels_ablehnung():
  """C3-Regression: längenrichtig, aber LÜCKE im belegten Körper (1,2,4,…).
  Früher crashte write_page_labels NACH teilweise angewandter Operation —
  jetzt verweigert schon validate (kein Traceback, kein Schreibzugriff)."""
  src = _working_copy()
  orig = read_labels(src)
  gapped = ["", ""] + [str(i) for i in (1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15)]
  assert len(gapped) == 16
  r = surgery_exec.run_plan(_plan(src, gapped), apply=True)
  assert r["valid"] is False
  assert "darstellbar" in r["error"]
  assert read_labels(src) == orig, "Verweigerung darf nichts schreiben"


def test_unbekannte_op_ablehnung():
  src = _working_copy()
  bad = {"operations": [{"op": "delete", "source": str(src)}]}
  r = surgery_exec.run_plan(bad, apply=True)
  assert r["valid"] is False


def test_readback_abweichung_rollback():
  """expected_after falsch => write_geschehen, dann Rollback auf Original."""
  src = _working_copy()
  orig = read_labels(src)
  bad_expected = ["x"] + [""] * 15
  r = surgery_exec.run_plan(_plan(src, _heal_labels(), bad_expected), apply=True)
  assert r["operations"][0]["rolled_back"] is True
  # Datei wieder im Originalzustand.
  assert read_labels(src) == orig


def test_validate_source_fehlend():
  r = surgery_exec.validate(
    {
      "operations": [
        {
          "op": "write_labels",
          "source": str(PKG / "nicht_da.pdf"),
          "backup": str(PKG / "runs" / "bak.pdf"),
          "labels": [""],
        }
      ]
    }
  )
  assert r[0] is False


def read_labels(p: Path):
  return pdf_kernel.read_page_labels(p)
