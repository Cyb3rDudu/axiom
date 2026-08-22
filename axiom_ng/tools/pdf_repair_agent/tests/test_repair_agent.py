"""repair_agent — Endpunkt-Tests: Mock-Protokoll, Dry-Run-Gate, Audit-Spur.

Läuft komplett in der Sandbox (fixtures/storage), Mock-Client statt Netz.
"""
from __future__ import annotations

import json
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
from tools.pdf_kernel import read_page_labels  # noqa: E402


def _cfg_sandbox():
    cfg = load_config({})
    cfg.ensure_dirs()
    assert cfg.sandbox
    return cfg


HEAL_STEPS = [
    '{"action":"forensics"}',
    '{"action":"surgery","apply":true,"plan_class":"constant-offset",'
    '"operations":[{"labels":["1","2","3","4","5","6","7","8","9","10"]}]}',
    '{"action":"report","reason":"Versatz geheilt; Folio==Label"}',
]


def test_volllauf_heilt_arbeitskopie_und_schreibt_report():
    cfg = _cfg_sandbox()
    key = "BBBB2222"  # Sandbox-Storage: falsche_labels-Kopie (Label=phys+2)
    run_dir = cfg.work_root / key
    if run_dir.exists():
        import shutil

        shutil.rmtree(run_dir)
    rep = run_agent(key, apply=True, client=MockClient(list(HEAL_STEPS)),
                    cfg=cfg)
    assert rep["verdict"] == "halt"
    assert [s["action"] for s in rep["steps"]] == ["forensics", "surgery"]
    # Heilung bewiesen an der Arbeitskopie (Storage-Original unberührt):
    assert read_page_labels(run_dir / "work.pdf") == [str(i) for i in range(1, 11)]
    # Audit-Spur mit Pflicht-Abschnitten:
    spur = json.loads((run_dir / "report.json").read_text())
    assert spur["key"] == key and "unproven" in spur and "evidence" in spur


def test_ohne_apply_kein_schreibzugriff():
    """apply=true im Step OHNE CLI-Freigabe darf NICHT schreiben."""
    cfg = _cfg_sandbox()
    key = "BBBB2222"
    run_dir = cfg.work_root / key
    if run_dir.exists():
        import shutil

        shutil.rmtree(run_dir)
    rep = run_agent(key, apply=False, client=MockClient(list(HEAL_STEPS)),
                    cfg=cfg)
    assert rep["verdict"] == "halt"
    lbls = read_page_labels(run_dir / "work.pdf")
    assert lbls == [str(i + 3) for i in range(10)], "Dry-Run schrieb heimlich!"


def test_ohne_modell_ehrliche_verweigerung():
    cfg = _cfg_sandbox()
    rep = run_agent("BBBB2222", apply=True, client=None, cfg=cfg)
    assert rep["verdict"] == "NO-MODEL"
    assert "DEEPSEEK_API_KEY" in rep["cause"]
