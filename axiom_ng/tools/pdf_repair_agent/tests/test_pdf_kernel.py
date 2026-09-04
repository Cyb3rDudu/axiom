"""Kernel-Regressionen: Label-Round-Trip, Titelei, Verweigerungs-Pfade.

Jede Aussage ist eine Sonde: Round-Trip muss identisch lesen (nie
Versatz), nicht darstellbare Mappings müssen ValueError werfen (nie
stilles Falsch-Schreiben), und die Fixtures müssen ihre dokumentierten
Label-Bäume tragen (Drift-Fang gegen generate_fixtures.py).
"""

from __future__ import annotations

import sys
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]
import pytest

PKG = Path(__file__).resolve().parent.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))

from tools.pdf_kernel import (  # noqa: E402
    _build_ranges,
    read_page_labels,
    write_page_labels,
)

FIX = PKG / "fixtures"
W, H = pymupdf.paper_size("a4")


def _make_pdf(path: Path, n: int) -> Path:
    """Frisches n-Seiten-PDF ohne Labels (Basis für Round-Trip-Sonden)."""
    doc = pymupdf.open()
    for i in range(n):
        page = doc.new_page(width=W, height=H)
        page.insert_text((40, 60), f"Folio {i + 1}", fontsize=12)
        page.insert_text((40, 100), f"TEST-SEITE-{i + 1}", fontsize=18)
        page.insert_textbox(
            (40, 130, W - 40, H - 60),
            f"Probeabsatz Seite {i + 1} mit hinreichend Text zum Zählen.",
            fontsize=11,
        )
    doc.save(path)
    doc.close()
    return path


# ------------------------------------------------------- Round-Trip-Sonden ---


def test_round_trip_geschlossener_lauf(tmp_path):
    p = _make_pdf(tmp_path / "t.pdf", 5)
    want = ["1", "2", "3", "4", "5"]
    write_page_labels(p, want)
    assert read_page_labels(p) == want


def test_titelei_bleibt_unbenannt(tmp_path):
    # Regression C2: Seiten vor der ersten Range liefen vorher auf IndexError.
    p = _make_pdf(tmp_path / "t.pdf", 5)
    want = ["", "", "1", "2", "3"]
    write_page_labels(p, want)
    assert read_page_labels(p) == want


def test_versetzter_lauf_round_trip(tmp_path):
    # Der Reparatur-Kernfall: Titelei + verschobener Lauf (firstpagenum>1).
    p = _make_pdf(tmp_path / "t.pdf", 6)
    want = ["", "", "57", "58", "59", "60"]
    write_page_labels(p, want)
    assert read_page_labels(p) == want


# ---------------------------------------------------- Verweigerungs-Sonden ---


def test_nachlaufende_leerseiten_werden_verweigert(tmp_path):
    # Regression C3: ["1","2","",""] würde als ["1","2","3","4"] gelesen.
    p = _make_pdf(tmp_path / "t.pdf", 4)
    with pytest.raises(ValueError):
        write_page_labels(p, ["1", "2", "", ""])
    # Quelle bleibt unverändert: frisches PDF ohne Labels bleibt unbenannt.
    assert read_page_labels(p) == [""] * 4


def test_luecke_im_koerper_wird_verweigert(tmp_path):
    p = _make_pdf(tmp_path / "t.pdf", 4)
    with pytest.raises(ValueError):
        write_page_labels(p, ["1", "2", "", "4"])
    assert read_page_labels(p) == [""] * 4


def test_sprung_im_lauf_wird_verweigert(tmp_path):
    p = _make_pdf(tmp_path / "t.pdf", 3)
    with pytest.raises(ValueError):
        write_page_labels(p, ["1", "2", "5"])
    assert read_page_labels(p) == [""] * 3


def test_laengen_abweichung_wird_verweigert(tmp_path):
    # Regression C4: kurze Liste würde Folgeseiten fälschlich befüllen,
    # lange Liste würde Einträge still verwerfen.
    p = _make_pdf(tmp_path / "t.pdf", 5)
    with pytest.raises(ValueError):
        write_page_labels(p, ["1", "2"])
    with pytest.raises(ValueError):
        write_page_labels(p, ["1", "2", "3", "4", "5", "6", "7"])
    assert read_page_labels(p) == [""] * 5


def test_alle_leer_loescht_labels(tmp_path):
    p = _make_pdf(tmp_path / "t.pdf", 5)
    write_page_labels(p, ["1", "2", "3", "4", "5"])
    assert any(read_page_labels(p))
    write_page_labels(p, [""] * 5)
    assert read_page_labels(p) == [""] * 5


# ------------------------------------------------------- _build_ranges-Tafel ---


def test_build_ranges_tafel():
    # #251: führende unbenannte Seiten bekommen einen Deckungs-Entry an
    # Seite 0 (prefix-only) — ein Baum ohne Deckung ist spec-legal, aber
    # pymupdf's get_label-util stürzt im Runner-Chunker ab (E2E-Befund).
    titled_run = [
        {"startpage": 0, "prefix": ""},  # Titelei ausdrücklich unbenannt
        {"startpage": 1, "prefix": "", "style": "D", "firstpagenum": 3},
    ]
    assert _build_ranges(["", "3", "4"]) == titled_run  # Titelei + Lauf
    assert _build_ranges(["1", "2"]) == [
        {"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}
    ]  # kurzer Lauf ab Seite 0
    assert _build_ranges(["1", "2", "", "4"]) == []  # Lücke im Körper
    assert _build_ranges(["1", "2", "5"]) == []  # Sprung im Lauf
    assert _build_ranges(["", "", ""]) == []  # komplett unbenannt
    assert _build_ranges(["1", "2", "", ""]) == []  # Nachlauf leer
    assert _build_ranges(["1", "a", "3"]) == []  # nicht-numerisch


# ------------------------------------------- Fixture-Invarianten (Drift-Fang) ---


def test_fixture_gesund_labels():
    assert read_page_labels(FIX / "gesund.pdf") == [str(i + 1) for i in range(12)]


def test_fixture_falsche_labels_versatz():
    assert read_page_labels(FIX / "falsche_labels.pdf") == [
        str(i + 3) for i in range(10)
    ]


def test_fixture_forensik_titelei_plus_versatz():
    want = ["", ""] + [str(i + 3) for i in range(14)]
    assert read_page_labels(FIX / "forensik.pdf") == want
