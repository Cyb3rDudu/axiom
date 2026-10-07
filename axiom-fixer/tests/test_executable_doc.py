"""Executable-Doc-Zeuge: das operations-Beispiel in prompts/system.txt ist
AUSFÜHRBAR — es durchläuft surgery_execs Validator grün UND den echten
Agentenpfad (Mock-Client, Sandbox).

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

# type: ignore: pyright indiziert paket-lokale Module am Workspace-Root nur
# träge — LSP-False-Positiv; pytest importiert real (siehe Paket-Venv).
from config import load_config  # noqa: E402
from deepseek_client import MockClient  # noqa: E402
from repair_agent import run_agent  # noqa: E402
from tools import surgery_exec  # noqa: E402  # type: ignore[reportAttributeAccessIssue]
from tools.pdf_kernel import doc_page_count, read_page_labels  # noqa: E402

SYSTEM_TXT = PKG / "prompts" / "system.txt"

# Sentinel: labels aus dem Doku-Beispiel übernehmen (None ist selbst ein
# Testwert — die negative Kontrolle injiziert bewusst labels=None).
_AUS_DOKU = object()


def _surgery_beispiel_aus_doku() -> dict:
    """Extrahiert das surgery-Beispiel-OBJEKT aus dem step-Schema-Abschnitt.
    Kein Wohlwollen-Annahmen: genau das, was im Prompt steht."""
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
    # Kannarke: GENAU EIN surgery-Beispiel — ein zweites, stale Beispiel
    # neben dem aktuellen darf den Zeugen nicht schweigend überleben.
    assert len(surg) == 1, (
        f"{len(surg)} surgery-Beispiele in system.txt — genau eines ist Vertrag"
    )
    return surg[0]


def _beispiel_plan(tmp_path, labels=_AUS_DOKU):
    """Doku-Beispiel → vollständiger Executor-Plan gegen eine Lab-Kopie;
    Pfadbindung wie der Dispatcher (source/backup auf die Kopie)."""
    beispiel = _surgery_beispiel_aus_doku()
    op = beispiel["operations"][0]
    assert "labels" in op, (
        "Doku-Beispiel trägt kein `labels`-Feld — der dokumentierte "
        "operations-Eintrag weicht vom Executor-Vertrag ab (system.txt)"
    )
    src = tmp_path / "falsche_labels.pdf"
    shutil.copy2(PKG / "fixtures" / "falsche_labels.pdf", src)
    plan = {
        "operations": [
            {
                "op": "write_labels",
                "source": str(src),
                "backup": str(tmp_path / "bak.pdf"),
                "labels": op["labels"] if labels is _AUS_DOKU else labels,
                "expected_after": op.get("expected_after"),
            }
        ]
    }
    return beispiel, op, src, plan


def test_doku_beispiel_validiert_gruen(tmp_path):
    """Der Tag-Zeuge: Beispiel aus der Doku → echter Validator → gültig."""
    _, op, src, plan = _beispiel_plan(tmp_path)
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
    """Negative Kontrolle: die alte Doku-Form (seite/label statt labels-
    Komplettliste) MUSS vom Validator abgelehnt werden — genau der Fehler,
    an dem der reale Lauf ehrlich scheiterte."""
    _, _, _, plan = _beispiel_plan(tmp_path, labels=None)
    ok, err = surgery_exec.validate(plan)
    assert not ok and "labels" in err


def test_beispiel_heilt_wirklich(tmp_path):
    """End-to-End auf Lab-Kopie: das DOKUMENTIERTE Beispiel angewandt heilt
    falsche_labels.pdf (3..12) auf die Folio-Wahrheit (1..10) — inkl.
    Read-back. Beweist, dass die Anleitung zum Erfolg führt, nicht nur zur
    Validierung."""
    _, op, src, plan = _beispiel_plan(tmp_path)
    # Vorher-Zustand selbst behauptet (kein Vertrauen in Fixture-Genese):
    assert read_page_labels(src) == [str(i + 3) for i in range(10)]
    res = surgery_exec.run_plan(plan, apply=True)
    assert res["applied"] and res["operations"][0]["match_expected"]
    assert read_page_labels(src) == op["labels"]


def test_beispiel_durchlaeuft_agentenpfad():
    """Der Ketten-Zeuge: das DOKUMENTIERTE Beispiel — unverändert, wie es
    im Prompt steht — durchläuft den echten Agentenpfad. h_surgery bindet
    source/backup selbst an die Arbeitskopie; die im Beispiel genannten
    Pfade sind deklarativ und werden überschrieben — genau diese Treue
    (inkl. expected_after-Durchreiche) wird hier gepinnt."""
    beispiel = _surgery_beispiel_aus_doku()
    cfg = load_config({})
    cfg.ensure_dirs()
    assert cfg.sandbox
    key = "BBBB2222"  # Sandbox-Storage: 10 Seiten, Label 3..12 (== Beispiel)
    run_dir = cfg.work_root / key
    if run_dir.exists():
        shutil.rmtree(run_dir)
    # Abschluss-Schritt ist HARNESS, nicht Beispiel: nur der surgery-Step
    # wird unverändert aus der Doku gespeist.
    schritte = [json.dumps(beispiel), '{"action":"report","reason":"geheilt"}']
    rep = run_agent(key, apply=True, client=MockClient(schritte), cfg=cfg)
    assert rep["verdict"] == "halt"
    surg = [r for r in rep["evidence"] if r.get("action") == "surgery"]
    assert surg and surg[0]["ok"] is True
    assert read_page_labels(run_dir / "work.pdf") == beispiel["operations"][0]["labels"]
    spur = json.loads((run_dir / "report.json").read_text())
    assert spur["key"] == key and "unproven" in spur and "evidence" in spur
