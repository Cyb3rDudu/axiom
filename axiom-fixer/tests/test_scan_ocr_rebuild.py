"""#284 scan_ocr_rebuild — Werkzeug- und Katalog-Regel-Tests.

Die echten OCR-Läufe hängen an tesseract/ghostscript/ocrmypdf (venv-bewusst
aufgelöst). Sind sie da, laufen die Rebuild-Tests REAL (Owner-Pilot:
Kommandoform end-to-end bewiesen); fehlen sie, überspringen nur die
Lauf-Tests — die Diagnose-Tests laufen immer (reine pymupdf-Messungen).
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest  # type: ignore[reportMissingImports]

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from config import load_config  # noqa: E402
from repair_agent import run_agent  # noqa: E402
from tools import (  # type: ignore[reportAttributeAccessIssue]  # noqa: E402
    labeltree_heal,
    scan_ocr_rebuild,
)
from tools.pdf_kernel import read_page_labels  # noqa: E402

FIX = PKG / "fixtures"
HAS_OCR_BINS = all(scan_ocr_rebuild._bins_available().values())

needs_ocr = pytest.mark.skipif(not HAS_OCR_BINS, reason="ocrmypdf/tesseract/gs fehlen")


def _ensure():
    # Storage-Kopien (ignoriert, regenerierbar) auffrischen — die Fixture-
    # PDFs selbst sind KOMMITTET und werden aus Tests NIE regeneriert
    # (Review #284: Fresh-Checkout-Testläufe dürfen den Baum nicht schmutzig
    # machen).
    if not (FIX / "storage" / "DDDD4444" / "scan_folios.pdf").exists():
        from fixtures import generate_fixtures

        generate_fixtures.ensure_storage()


def _fresh_run_dir(cfg, key):
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


# ── Diagnose (immer lauffähig) ───────────────────────────────────────────


def test_diagnose_trennt_die_klassen():
    _ensure()
    d = scan_ocr_rebuild.diagnose(FIX / "ohne_textschicht.pdf")
    assert d["text_layer"] is False and d["mode"] == "plain"
    d = scan_ocr_rebuild.diagnose(FIX / "scan_mit_folios.pdf")
    assert d["text_layer"] is False and d["mode"] == "plain"
    d = scan_ocr_rebuild.diagnose(FIX / "gesund.pdf")
    assert d["class"] == "not_scan_class" and d["mode"] is None
    d = scan_ocr_rebuild.diagnose(FIX / "kaputte_textschicht.pdf")
    # Reder-Klasse: Textschicht DA, aber konkateniert (Space-Ratio ~0,
    # Wortlänge weit jenseits deutscher Prosa) → force-Modus
    assert d["text_layer"] is True
    assert d["broken_segmentation"] is True
    assert d["mode"] == "force"


def test_diagnose_ist_rein_lesend(monkeypatch):
    """Mutationssonde: die Diagnose darf die Datei nicht verändern."""
    _ensure()
    before = (FIX / "scan_mit_folios.pdf").read_bytes()
    scan_ocr_rebuild.diagnose(FIX / "scan_mit_folios.pdf")
    assert (FIX / "scan_mit_folios.pdf").read_bytes() == before


def test_rebuild_lehnt_ohne_binaries_ehrlich_ab(monkeypatch):
    _ensure()
    monkeypatch.setattr(
        scan_ocr_rebuild,
        "_bins_available",
        lambda: {"tesseract": False, "gs": True, "ocrmypdf": True},
    )
    res = scan_ocr_rebuild.run_rebuild(
        FIX / "ohne_textschicht.pdf",
        _fresh_run_dir(_cfg_sandbox(), "OCRTOOL0") / "never.pdf",
    )
    assert res["applied"] is False
    assert "tesseract" in res["cause"]


# ── echte Rebuilds (OCR-Binaries nötig) ─────────────────────────────────


@needs_ocr
def test_rebuild_pure_scan_baut_durchsuchbare_schicht():
    """DoD: pure-scan fixture repariert zu einem durchsuchbaren PDF —
    Textschicht vorhanden, Seitenzahl UND Geometrie unverändert."""
    _ensure()
    dst = _fresh_run_dir(_cfg_sandbox(), "OCRTOOL1") / "rebuild.pdf"
    res = scan_ocr_rebuild.run_rebuild(FIX / "scan_mit_folios.pdf", dst, lang="deu")
    assert res["applied"] is True, res.get("cause")
    assert res["pages"] == 7  # Seitezahl unverändert
    assert res["quality"]["total_chars"] > 0  # Textschicht da
    import pymupdf  # type: ignore[import-not-found]

    a, b = pymupdf.open(FIX / "scan_mit_folios.pdf"), pymupdf.open(dst)
    try:
        assert [p.rect for p in a] == [p.rect for p in b]  # Geometrie unverändert
        assert str(b[0].get_text()).strip() != ""  # Seite 1 hat OCR-Text
    finally:
        a.close(), b.close()


@needs_ocr
def test_rebuild_force_heilt_worttrennung():
    """DoD: kaputte Textschicht (Reder-Klasse) — konkatenierte Wörter sind
    nach dem force-Rebuild wieder leerzeichen-getrennt."""
    _ensure()
    import pymupdf  # type: ignore[import-not-found]

    before = pymupdf.open(FIX / "kaputte_textschicht.pdf")
    t0 = str(before[0].get_text())
    before.close()
    assert t0.count(" ") < 3  # kaputt: kaum Leerzeichen
    dst = _fresh_run_dir(_cfg_sandbox(), "OCRTOOL2") / "rebuild.pdf"
    res = scan_ocr_rebuild.run_rebuild(
        FIX / "kaputte_textschicht.pdf", dst, lang="deu", force=True
    )
    assert res["applied"] is True, res.get("cause")
    after = pymupdf.open(dst)
    t1 = str(after[0].get_text())
    after.close()
    assert " " in t1  # Leerzeichen zurück
    words = t1.split()
    assert len(words) > 10  # echte Wörter, kein Monolith
    assert all(len(w) < 40 for w in words)  # konkatenierte Brocken sind weg


@needs_ocr
def test_rebuild_vollgas_und_ohne_internen_kill(monkeypatch):
    """#293: VOLLGAS-Default — der ocrmypdf-Aufruf trägt --jobs = alle
    verfügbaren Kerne, und es gibt KEIN internes Timeout-Kill mehr (der
    Rebuild dauert, so lange er dauert; Wedge-Guards leben außen bei
    fix.sh/Invoker). Gepingt per Cmd-Abgriff: das timeout-Kwarg muss
    ABWESEND sein, --jobs muss die Kernzahl tragen."""
    import os

    captured: dict = {}

    class _FakeRc0:
        returncode = 0
        stderr = ""

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["kwargs"] = kwargs
        return _FakeRc0()

    monkeypatch.setattr(scan_ocr_rebuild.subprocess, "run", fake_run)
    _ensure()
    dst = _fresh_run_dir(_cfg_sandbox(), "OCRJOBS") / "probe.pdf"
    # Verifikate wegstubben — die Sonde misst die Prozessübergabe
    monkeypatch.setattr(scan_ocr_rebuild.pdf_kernel, "page_char_count", lambda pdf: [0])
    monkeypatch.setattr(scan_ocr_rebuild, "_page_dims", lambda pdf: [(100.0, 100.0)])
    monkeypatch.setattr(
        scan_ocr_rebuild,
        "_text_layer_metrics",
        lambda pdf: {"pages": 1, "total_chars": 99, "mean_chars_per_page": 99.0},
    )
    res = scan_ocr_rebuild.run_rebuild(FIX / "scan_mit_folios.pdf", dst, lang="deu")
    assert res["applied"] is True, res
    cmd = captured["cmd"]
    jobs = cmd[cmd.index("--jobs") + 1]
    assert jobs == str(os.cpu_count() or 1)
    assert res.get("jobs") == jobs
    assert "timeout" not in captured["kwargs"], (
        "#293: kein internes Timeout-Kill mehr — der Invoker-Backstop ist reiner Wedge-Guard"
    )


# ── Katalog-Regel (repair_agent, echte OCR) ─────────────────────────────


@needs_ocr
def test_katalogregel_scan_mit_folios_heilt_text_und_labels_2in1():
    """DoD-Kern: der Fixer heilt einen Scan MIT Folio-Lauf in EINER Heilung
    — Textschicht aus dem OCR-Rebuild, Labels aus der NEUEN Schicht
    (Post-OCR-Folio-Verifikation), Bericht zeigt das geheilte Mapping."""
    _ensure()
    cfg = _cfg_sandbox()
    key = "DDDD4444"  # Sandbox-Storage: scan_mit_folios-Kopie
    run_dir = _fresh_run_dir(cfg, key)
    orig = cfg.zotero_storage_root / key / "scan_folios.pdf"
    orig_labels_before = read_page_labels(orig)

    rep = run_agent(key, apply=True, cfg=cfg, ocr_lang="deu")

    assert rep["verdict"] == "healed", rep["final_step"]["reason"]
    assert rep["catalog_rule"] == "scan_ocr_rebuild"
    work = run_dir / "work.pdf"
    assert work.exists()  # Artefakt bleibt (Invocker-Upload-Pfad)
    # 2-in-1: Labels aus der NEUEN Textschicht geschrieben (phys 3 → 5)
    labels = read_page_labels(work)
    assert labels[:2] == ["", ""]
    assert labels[2:] == ["5", "6", "7", "8", "9"]
    assert rep["heal_readback"] is not None  # Readback-Beweis dabei
    # Storage-Original unberührt (Custody: Heilung nur auf der Arbeitskopie)
    assert read_page_labels(orig) == orig_labels_before


@needs_ocr
def test_katalogregel_force_override_heilt_kaputte_schicht():
    """Per-Case-Override: --ocr-mode force erzwingt das Wegrastern auch ohne
    Auto-Diagnose (Operator-Weg für Grenzfälle); Reder-Fixture heilt."""
    _ensure()
    cfg = _cfg_sandbox()
    key = "EEEE5555"  # Sandbox-Storage: kaputte_textschicht-Kopie
    _fresh_run_dir(cfg, key)

    rep = run_agent(key, apply=True, cfg=cfg, ocr_force=True)

    assert rep["verdict"] == "healed", rep["final_step"]["reason"]
    import pymupdf  # type: ignore[import-not-found]

    d = pymupdf.open(cfg.work_root / key / "work.pdf")
    t = str(d[0].get_text())
    d.close()
    assert " " in t and len(t.split()) > 10  # Leerzeichen zurück


def test_katalogregel_dry_run_veraendert_nichts():
    _ensure()
    cfg = _cfg_sandbox()
    key = "DDDD4444"
    run_dir = _fresh_run_dir(cfg, key)
    rep = run_agent(key, apply=False, cfg=cfg, ocr_lang="deu")
    assert rep["verdict"] == "report"
    assert rep["catalog_rule"] == "scan_ocr_rebuild"
    # Dry-Run: die Arbeitskopie darf angelegt sein, muss aber byte-identisch
    # zum Storage-Original bleiben (kein Heilungs-Byte geschrieben).
    work = run_dir / "work.pdf"
    orig = cfg.zotero_storage_root / key / "scan_folios.pdf"
    assert work.read_bytes() == orig.read_bytes()


def test_katalogregel_gesundes_pdf_faellt_durch():
    """Nicht die Klasse → None-artiges Verhalten: der Bericht kommt aus dem
    normalen Agentenpfad (hier: Mock-frei endet er NO-MODEL oder Agent)."""
    _ensure()
    cfg = _cfg_sandbox()
    key = "AAAA1111"  # gesund.pdf — intakte Textschicht, kein force
    _fresh_run_dir(cfg, key)
    rep = run_agent(key, apply=True, cfg=cfg)
    assert rep.get("catalog_rule") != "scan_ocr_rebuild"


@needs_ocr
def test_katalogregel_scan_ohne_folios_heilt_textlayer_allein():
    """Ohne darstellbares Folio-Mapping bleibt die Textschicht-Heilung
    allein bestehen (physical_only-verarbeitbar) — verdict healed, aber
    KEIN Label-Readback (ehrlich)."""
    _ensure()
    cfg = _cfg_sandbox()
    key = "CCCC3333"  # ohne_textschicht.pdf — keine Foliozahlen
    _fresh_run_dir(cfg, key)
    rep = run_agent(key, apply=True, cfg=cfg, ocr_lang="deu")
    assert rep["verdict"] == "healed", rep["final_step"]["reason"]
    assert rep["heal_readback"] is None  # kein Folio-Lauf → keine Labels
    assert labeltree_heal.label_tree_state(cfg.work_root / key / "work.pdf") in (
        "missing",
        "empty",
    )


@needs_ocr
def test_katalogregel_auto_diagnose_force_ohne_override():
    """Mutationsonde (Review #284): die AUTO-Diagnose muss den force-Modus
    selbst wählen — der Produktionsfall (Reder-Klasse) kommt OHNE
    Aufruf-Override an. `mode_force = force or diag.get("mode") == "force"`
    ohne die diag-Hälfte liefe dieser Test rot."""
    _ensure()
    cfg = _cfg_sandbox()
    key = "EEEE5555"  # kaputte_textschicht — diagnose muss force erkennen
    _fresh_run_dir(cfg, key)

    rep = run_agent(key, apply=True, cfg=cfg)  # KEIN ocr_force

    assert rep["verdict"] == "healed", rep["final_step"]["reason"]
    assert rep["catalog_rule"] == "scan_ocr_rebuild"
    assert "(force," in rep["final_step"]["reason"] or "force" in rep["final_step"]["reason"]
    import pymupdf  # type: ignore[import-not-found]

    d = pymupdf.open(cfg.work_root / key / "work.pdf")
    t = str(d[0].get_text())
    d.close()
    assert " " in t and len(t.split()) > 10  # Leerzeichen zurück


def _cjk_font() -> str | None:
    """Eine CJK-Font für die Sonde (fixe Kandidaten-Pfade); fehlt → Skip."""
    for cand in (
        "/System/Library/Fonts/Hiragino Sans GB.ttc",
        "/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
        "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
        "/usr/share/fonts/opentype/noto/NotoSansCJK.ttc",
    ):
        if Path(cand).exists():
            return cand
    return None


def test_diagnose_verweigert_force_fuer_space_lose_schriften(tmp_path):
    """Review-Major (Mutationssonde): intakter CJK-Textlayer trifft dieselben
    Schwellen (space_ratio ~0, lange Wörter) wie die Reder-Klasse — die
    Diagnose muss ihn ALS INTAKT erkennen (spaceless_script), sonst würde
    --force-ocr --lang deu eine intakte fremde Schrift zerstören."""
    font = _cjk_font()
    if font is None:
        pytest.skip("keine CJK-Font auf diesem System")
    import pymupdf  # type: ignore[import-not-found]

    W, H = pymupdf.paper_size("a4")
    d = pymupdf.open()
    p = d.new_page(width=W, height=H)
    p.insert_font(fontname="cjk", fontfile=font)
    p.insert_text((60, 120), "教育測定評価研究は重要である" * 3, fontsize=12, fontname="cjk")
    p.insert_textbox(
        (60, 160, W - 60, H - 80),
        "日本語のテキスト層は正常である。教育測定評価研究。" * 4,
        fontsize=11,
        fontname="cjk",
    )
    d.save(tmp_path / "cjk.pdf")
    d.close()

    r = scan_ocr_rebuild.diagnose(tmp_path / "cjk.pdf")
    assert r["text_layer"] is True
    assert r["spaceless_script"] is True
    assert r["broken_segmentation"] is False
    assert r["mode"] is None  # intakt → fällt an die normale Klassifikation
    # Gegenprobe: die Reder-Fixture (Latin, konkateniert) bleibt force
    r2 = scan_ocr_rebuild.diagnose(FIX / "kaputte_textschicht.pdf")
    assert r2["broken_segmentation"] is True and r2["mode"] == "force"
