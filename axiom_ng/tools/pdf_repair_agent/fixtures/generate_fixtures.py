"""Synthetische Sandbox-Fixtures für den pdf_repair_agent — bekannte,
deterministische Label-Bäume. KEINE echten Bücher werden berührt.

Erzeugt unter fixtures/:
  gesund.pdf            Tier-1-Labels deckend monoton 1..N + Textschicht.
  falsche_labels.pdf    Tier-1-Labels mit Versatz (Label = phys+OFF);
                        gedruckte Foliozahlen decken auf (→ T3/T4-Heilpfad).
  ohne_textschicht.pdf  Scanner ohne Textschicht, ohne Labels (→ T2).
  doppelseiten.pdf      2-up-Scans (Landschafts-Spreads), null Labels
                        (→ T1 + T4 nach Owner-Formel).
  forensik.pdf          Titelei + IV/sachverzeichnis + gedruckte
                        Foliozahl in der Kopfzeile je Seite (→ T3-Beweiskarte).

Jedes Fixture trägt pro physischer Seite eine vorhersagbare Druckstruktur.
Nutzen:  <paket>/.venv/bin/python fixtures/generate_fixtures.py
"""

from __future__ import annotations

import sys
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))
W, H = pymupdf.paper_size("a4")


def _fill(doc: pymupdf.Document, phys: int, folio: str) -> None:
    """Basistext: bekannter Satz je Seite + gedruckte Foliozahl (Kopfzeile)."""
    page = doc.new_page(width=W, height=H)
    # Gedruckte Seitenzahl: BARE Zahl in der Kopfzone (wie im echten Satzbuch).
    page.insert_text((40, 60), folio, fontsize=12)
    page.insert_text((40, 100), f"PEINLICH-SEITE-{phys}", fontsize=18)
    para = (
        f"Dies ist die synthetische Fixture-Seite Nummer {phys}. "
        f"Auf Seite {phys} des Belegkodizes steht dieser eindeutige "
        f"Satz absolut allein in diesem Absatz, damit er sich in "
        f"jedem Textwerkzeug auffinden lässt. Er endet mit einem "
        f"Punkt und einem Zeilenumbruch."
    )
    page.insert_textbox((40, 130, W - 40, H - 60), para, fontsize=11)


def _new_doc() -> pymupdf.Document:
    return pymupdf.open()


def _label(doc: pymupdf.Document, saveto: Path, labels: list[str]) -> None:
    """Label-Baum via write_page_labels setzen (pymupdf-Range-Semantik);
    '' = unbenannt."""
    doc.save(saveto)
    doc.close()
    from tools.pdf_kernel import write_page_labels  # type: ignore[reportMissingImports]

    write_page_labels(saveto, labels)


