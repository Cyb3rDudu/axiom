"""T2 ocr_tool — Fixture-Tests: Textschicht-Diagnose, Abhängigkeits-Ehrlichkeit,
Qualitätstor. Der echte OCR-Lauf hängt an tesseract/ghostscript/ocrmypdf
(fehlen auf diesem System) → hier wird der ehrliche Unfähigkeitszweig geprüft,
nicht geraten. Das Qualitätstor ist eine reine Funktion der Zeichenzahlen und
ist unabhängig von den Binaries testbar."""

from __future__ import annotations

import sys
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]
import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

from tools import (  # noqa: E402  # type: ignore[reportAttributeAccessIssue]
    ocr_tool,
    pdf_kernel,
)

FIX = PKG / "fixtures"
# Verfügbarkeits-Sonde über das WERKZEUG selbst (venv-bewusste Auflösung —
# shutil.which allein wäre der alte Binary-Bug).
HAS_OCR_BINS = all(ocr_tool._bins_available().values())


def _ensure():
    if not (FIX / "ohne_textschicht.pdf").exists():
        from fixtures import generate_fixtures

        generate_fixtures.main()


def test_erkennt_textschicht_fehlend():
    _ensure()
    pl = ocr_tool.plan(FIX / "ohne_textschicht.pdf")
    assert pl["text_layer_ok"] is False
    assert pl["text_layer_missing_pages"] == list(range(8))
    assert pl["raster_scan_hypothesis"] is True


def test_gesund_hat_textschicht():
    _ensure()
    pl = ocr_tool.plan(FIX / "gesund.pdf")
    assert pl["text_layer_ok"] is True
    assert pl["ocr_verdict"] == "not_needed"


def test_qualitaetstor_schuetzt_vor_leerer_textschicht():
    # Leeres/zu dünnes OCR-Ergebnis darf das Tor NICHT passieren.
    assert (
        ocr_tool._quality_report(FIX / "ohne_textschicht.pdf")["quality_gate_pass"]
        is False
    )


def test_qualitaetstor_je_seite_nicht_aggregat():
    """C4-Regression: 1 dichte + mehrere leere Seiten. Das Aggregat-Tor
    (sum >= MIN*len) hätte bestanden — per Seite muss es FAILen."""
    p = FIX.parent / "runs" / "_ocr_gate.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    dense = d.new_page(width=595, height=842)
    dense.insert_textbox(
        (40, 60, 555, 780),
        "Volltextseite. " * 200,  # weit über MIN_TEXT_CHARS
        fontsize=10,
    )
    for _ in range(3):
        d.new_page(width=595, height=842)  # leere Seiten (0 Zeichen)
    d.save(str(p))
    d.close()
    q = ocr_tool._quality_report(p)
    assert q["quality_gate_pass"] is False
    assert q["pages_below_min"] == [1, 2, 3]  # nur die leeren, Belegstellen
    assert q["textchars_pages"][0] >= ocr_tool.MIN_TEXT_CHARS
    p.unlink(missing_ok=True)


@pytest.mark.skipif(
    HAS_OCR_BINS, reason="OCR-Binaries vorhanden → echter Lauf, kein Unfähigkeitszweig"
)
def test_apply_ohne_binaries_luegt_nicht():
    _ensure()
    dst = FIX.parent / "runs" / "ocr_refuse_test.pdf"
    # Ohne tesseract/gs/ocrmypdf muss --apply ehrlich ablehnen (kein Raten,
    # keine leere Erfolgsmeldung) und nichts schreiben.
    res = ocr_tool.run_ocr(FIX / "ohne_textschicht.pdf", dst, "deu")
    assert res["applied"] is False
    assert "nicht baubar" in res["cause"]
    assert not dst.exists()


@pytest.mark.skipif(
    not HAS_OCR_BINS,
    reason="OCR-Binaries fehlen → Unfähigkeitszweig hat eigenen Test",
)
def test_ocr_live_lauf_und_qualitaetstor():
    """Echter ocrmypdf-Lauf auf der Text-Pixel-Fixture: Textschicht wird
    gebaut, das per-seite-Tor besteht, die Ausgabedatei trägt Text."""
    _ensure()
    dst = FIX.parent / "runs" / "ocr_live_test.pdf"
    res = ocr_tool.run_ocr(FIX / "ohne_textschicht.pdf", dst, "deu")
    assert res["applied"] is True, res.get("cause")
    assert res["quality"]["quality_gate_pass"] is True
    assert res["quality"]["pages_below_min"] == []
    counts = pdf_kernel.page_char_count(dst)
    assert all(c >= ocr_tool.MIN_TEXT_CHARS for c in counts), counts
    dst.unlink(missing_ok=True)
