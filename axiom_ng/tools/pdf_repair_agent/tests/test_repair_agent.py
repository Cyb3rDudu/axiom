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
from repair_agent import main, run_agent  # noqa: E402
from tools.pdf_kernel import read_page_labels  # noqa: E402


def _fresh_run_dir(cfg, key):
    """Run-Verzeichnis zurücksetzen — jeder Test startet ohne Altlasten."""
    import shutil

    run_dir = cfg.work_root / key
    if run_dir.exists():
        shutil.rmtree(run_dir)
    return run_dir


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
    run_dir = _fresh_run_dir(cfg, key)
    # Storage-Original einfrieren (E): die Heilung darf NUR die Arbeitskopie
    # berühren — das Original bleibt byte-identisch falsch (3..12).
    orig = cfg.zotero_storage_root / key / "falsch.pdf"
    original_vorher = read_page_labels(orig)
    assert original_vorher == [str(i + 3) for i in range(10)]
    rep = run_agent(key, apply=True, client=MockClient(list(HEAL_STEPS)), cfg=cfg)
    assert rep["verdict"] == "halt"
    assert [s["action"] for s in rep["steps"]] == ["forensics", "surgery"]
    # Heilung bewiesen an der Arbeitskopie:
    assert read_page_labels(run_dir / "work.pdf") == [str(i) for i in range(1, 11)]
    assert read_page_labels(orig) == original_vorher, (
        "Storage-Original wurde verändert!"
    )
    # Audit-Spur mit Pflicht-Abschnitten:
    spur = json.loads((run_dir / "report.json").read_text())
    assert spur["key"] == key and "unproven" in spur and "evidence" in spur


def test_ohne_apply_kein_schreibzugriff():
    """apply=true im Step OHNE CLI-Freigabe darf NICHT schreiben."""
    cfg = _cfg_sandbox()
    key = "BBBB2222"
    run_dir = _fresh_run_dir(cfg, key)
    rep = run_agent(key, apply=False, client=MockClient(list(HEAL_STEPS)), cfg=cfg)
    assert rep["verdict"] == "halt"
    lbls = read_page_labels(run_dir / "work.pdf")
    assert lbls == [str(i + 3) for i in range(10)], "Dry-Run schrieb heimlich!"


def test_spread_ocr_apply_ohne_freigabe_erzeugt_keine_artefakte():
    """D: apply=true-Schritte ohne CLI-Freigabe dürfen KEINE Ausgabedateien
    (spread_split.pdf / ocr.pdf) anlegen — Plan-Evidenz ja, Bytes nein."""
    cfg = _cfg_sandbox()
    key = "BBBB2222"
    run_dir = _fresh_run_dir(cfg, key)
    steps = [
        '{"action":"spread","apply":true}',
        '{"action":"ocr","apply":true}',
        '{"action":"report","reason":"nur Plan-Evidenz gesammelt"}',
    ]
    rep = run_agent(key, apply=False, client=MockClient(steps), cfg=cfg)
    assert rep["verdict"] == "halt"
    assert not (run_dir / "spread_split.pdf").exists()
    assert not (run_dir / "ocr.pdf").exists()


def test_ohne_modell_ehrliche_verweigerung():
    cfg = _cfg_sandbox()
    rep = run_agent("BBBB2222", apply=True, client=None, cfg=cfg)
    assert rep["verdict"] == "NO-MODEL"
    assert "DEEPSEEK_API_KEY" in rep["cause"]


def test_nomodel_schreibt_reportjson_ueberschreibt_stale():
    """B: Auch der NO-MODEL-Abbruch schreibt report.json — ein stale
    Bericht eines früheren Laufs kann nie vom Rückgabewert abweichen."""
    cfg = _cfg_sandbox()
    key = "BBBB2222"
    run_dir = _fresh_run_dir(cfg, key)
    rep = run_agent(key, apply=False, client=None, cfg=cfg)
    assert rep["verdict"] == "NO-MODEL"
    spur = json.loads((run_dir / "report.json").read_text())
    assert spur == rep and spur["verdict"] == "NO-MODEL"
    # Folgelauf mit Modell überschreibt (kein NO-MODEL-Stand bleibt liegen):
    rep2 = run_agent(
        key,
        apply=False,
        client=MockClient(['{"action":"report","reason":"nachgetragen"}']),
        cfg=cfg,
    )
    assert rep2["verdict"] == "halt"
    spur2 = json.loads((run_dir / "report.json").read_text())
    assert spur2 == rep2 and spur2["verdict"] == "halt"


def test_fehlender_key_unproven_ehrlich():
    """G: Scheitert ein Handler (hier: Key ohne PDF), landet das zwingend
    im Pflicht-Abschnitt unproven — nie still behauptet."""
    cfg = _cfg_sandbox()
    rep = run_agent(
        "ZZZZ9999",
        apply=False,
        client=MockClient(
            ['{"action":"forensics"}', '{"action":"report","reason":"unbekannt"}']
        ),
        cfg=cfg,
    )
    assert rep["verdict"] == "halt"
    assert rep["unproven"], "unproven-Pflichtabschnitt ist leer"
    assert any("kein PDF" in u for u in rep["unproven"])


def test_main_ohne_modell_exit1_und_json_report(capsys, tmp_path, monkeypatch):
    """G: main() verweigert ohne API-Key mit Exit 1 und druckt den
    NO-MODEL-Bericht als JSON auf stdout."""
    monkeypatch.delenv("DEEPSEEK_API_KEY", raising=False)  # Env überlagert
    env = tmp_path / "config.env"
    env.write_text("")  # leer: kein Key, Sandbox-Default bleibt
    rc = main(["--key", "BBBB2222", "--config", str(env)])
    rep = json.loads(capsys.readouterr().out)
    assert rc == 1
    assert rep["verdict"] == "NO-MODEL"