def main() -> None:
    """Erzeugt alle Fixtures. Idempotent regeneriert."""
    here = HERE
    here.mkdir(parents=True, exist_ok=True)

    # 1) gesund.pdf — 12 Seiten, Labels "1".."12", Textschicht ok.
    d = _new_doc()
    for i in range(12):
        _fill(d, i + 1, str(i + 1))
    _label(d, here / "gesund.pdf", [str(i + 1) for i in range(12)])
    # 2) falsche_labels.pdf — 10 Seiten, Label = phys+2 (Versatz), Folio korrekt.
    d = _new_doc()
    for i in range(10):
        _fill(d, i + 1, str(i + 1))
    _label(d, here / "falsche_labels.pdf", [str(i + 3) for i in range(10)])

    # 3) ohne_textschicht.pdf — 8 Seiten OHNE Text-Layer (nur Rasterbild
    #    MIT lesbaren Text-Pixeln): Textseite rendern → rastern → als Bild
    #    einbetten. get_text bleibt leer (kein Text-Span), OCR findet Wörter.
    d = _new_doc()
    for i in range(8):
        src = _new_doc()
        sp = src.new_page(width=W, height=H)
        sp.insert_text((60, 120), f"Kapitel {i + 1}", fontsize=24)
        sp.insert_textbox(
            (60, 160, W - 60, H - 80),
            "Dies ist ein synthetischer Scan mit gedrucktem Text, den die "
            "optische Zeichenerkennung wiederfinden muss. Jede Seite trägt "
            "mehrere Zeilen gut lesbarer Schrift, damit das Qualitätstor "
            "der Textschicht-Heilung genügend Zeichen je Seite vorfindet. "
            f"Die vorliegende Seite {i + 1} endet mit einem Satzzeichen.",
            fontsize=14,
        )
        pix = sp.get_pixmap(dpi=150)
        src.close()
        page = d.new_page(width=W, height=H)
        # JPEG statt PNG: gleiche Lesbarkeit, ein Bruchteil der Größe.
        page.insert_image(page.rect, stream=pix.tobytes("jpg"))
    d.save(here / "ohne_textschicht.pdf")
    d.close()

    # 4) doppelseiten.pdf — 5 Landschafts-Spreads (je 2 "Buchseiten"), 0 Labels.
    d = _new_doc()
    leaf = 1
    for _ in range(5):
        page = d.new_page(width=W * 2, height=H)
        for half in (0, 1):
            x0 = half * W
            page.draw_rect(
                pymupdf.Rect(x0 + 20, 30, x0 + W - 20, H - 30), color=(0, 0, 0), width=1
            )
            page.insert_text((x0 + 60, 100), f"LEAF {leaf}", fontsize=22)
            page.insert_text(
                (x0 + 60, 140),
                f"Druckseite eines Doppelseiten-Scans Leaf {leaf}.",
                fontsize=11,
            )
            leaf += 1
    d.save(here / "doppelseiten.pdf")
    d.close()

    # 5) forensik.pdf — Titelei + IV + 14 Seiten mit gedruckter Foliozahl;
    #    Körper-Labels um +2 versetzt (Titelei bleibt unbenannt).
    d = _new_doc()
    t = d.new_page(width=W, height=H)
    t.insert_text((40, 100), "Erster Eindruck der Fixture-Ausgabe", fontsize=22)
    t.insert_textbox(
        (40, 140, W - 40, H - 80),
        "Ein gedrucktes Vorwort über synthetische Belegmaterie. "
        "Hier steht nichts von Folio, damit die Seite als Titelblatt "
        "(Impressum) erkannt wird und keine Drucknummer trägt. "
        "© Fixture-Verlag, ISBN 000-0-0000-0000-0. Auflage 1. 2024.",
        fontsize=11,
    )
    s = d.new_page(width=W, height=H)
    s.insert_text((40, 100), "Inhaltsverzeichnis (IV)", fontsize=22)
    for j, entry in enumerate(
        [
            "Einleitung ................................ Seite 1",
            "Hauptteil  ................................ Seite 3",
            "Schluss  ................................. Seite 9",
        ]
    ):
        s.insert_text((60, 150 + j * 20), entry, fontsize=11)
    for i in range(14):
        _fill(d, i + 1, str(i + 1))
    # Körper-Labels absichtlich um +2 versetzt (Reparaturfall): Label zeigt
    # nicht die gedruckte Foliozahl, nur ein führender unbenannter Block
    # (Titelei + IV) steht unten als '' — ein geschlossener, repräsentierbarer
    # numerischer Lauf OHNE Lücke im belegten Körper (PDF-Semantik).
    labels = ["", ""] + [str(i + 3) for i in range(14)]
    _label(d, here / "forensik.pdf", labels)

    # 6) storage/ — Zotero-Storage-Einheit (Sandbox-Storage-Root).
    storage = here / "storage"
    storage.mkdir(parents=True, exist_ok=True)

    def save_copy(att: str, filename: str, src: str):
        att_dir = storage / att
        att_dir.mkdir(parents=True, exist_ok=True)
        (att_dir / filename).write_bytes((here / src).read_bytes())

    save_copy("AAAA1111", "gesund.pdf", "gesund.pdf")
    save_copy("BBBB2222", "falsch.pdf", "falsche_labels.pdf")
    save_copy("CCCC3333", "scan.pdf", "ohne_textschicht.pdf")
    print("fixtures generiert nach", here)


if __name__ == "__main__":
    main()
