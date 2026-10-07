"""T1 spread_tool — Fixture-Tests: Erkennung, Owner-Formel, Trennung, Negativ."""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

import pymupdf  # noqa: E402  # type: ignore[reportMissingImports]

from fixtures import generate_fixtures  # noqa: E402
from tools import spread_tool  # noqa: E402

FIX = PKG / "fixtures"


def _ensure_fixtures():
    if not (FIX / "doppelseiten.pdf").exists():
        generate_fixtures.main()


def _landscape_pdf(n: int) -> Path:
    """Kleine echte Landschafts-Doppelseite ohne Gutter (Kontrolle)."""
    p = FIX.parent / "runs" / "_ctrl.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    for _ in range(n):
        page = d.new_page(width=1190, height=842)  # ratio 1.41 ohne Mitte
        page.insert_text((60, 100), "Nur eine Seite breit — kein Gutter.", fontsize=24)
        # volle Fläche mit Text füllen, damit keine helle Mitte entsteht
        page.insert_textbox(
            (60, 200, 1130, 800), " ".join(["Absatz"] * 200), fontsize=12
        )
    d.save(str(p))
    d.close()
    return p


def _mixed_doc() -> Path:
    """C1-Fall: EIN echter Spread (mit Bundsteg-Lücke) + EINE Hochformatseite."""
    p = FIX.parent / "runs" / "_mixed_src.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    w = 595  # A4-Hochformat; Spread = doppelte Breite (Ratio 1.41)
    h = 842
    d = pymupdf.open()
    spread = d.new_page(width=w * 2, height=h)
    for half in (0, 1):
        x0 = half * w
        spread.draw_rect(
            pymupdf.Rect(x0 + 20, 30, x0 + w - 20, h - 30), color=(0, 0, 0), width=1
        )
        spread.insert_text((x0 + 60, 100), f"LEAF {57 + half}", fontsize=22)
        spread.insert_text(
            (x0 + 60, 140),
            f"Druckseite {57 + half} eines Doppelseiten-Scans mit Textfläche.",
            fontsize=11,
        )
    hoch = d.new_page(width=w, height=h)
    hoch.insert_text((60, 100), "Anhang als Hochformat-Durchreichseite.", fontsize=18)
    d.save(str(p))
    d.close()
    return p


def test_erkennt_alle_spreads():
    _ensure_fixtures()
    doc = pymupdf.open(str(FIX / "doppelseiten.pdf"))
    n = doc.page_count
    doc.close()
    plan = spread_tool._plan(FIX / "doppelseiten.pdf", spread_tool.DEFAULT_OFFSET)
    assert all(p["is_spread"] for p in plan["pages"]), (
        f"{[p['is_spread'] for p in plan['pages']]}"
    )
    assert len(plan["pages"]) == n


def test_owner_formel_labels():
    _ensure_fixtures()
    plan = spread_tool._plan(FIX / "doppelseiten.pdf", spread_tool.DEFAULT_OFFSET)
    # Spread 0 (1-basiert 1) -> 58,59 ; Spread 4 (5) -> 66,67
    assert plan["pages"][0]["labels"] == [58, 59]
    assert plan["pages"][4]["labels"] == [66, 67]


def test_split_apply_erzeugt_blaetter_und_labels():
    _ensure_fixtures()
    dst = FIX.parent / "runs" / "spread_fixture_split.pdf"
    res = spread_tool.split_and_write(
        FIX / "doppelseiten.pdf", dst, spread_tool.DEFAULT_OFFSET
    )
    assert res["leaf_count"] == 10
    assert res["labels"] == [str(x) for x in range(58, 68)]
    assert res["labels_written"] is True  # geschlossener Lauf 58..67 darstellbar
    assert res["readback_matches"] is True
    from tools.pdf_kernel import read_page_labels

    assert read_page_labels(dst) == [str(x) for x in range(58, 68)]


def test_mixed_doc_kein_falsches_label():
    """C1-Regression: Spread + Hochformat-Seite. Soll-Labels
    ['58','59',''] sind PDF-nicht-darstellbar (die Range würde die leere
    Durchreichseite fälschlich befüllen) → Kernel verweigert EHRLICH,
    statt '60' zu schreiben."""
    src = _mixed_doc()
    dst = FIX.parent / "runs" / "spread_mixed_split.pdf"
    res = spread_tool.split_and_write(src, dst, spread_tool.DEFAULT_OFFSET)
    assert res["labels"] == ["58", "59", ""]
    assert res["labels_written"] is False  # Verweigerung, kein Falschgut
    assert "darstellbar" in res["labels_cause"]
    # Kernbeweis: KEINE fortgeschriebene Zahl auf der Durchreichseite.
    assert "60" not in res["readback"]
    assert res["readback"] == ["", "", ""]
    src.unlink(missing_ok=True)
    dst.unlink(missing_ok=True)


def test_negativ_kein_spread():
    _ensure_fixtures()
    # A4-Hochformat-Seitensatz → ratio < 1.25 → kein Spread.
    p = FIX.parent / "runs" / "_ctrl_high.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    d.new_page(width=595, height=842)
    d.save(str(p))
    d.close()
    plan = spread_tool._plan(p, spread_tool.DEFAULT_OFFSET)
    assert not any(pp["is_spread"] for pp in plan["pages"])
    p.unlink(missing_ok=True)


def test_landschaft_ohne_gutter_wird_nicht_split():
    """Wide Seite OHNE echte Bindung: Gutter fehlt → Konfidenz unter Schwelle."""
    _ensure_fixtures()
    p = _landscape_pdf(2)
    plan = spread_tool._plan(p, spread_tool.DEFAULT_OFFSET)
    # Konfidenz nahe 0, da keine Furche: soll als Nicht-Spread gelten
    # (sonst würde eine zufällige helle Seite falsch gesplittet).
    for pg in plan["pages"]:
        assert pg["is_spread"] is False, pg
    p.unlink(missing_ok=True)


def test_c5_zentrierte_luecke_ohne_staerke_kein_spread():
    """C5-Regression (reine Urteilsfunktion): fast perfekte Mittigkeits-Nähe
    (conf 0.9) mit Furchen-Stärke 0 DARF NICHT als Spread gelten — beide
    Signale sind nötig, sonst spaltet eine Wortlücke die Seite."""
    assert spread_tool._classify(strength=0.0, conf=0.9) is False
    assert spread_tool._classify(strength=0.4, conf=0.99) is False
    # Gegenteil: echte Furche + Konfidenz → Spread.
    assert spread_tool._classify(strength=2.0, conf=0.5) is True


def test_c5_leere_landschaftsseite_kein_spread():
    """C5-Regression (echte Seite): leeres Landschaftsblatt ohne jeden
    Text/Furchen-Beweis wird NICHT als Spread klassifiziert."""
    _ensure_fixtures()
    p = FIX.parent / "runs" / "_ctrl_blank.pdf"
    p.parent.mkdir(parents=True, exist_ok=True)
    d = pymupdf.open()
    d.new_page(width=1190, height=842)  # ratio 1.41, komplett leer
    d.save(str(p))
    d.close()
    plan = spread_tool._plan(p, spread_tool.DEFAULT_OFFSET)
    assert plan["pages"][0]["is_spread"] is False, plan["pages"][0]
    p.unlink(missing_ok=True)
