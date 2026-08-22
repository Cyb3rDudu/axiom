"""G4 — Wahrheits-Ordnung (Owner-Ruling 23.08.) + Beweis-Zeugen.

· RAUSCH-ZEUGE:    Jahreszahl/vereinzelter Zahl als „Folio“ wird NICHT
                   als Anker akzeptiert (Qualitäts-Tor, Verweigerung).
· DUBS-G4:        vollautonome Heilung des reproduzierten +15-Offsets auf
                   einer Dubs-Fenster-Kopie — Plan → Surgery → Read-back →
                   Folio-Anker grün, ohne menschlichen Schritt.
· 3-STELLEN:      truth_source im Bericht dokumentiert genutzte/offene
                   Stellen (Lab: Stelle 1, 2+3 ehrlich offen).
· OCR-BINARY:     ocrmypdf wird venv-bewusst gefunden (Regression Fix a).
"""

from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

# type: ignore: pyright indiziert paket-lokale/venv-Module am Workspace-Root
# nur träge — LSP-False-Positiv; pytest importiert real (siehe Paket-Venv).
import pymupdf  # noqa: E402  # type: ignore[reportMissingImports]
import pytest  # noqa: E402

from config import load_config  # noqa: E402
from deepseek_client import MockClient  # noqa: E402
from repair_agent import _truth_source, h_forensics, run_agent  # noqa: E402
from tests.helpers import (  # noqa: E402  # type: ignore[reportMissingImports]
    _damage_spec_offset,
    _trim_to_body_window,
)
from tools.forensics_tool import anchor_folio_run, build_map  # noqa: E402
from tools.ocr_tool import _bins_available, ocrmypdf_bin, plan  # noqa: E402
from tools.pdf_kernel import read_page_labels  # noqa: E402

FIX = PKG / "fixtures"
DIFFICULT = FIX / "difficult"
# Binary-Sonde über das WERKZEUG (venv-bewusst) — konsistent zu test_ocr_tool.
HAS_OCR_BINS = all(_bins_available().values())


# ----------------------------------------------------------- Rausch-Zeuge ----


def _pdf_mit_kopfzahlen(tmp: Path, name: str, zahlen: list[str]) -> Path:
    d = pymupdf.open()
    w, h = pymupdf.paper_size("a4")
    for z in zahlen:
        p = d.new_page(width=w, height=h)
        p.insert_text((40, 60), z, fontsize=12)  # Kopfzone, „gedruckte Zahl“
        p.insert_textbox(
            (40, 130, w - 40, h - 60),
            "Fließtext der Seite, der keine Folio-Rolle spielt "
            "und auch bei langer Betrachtung keine wird.",
            fontsize=11,
        )
    out = tmp / name
    d.save(str(out))
    d.close()
    return out


def test_rausch_zeuge_jahr_ist_kein_anker(tmp_path):
    """Eine Jahreszahl in der Kopfzeile ist KEIN Folio-Anker: kein +1-Lauf,
    kein Anker — Verweigerung bleibt Pflicht (kein Raten)."""
    p = _pdf_mit_kopfzahlen(tmp_path, "jahr.pdf", ["2024"] * 8)  # 8× dieselbe „Zahl“
    m = build_map(p)
    assert anchor_folio_run(m) == [], "Jahreszahl darf nie Anker werden"


def test_rausch_zeuge_vereinzelte_zahlen_kein_lauf(tmp_path):
    """Vereinzelte, nicht aufeinanderfolgende Zahlen (Tabellen-/Abschnitts-
    zahl-Rauschen) bilden keinen messbaren Lauf."""
    p = _pdf_mit_kopfzahlen(
        tmp_path, "rausch.pdf", ["7", "19", "3", "42", "8", "100", "5", "23"]
    )
    m = build_map(p)
    assert anchor_folio_run(m) == []


def test_ankerlauf_nur_bei_echtem_druckfoliolauf(tmp_path):
    """Positiver Gegencheck: ein echter +1-Druckfoliolauf (>= 5) wird als
    Stelle-1-Quelle akzeptiert — Rauschseiten davor stören nicht."""
    zahlen = ["99", "2024"] + [str(i) for i in range(11, 19)]  # Rauschen + Lauf
    p = _pdf_mit_kopfzahlen(tmp_path, "echt.pdf", zahlen)
    m = build_map(p)
    run = anchor_folio_run(m)
    assert run and len(run) == 8, run
    assert [a["folio"] for a in run] == [str(i) for i in range(11, 19)]


# --------------------------------------------------------------- OCR-Fix a --


