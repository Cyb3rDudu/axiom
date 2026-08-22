"""Executable-Doc-Zeuge: das operations-Beispiel in prompts/system.txt ist
AUSFÜHRBAR — es durchläuft surgery_execs Validator grün.

Wächst die Doku auseinander mit dem Executor-Vertrag, fällt dieser Zeuge
ROT (die Fehlerklasse „Agent befolgt die Doku und wird abgelehnt" stirbt
damit endgültig). Negative Kontrolle: die alte kaputte Form
({"seite": 1, "label": "I"}) muss vom Validator abgelehnt werden.
"""
from __future__ import annotations

import json
import re
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from tools import surgery_exec  # noqa: E402  # type: ignore[reportAttributeAccessIssue]
from tools.pdf_kernel import doc_page_count, read_page_labels  # noqa: E402

SYSTEM_TXT = PKG / "prompts" / "system.txt"


def _surgery_beispiel_aus_doku() -> dict:
    """Extrahiert das surgery-Beispiel-OBJEKT aus dem step-Schema-Abschnitt.
    Keine Wohlwollen-Annahmen: genau das, was im Prompt steht."""
    text = SYSTEM_TXT.read_text()
    schema = text.split("## step-Schema", 1)[1]
    # Alle eingerückten JSON-Blöcke des Beispiel-Abschnitts parsen.
    objs = []
    for block in re.finditer(r"^\s+(\{.*?\})\s*$", schema, re.M | re.S):
        raw = block.group(1)
        try:
            objs.append(json.loads(raw))
        except json.JSONDecodeError:
            continue
    surg = [o for o in objs if isinstance(o, dict) and o.get("action") == "surgery"]
    assert surg, "system.txt enthält kein parsebares surgery-Beispiel"
    return surg[0]


def test_doku_beispiel_validiert_gruen(tmp_path):
    """Der Tag-Zeuge: Beispiel aus der Doku → echter Validator → gültig."""
    beispiel = _surgery_beispiel_aus_doku()
    op = beispiel["operations"][0]

    # Pfade wie der Dispatcher auflösen (source existiert wirklich machen):
    src = tmp_path / "falsche_labels.pdf"
    shutil.copy2(PKG / "fixtures" / "falsche_labels.pdf", src)
    backup = tmp_path / "bak.pdf"
    plan = {"operations": [{
        "op": "write_labels",
        "source": str(src),
        "backup": str(backup),
        "labels": op["labels"],
        "expected_after": op.get("expected_after"),
    }]}
    ok, err = surgery_exec.validate(plan)
    assert ok, f"Doku-Beispiel ist NICHT executor-valide: {err}"
    # labels-Länge == page_count ist Teil des Vertrags — hier bewiesen:
    assert len(op["labels"]) == doc_page_count(src)
    # Dry-Run über den echten Pfad (kein Schreibzugriff, aber volle Kette):
    res = surgery_exec.run_plan(plan, apply=False)
    assert res["valid"] and res["dry_run"]


def test_doku_beispiel_vertrag_felder():
    """Der Vertrag muss vollständig dokumentiert sein: source · backup ·
    labels · expected_after sowie Längen- und PRESERVE-Hinweis."""
    text = SYSTEM_TXT.read_text()
    schema = text.split("## step-Schema", 1)[1]
    for feld in ("source", "backup", "labels", "expected_after"):
        assert f"`{feld}`" in schema or f'"{feld}"' in schema, feld
    assert "page_count" in schema, "Längen-Hinweis fehlt"
    assert "PRESERVE" in schema, "PRESERVE-Hinweis fehlt"


def test_alte_kaputte_form_wird_abgelehnt(tmp_path):
    """Negative Kontrolle: die alte Doku-Form (seite/label statt labels-\n    Komplettliste) MUSS vom Validator abgelehnt werden — genau der Fehler,\n    an dem der reale Lauf ehrlich scheiterte."""
    src = tmp_path / "falsche_labels.pdf"
    shutil.copy2(PKG / "fixtures" / "falsche_labels.pdf", src)
    plan = {"operations": [{
        "op": "write_labels",
        "source": str(src),
        "backup": str(tmp_path / "bak.pdf"),
        "labels": None,  # was bei {"seite":1,"label":"I"} ankommt
    }]}
    ok, err = surgery_exec.validate(plan)
    assert not ok and "labels" in err


def test_beispiel_heilt_wirklich(tmp_path):
    """End-to-End auf Lab-Kopie: das DOKUMENTIERTE Beispiel angewandt heilt
    falsche_labels.pdf (3..12) auf die Folio-Wahrheit (1..10) — inkl.
    Read-back. Beweist, dass die Anleitung zum Erfolg führt, nicht nur zur
    Validierung."""
    beispiel = _surgery_beispiel_aus_doku()
    op = beispiel["operations"][0]
    src = tmp_path / "falsche_labels.pdf"
    shutil.copy2(PKG / "fixtures" / "falsche_labels.pdf", src)
    plan = {"operations": [{
        "op": "write_labels",
        "source": str(src),
        "backup": str(tmp_path / "bak.pdf"),
        "labels": op["labels"],
        "expected_after": op.get("expected_after"),
    }]}
    res = surgery_exec.run_plan(plan, apply=True)
    assert res["applied"] and res["operations"][0]["match_expected"]
    assert read_page_labels(src) == op["labels"]
