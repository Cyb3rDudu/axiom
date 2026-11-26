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

from tools import surgery_exec  # noqa: E402  # type: ignore[reportAttributeAccessIssue]
from tools.pdf_kernel import (  # noqa: E402
    _build_ranges,
    _validate_ranges,
    labels_from_ranges,
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


# --------------------------------------- _validate_ranges-Tafel (#395, W4) ---


class TestValidateRanges395:
    """Struktur-Tore des Mehrfach-Range-Baums: der Docstring-Versprechen
    (Deckung ab Seite 0, strikt steigend, PDF-Semantik-Styles, int >= 1
    als firstpagenum) werden eingefordert — pymupdf normalisiert leise
    (<= 0 → Default, Lücken still unbenannt), der Validator darf das
    nicht durchlassen, sonst divergiert der Write vom Render."""

    @staticmethod
    def _d(sp, fpn=1):
        return {"startpage": sp, "prefix": "", "style": "D", "firstpagenum": fpn}

    def test_gueltiger_baum_passiert(self):
        ranges = [self._d(0), self._d(5, 3)]
        assert _validate_ranges(ranges, 10) == ranges

    def test_reihenfolge_strikt_steigend(self, tmp_path):
        assert _validate_ranges([self._d(5), self._d(2)], 10) is None
        assert _validate_ranges([self._d(2), self._d(2)], 10) is None

    def test_erste_range_muss_seite_0_decken(self, tmp_path):
        assert _validate_ranges([self._d(3)], 10) is None
        # Deckungspflicht gilt auch MIT gültiger Reihenfolge:
        assert _validate_ranges([self._d(1), self._d(5)], 10) is None

    def test_unzulaessiger_style(self, tmp_path):
        r = {"startpage": 0, "prefix": "", "style": "X", "firstpagenum": 1}
        assert _validate_ranges([r], 10) is None

    def test_bool_startpage_ist_kein_int(self, tmp_path):
        r = {"startpage": True, "prefix": "", "style": "D", "firstpagenum": 1}
        assert _validate_ranges([r], 10) is None

    def test_bool_firstpagenum_ist_kein_int(self, tmp_path):
        r = {"startpage": 0, "prefix": "", "style": "D", "firstpagenum": True}
        assert _validate_ranges([r], 10) is None

    def test_firstpagenum_null_und_negativ_werden_verweigert(self, tmp_path):
        # Alle nummerierten Styles: pymupdf normalisiert <= 0 still zum
        # Default und divergierte damit vom labels_from_ranges-Render.
        for style in ("D", "r", "R"):
            for fpn in (0, -2):
                r = {"startpage": 0, "prefix": "", "style": style,
                     "firstpagenum": fpn}
                assert _validate_ranges([r], 10) is None, (style, fpn)

    def test_firstpagenum_beyond_n_passiert_strukturcheck(self):
        # Struktur-Tor, nicht Konsistenz-Tor: 1..n übersteigt die Seite
        # nicht — set_page_labels/PageLabels-Semantik trägt jeden Startwert.
        assert _validate_ranges([self._d(0, 99)], 3) is not None

    def test_write_label_ranges_lehnt_tafel_ab(self, tmp_path):
        """Der Write-Pfad erbt die Tore (ValueError, nichts geschrieben)."""
        from tools.pdf_kernel import write_label_ranges as wlr

        p = _make_pdf(tmp_path / "v.pdf", 6)
        for bad in ([self._d(5)], [self._d(5), self._d(2)],
                    [{"startpage": 0, "prefix": "", "style": "D",
                      "firstpagenum": 0}]):
            with pytest.raises(ValueError):
                wlr(p, bad)
        assert read_page_labels(p) == [""] * 6  # Quelle unverändert

    def test_surgery_validate_lehnt_leeren_render_ab(self, tmp_path):
        """No-Op-Refusal über surgery_exec.validate: ein Range-Baum, der
        KEINE Seite benennt (hier: reiner Deckungs-Entry ohne Nummerierung),
        rendert nur ''-Labels — gleiche Pforte wie write_labels."""
        p = _make_pdf(tmp_path / "noop.pdf", 4)
        plan = {
            "operations": [
                {
                    "op": "write_label_ranges",
                    "source": str(p),
                    "backup": str(tmp_path / "bak.pdf"),
                    "ranges": [{"startpage": 0, "prefix": ""}],
                }
            ]
        }
        ok, err = surgery_exec.validate(plan)
        assert not ok and "benennen keine Seite" in err
        # Gegenprobe: echte Nummerierung passiert.
        plan["operations"][0]["ranges"] = [
            {"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}
        ]
        ok, _ = surgery_exec.validate(plan)
        assert ok
        # Render-Sicht selbst: Deckungs-Entry benennt nichts.
        assert labels_from_ranges([{"startpage": 0, "prefix": ""}], 4) == [""] * 4


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