def test_ocrmypdf_venv_aufloesung():
    """Regression Fix a: ocrmypdf wird im eigenen Venv gefunden (sys.prefix),
    nicht nur über PATH — und der Plan meldet alle drei Binaries ehrlich."""
    bin_path = ocrmypdf_bin()
    if (Path(sys.prefix) / "bin" / "ocrmypdf").is_file():
        assert bin_path == str(Path(sys.prefix) / "bin" / "ocrmypdf")
    bins = _bins_available()
    assert bins["ocrmypdf"] is (bin_path is not None)


@pytest.mark.skipif(
    not HAS_OCR_BINS,
    reason="OCR-Binaries fehlen — Binär-Bilanz nicht prüfbar (Live-Zeuge: test_ocr_tool)",
)
def test_ocr_dryrun_liefert_evidenz():
    """DoD: OCR-Dry-Run auf textloser Seite liefert Evidenz (Lage + Binaries)."""
    src = FIX / "ohne_textschicht.pdf"
    if not src.exists():
        from fixtures.generate_fixtures import main as gen

        gen()
    pl = plan(src)
    assert pl["text_layer_missing_pages"], "textlose Seiten müssen benannt sein"
    assert pl["ocr_binaries"] == {"tesseract": True, "gs": True, "ocrmypdf": True}, pl[
        "ocr_binaries"
    ]
    assert pl["ocr_verdict"] == "possible"


# ------------------------------------------------- DUBS-G4 Vollautonomie ----


def test_truth_source_nur_gemessene_stellen():
    """C1-Regression: RAG-Erreichbarkeit ALLEIN beweist NICHT Stelle 2/3 —
    ohne Chunk-Seiten-Vergleich und Annotation-Check bleiben beide offen
    (Notiz statt Beweis); Stelle 1 nur aus forensischer Evidenz."""
    ts = _truth_source(
        [
            {"action": "probe", "ok": True, "measured": ["rag-reachability"]},
            {"action": "forensics", "ok": True},
        ]
    )
    assert ts["stelle1_druckseite"].startswith("forensics_tool")
    assert ts["stelle2_chunk"] is None and ts["stelle3_zitat"] is None
    assert ts["offene_stellen"] == ["stelle2_chunk", "stelle3_zitat"]
    assert any("rag_erreichbar" in n for n in ts.get("notizen", []))
    # Ohne forensische Evidenz wird auch Stelle 1 als ungemessen benannt:
    ts2 = _truth_source([{"action": "probe", "ok": True, "measured": []}])
    assert ts2["stelle1_druckseite"] == "nicht gemessen"


def test_forensics_evidenz_traegt_anker(tmp_path):
    """W1-Regression: das Rausch-Qualitäts-Tor ist CODE-Evidenz —
    h_forensics liefert `anchors` mit (bei Rauschen ehrlich [])."""
    cfg = load_config(
        {
            "ZOTERO_STORAGE_ROOT": str(tmp_path / "storage"),
            "PDF_REPAIR_WORK_ROOT": str(tmp_path / "work"),
            "PDF_REPAIR_BACKUP_ROOT": str(tmp_path / "backup"),
        }
    )
    cfg.ensure_dirs()
    ctx = {"cfg": cfg, "key": "NOISE", "allow_apply": False}
    # Rausch-PDF (8× Jahreszahl): KEIN Anker, trotzdem ok=True (offen ≠ Fehler).
    store = tmp_path / "storage" / "NOISE"
    store.mkdir(parents=True)
    shutil.copy2(_pdf_mit_kopfzahlen(tmp_path, "n.pdf", ["2024"] * 8), store / "n.pdf")
    res = h_forensics({}, ctx)
    assert res["ok"] is True and res["anchors"] == [], res["anchors"]
    # Echter Lauf (Folios 20..27): Anker stehen direkt in der Evidenz.
    store2 = tmp_path / "storage" / "RUN8"
    store2.mkdir(parents=True)
    shutil.copy2(
        _pdf_mit_kopfzahlen(tmp_path, "r.pdf", [str(i) for i in range(20, 28)]),
        store2 / "r.pdf",
    )
    res2 = h_forensics({}, {**ctx, "key": "RUN8"})
    assert [a["folio"] for a in res2["anchors"]] == [str(i) for i in range(20, 28)]


