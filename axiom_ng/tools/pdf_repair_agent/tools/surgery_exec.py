"""T4 surgery_exec — der EINZIGE Label-Schreibpfad (über pdf_kernel).

Eigenständig aufrufbar; Dry-Run-Default (zeigt Wirkung, ändert nichts):
    .venv/bin/python tools/surgery_exec.py plan.json
    .venv/bin/python tools/surgery_exec.py plan.json --apply

Erwartet ein PLAN-Dokument (JSON), das der Agent gem. Protokoll erzeugt hat
(und das forensics_tool + Sonde als Beweislage bestätigen). Pro Operation
(zur Verdeutlichung Pseudo-JSON, im echten Plan gültiges JSON):
  { "op": "write_labels",
    "source": "/abs/pfad/quelle.pdf",
    "backup": "/abs/pfad/backup.pdf",
    "labels": ["", "", "3", "4"],
    "expected_after": ["", "", "1", "2"] }

ABLAUF (unverletzlich):
  1. VALIDIERE Plan: source existiert, backup gesetzt (≠ source),
     labels-Länge == page_count, Mapping PDF-darstellbar
     (_build_ranges-Wächter des Kernels) — sonst Abbruch (kein Raten).
  2. BACKUP-Pflicht: Kopie → backup (nur im --apply-Modus; Dry-Run zeigt
     nur). Scheitert das Backup, wird die Operation EHRLICH abgelehnt,
     BEVOR irgendetwas geschrieben wird.
  3. write_page_labels via pdf_kernel (die eine Schreibengine).
  4. READ-BACK: labels neu lesen, mit expected_after vergleichen.
     Abweichung → Datei zurückrollen (backup → source), Fehler melden.
  5. Roh-Evidenz: alles als "applied"/"rolled_back"/"dry_run" berichtet.
     Ein Laufzeitfehler pro Operation wird gefangen: Rollback auf das
     frische Backup + strukturierter Bericht — nie ein nackter Traceback,
     nie eine halb geschriebene Datei ohne Meldung.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))  # standalone: `python tools/surgery_exec.py …`

from tools import pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402


def validate(plan: dict) -> tuple[bool, str]:
    """Struktur- + Konsistenzprüfung des Plans (kein Schreibzugriff)."""
    if not isinstance(plan, dict):
        return False, "Plan ist kein Objekt"
    ops = plan.get("operations")
    if not isinstance(ops, list) or not ops:
        return False, "Plan hat keine operations-Liste"
    for i, op in enumerate(ops):
        if op.get("op") != "write_labels":
            return False, f"Op {i}: unbekannter op '{op.get('op')}'"
        src = Path(op.get("source", ""))
        if not src.exists():
            return False, f"Op {i}: source fehlt: {op['source']}"
        # Backup-Pflicht: ohne gültiges Backup KEINE Operation (sonst
        # copy2(src, '.') Müll in den CWD und Rollback-Chaos).
        backup = op.get("backup")
        if not isinstance(backup, str) or not backup.strip():
            return False, f"Op {i}: backup fehlt (Pflicht vor jeder Änderung)"
        if str(Path(backup)) == str(src):
            return False, f"Op {i}: backup == source — Backup wäre die Änderung selbst"
        labels = op.get("labels")
        if not isinstance(labels, list):
            return False, f"Op {i}: labels fehlt"
        want = pdf_kernel.doc_page_count(src)
        if len(labels) != want:
            return False, f"Op {i}: labels-Länge {len(labels)} != page_count {want}"
        # PDF-Darstellbarkeit VOR jedem Schreiben prüfen (sonst crasht der
        # Write mitten im Lauf und lässt die Quelle teils geschrieben).
        if any(labels) and not pdf_kernel._build_ranges(labels):
            return False, (
                f"Op {i}: labels nicht PDF-darstellbar (Lücke/Sprung im"
                f" belegten Körper oder Restseiten nach dem Lauf) — Verweigerung"
            )
        exp = op.get("expected_after")
        if exp is not None and len(exp) != want:
            return False, f"Op {i}: expected_after-Länge {len(exp)} != {want}"
    return True, "ok"


def _apply_op_plan(op: dict) -> dict:
    """Führt EINE write_labels-Operation mit Backup + Read-Back aus.

    Kein Raise nach außen: Laufzeitfehler → Rollback auf das frische
    Backup + strukturierter applied:false-Bericht.
    """
    src = Path(op["source"])
    backup = Path(op["backup"])
    labels = op["labels"]
    expected = op.get("expected_after")
    result: dict = {
        "op": "write_labels",
        "source": str(src),
        "backup": str(backup),
        "pages": len(labels),
    }
    backup_taken = False
    try:
        # Backup-Pflicht: Kopie vor jeder Änderung; scheitert sie, wird
        # EHRLICH abgelehnt, BEVOR irgendetwas geschrieben wird.
        backup.parent.mkdir(parents=True, exist_ok=True)
        try:
            shutil.copy2(src, backup)
        except OSError as e:
            result["applied"] = False
            result["cause"] = f"Backup fehlgeschlagen ({e}) — nichts geschrieben"
            result["rolled_back"] = False
            return result
        backup_taken = True
        pdf_kernel.write_page_labels(src, labels)
        got = pdf_kernel.read_page_labels(src)
        result["readback"] = got
        # None = nichts verglichen (expected_after nicht gesetzt), kein "true".
        result["match_expected"] = None if expected is None else (got == expected)
        if result["match_expected"] is False:
            # Read-Back-Abweichung: Rollback (backup → source).
            shutil.copy2(backup, src)
            result["rolled_back"] = True
            result["applied"] = False
            result["cause"] = "readback != expected_after"
        else:
            result["rolled_back"] = False
            result["applied"] = True
    except Exception as e:  # noqa: BLE001 — Berichtspflicht schlägt Traceback
        if backup_taken:
            try:
                shutil.copy2(backup, src)
                rolled = True
            except OSError:
                rolled = False
        else:
            rolled = False
        result["applied"] = False
        result["rolled_back"] = rolled
        result["cause"] = f"Laufzeitfehler: {e}"
    return result


def run_plan(plan: dict, apply: bool = False) -> dict:
    """Validiert (und wendet optional an) einen Plan. Lässt keine Ausnahme
    nach außen — Validierungs-/Lesefehler werden strukturiert berichtet."""
    try:
        ok, err = validate(plan)
    except Exception as e:  # noqa: BLE001 — z. B. korrupte Quelle beim Lesen
        return {
            "valid": False,
            "error": f"Validierung fehlgeschlagen: {e}",
            "applied": False,
        }
    if not ok:
        return {"valid": False, "error": err, "applied": False}
    if not apply:
        # Dry-Run: nur validieren + beabsichtigte Wirkung zeigen.
        return {
            "valid": True,
            "applied": False,
            "dry_run": True,
            "operations": [
                {
                    "op": "write_labels",
                    "source": op["source"],
                    "backup": op["backup"],
                    "label_count": len(op["labels"]),
                    "expected_after": op.get("expected_after"),
                }
                for op in plan["operations"]
            ],
        }
    results = [_apply_op_plan(op) for op in plan["operations"]]
    return {
        "valid": True,
        "applied": all(r.get("applied") for r in results),
        "operations": results,
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="surgery_exec")
    p.add_argument("plan_file")
    p.add_argument("--apply", action="store_true")
    a = p.parse_args(argv)
    try:
        plan = json.loads(Path(a.plan_file).read_text())
    except (OSError, json.JSONDecodeError) as e:
        print(json.dumps({"valid": False, "error": str(e)}))
        return 1
    result = run_plan(plan, apply=a.apply)
    print(json.dumps(result, ensure_ascii=False, indent=1))
    if result.get("valid") == False or any(  # noqa: E712
        o.get("rolled_back") or o.get("applied") is False
        for o in result.get("operations", [])
    ):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
