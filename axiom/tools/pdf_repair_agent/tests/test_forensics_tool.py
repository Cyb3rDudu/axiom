"""T3 forensics_tool — Fixture-Tests: Druck-Struktur-Karte (Roh-Evidenz)."""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

# type: ignore: pyright indiziert den `tools`-Namespace nur träge und löst
# forensics_tool nicht als Attribut auf — dies ist ein LSP-False-Positiv;
# pytest importiert das Modul real (siehe venv).
import pymupdf  # noqa: E402  # type: ignore[reportMissingImports]

from tools import (  # noqa: E402
    forensics_tool,  # type: ignore[reportAttributeAccessIssue]
)

FIX = PKG / "fixtures"


def _ensure():
    if not (FIX / "forensik.pdf").exists():
        from fixtures import generate_fixtures

        generate_fixtures.main()


def test_forensik_map_struktur():
    _ensure()
    m = forensics_tool.build_map(FIX / "forensik.pdf")
    assert m["page_count"] == 16
    # Seite 0 = Titelei (Impressum-Verdacht, kein Folio), Seite 1 = IV.
    assert m["pages"][0]["is_titelei"] is True
    assert m["pages"][1]["is_toc"] is True
    # Folio-Sequenz im Körper ist monoton (1..14) und ohne Anomalien.
    assert m["folio_sequence_monotonic"] is True
    assert m["folio_anomalies"] == []
    # IV-Zeilen aus der Titelei-Seite extrahiert.
    assert any("Einleitung" in ln for ln in m["toc_lines"])


def test_forensik_folio_und_label_getrennt():
    """Label (=+2 versetzt) und gedrucktes Folio sind unterschiedliche Wahrheit."""
    _ensure()
    m = forensics_tool.build_map(FIX / "forensik.pdf")
    # Seite 2 (phys Index 2): Folio 1, Label '3' (versetzt).
    p = m["pages"][2]
    assert p["folio"] == "1"
    assert p["tier1_label"] == "3"
    assert p["folio"] != p["tier1_label"]  # Abweichung = Beweis für T4-Plan


def test_folio_luecke_als_anomalie():
    """Positive Regression: gedruckte Folios 1,2,4 (Lücke) → Anomalie-Bericht
    mit Belegwerten — Roh-Evidenz statt Pauschal-Monotonie."""
    p = PKG / "runs" / "_folio_gap.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    for folio in (1, 2, 4):
        page = d.new_page(width=595, height=842)
        page.insert_text((40, 60), str(folio), fontsize=12)  # Kopf-Zone, bare Zahl
        page.insert_text((40, 100), f"Seite mit Folio {folio}.", fontsize=14)
        page.insert_textbox(
            (40, 130, 555, 780),
            "Textkörper der Belegseite. " * 30,
            fontsize=11,
        )
    d.save(str(p))
    d.close()
    m = forensics_tool.build_map(p)
    assert m["folio_sequence_monotonic"] is False
    assert m["folio_anomalies"] == [{"prev_folio": 2, "next_folio": 4, "gap": 1}]
    p.unlink(missing_ok=True)


def test_falsche_labels_map_showt_kein_titelei():
    """falsche_labels.pdf = reiner Offset, keine Titelei/IV-Struktur."""
    _ensure()
    m = forensics_tool.build_map(FIX / "falsche_labels.pdf")
    assert m["titelei_pages"] == 0
    assert m["toc_pages"] == 0
    assert m["pages"][0]["folio"] == "1"


def test_ohne_textschicht_text_layer_false():
    _ensure()
    m = forensics_tool.build_map(FIX / "ohne_textschicht.pdf")
    assert m["pages"][0]["text_layer"] is False
    assert m["pages"][0]["textchars"] == 0


def test_gesund_folio_equals_label():
    """Gesundes Fixture: Label == Folio == gedruckte Zahl (kein Heilbedarf)."""
    _ensure()
    m = forensics_tool.build_map(FIX / "gesund.pdf")
    for p in m["pages"][:3]:
        assert p["folio"] == p["tier1_label"], p