def _dubs_fenster_kopie(tmp: Path) -> tuple[Path, list[str]]:
    """Dubs-Fenster (num. Körperlauf) als Lab-Attachment MIT +15-Schaden."""
    book = next(DIFFICULT.glob("Dubs*.pdf"))
    storage = tmp / "storage" / "G4DUBS"
    storage.mkdir(parents=True)
    att = storage / "dubs.pdf"
    truth = _trim_to_body_window(book, att)
    assert truth is not None, "Dubs-Fenster nicht darstellbar"
    _damage_spec_offset(att, delta=15)
    # S1: Schaden WIRKSAM belegen, bevor der Agent läuft (Fenster = ein
    # D-Range → alle Labels exakt +15) — sonst wäre der Zeuge sinnlos.
    assert read_page_labels(str(att)) == [str(int(t) + 15) for t in truth], (
        "Schaden nicht wirksam"
    )
    return att, truth


def test_dubs_g4_vollautonome_heilung(tmp_path):
    """DoD-Kronzeuge: reproduzierter +15-Offset auf Dubs-Kopie. Der Agent
    (Mock-Planner, echte Werkzeuge) heilt VOLLAUTONOM: forensische M-Quelle
    (Stelle 1) → Plan → Surgery → Read-back → Folio-Anker grün. Kein
    menschlicher Schritt, keine Produktionsschreibzugriffe (Lab-Storage)."""
    if not DIFFICULT.is_dir():
        import pytest

        pytest.skip("fixtures/difficult/ nicht vorhanden (lokales Testset)")
    att, truth = _dubs_fenster_kopie(tmp_path)

    # Planner-Rolle (Modell): Diagnose aus Stelle-1-M-Quelle. Die Anker
    # kommen aus forensics_tool — dem Standardweg — NICHT aus der Sonde.
    m = build_map(att)
    run = anchor_folio_run(m)
    assert run, "Stelle-1-Ankerlauf fehlt — Diagnose müsste verweigern"
    # Plan aus Anker-Arithmetik (wie Stufe-1): Label[p] = f0 + (p - p0),
    # fortgeführt bis Dokumentende (PDF-Range-Semantik); führende Seiten
    # vor dem Ankerlauf bleiben unbenannt — kernel-darstellbar.
    n = len(read_page_labels(str(att)))
    p0, f0 = run[0]["page"], int(run[0]["folio"])
    labels = [""] * p0 + [str(f0 + i) for i in range(n - p0)]

    cfg = load_config(
        {
            "ZOTERO_STORAGE_ROOT": str(tmp_path / "storage"),
            "PDF_REPAIR_WORK_ROOT": str(tmp_path / "work"),
            "PDF_REPAIR_BACKUP_ROOT": str(tmp_path / "backup"),
        }
    )
    cfg.ensure_dirs()
    steps = [
        '{"action":"probe"}',  # Stellen 2/3 → offen (kein Eskalationsgrund)
        '{"action":"forensics"}',
        json.dumps(
            {
                "action": "surgery",
                "apply": True,
                "plan_class": "constant-offset",
                "operations": [{"labels": labels}],
            }
        ),
        '{"action":"report","reason":"Stelle-1-Heilung; Folio-Anker grün; '
        'Stellen 2/3 im Lab offen"}',
    ]
    rep = run_agent("G4DUBS", apply=True, client=MockClient(steps), cfg=cfg)

    assert rep["verdict"] == "halt", rep["verdict"]
    # Surgery muss ERFOLGREICH gewesen sein (kein stiller Teilerfolg):
    surg = [r for r in rep["evidence"] if r.get("action") == "surgery"]
    assert surg and surg[0].get("ok") is True, surg
    work = cfg.work_root / "G4DUBS" / "work.pdf"
    # Read-back: geheilte Labels == Folio-Wahrheit (Stelle 1).
    healed = read_page_labels(work)
    assert healed == labels
    assert healed[p0:] == [a["folio"] for a in run] + [
        str(f0 + i) for i in range(len(run), n - p0)
    ]
    # Folio-Anker NACHHER grün: Ankerseiten zeigen Label == Druckfolio.
    m2 = build_map(work)
    folios = {p: q["folio"] for p, q in enumerate(m2["pages"])}
    for a in run:
        assert healed[a["page"]] == folios[a["page"]], a
    # 3-Stellen-Transparenz: Stelle 1 genutzt, 2+3 ehrlich offen (Lab).
    ts = rep["truth_source"]
    assert ts["stelle1_druckseite"].startswith("forensics_tool")
    assert "stelle2_chunk" in ts["offene_stellen"]
    assert "stelle3_zitat" in ts["offene_stellen"]
    # Backup-Pflicht erfüllt (surgery_exec schrieb es):
    assert (work.parent / "backup.pdf").exists()
