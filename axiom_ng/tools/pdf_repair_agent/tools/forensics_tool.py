"""T3 forensics_tool — Beweis-Extraktion: DRUCK-STRUKTUR-KARTE (Roh-Evidenz).

Eigenständig aufrufbar (Dry-Run, nur Lesen):
    .venv/bin/python tools/forensics_tool.py fixtures/forensik.pdf

Liefert eine JSON-Karte der gedruckten Struktur je Seite — BEWEISBASIS statt
Ratebasis. Der Agent plant Labels daraus; das Tool erfindet nichts.

Je Seite (Roh-Evidenz mit Belegstelle):
  · textchars        Zeichenzahl der Textschicht (0 = Scan/OCR-fällig)
  · tier1_label      eingebettetes PDF-Label ('' = unbenannt)
  · folio            gedruckte Foliozahl aus Kopf-/Fußzone + Belegschnipsel
  · zone             header|footer|body
  · is_titelei       Verdacht auf Titelei (Impressum-Marker, keine Folio)
  · is_toc           Verdacht auf Inhaltsverzeichnis (IV) - Zeilen ".....S."

Sprünge/Anomalien in der Folio-Sequenz werden als strukturelle Hinweise
gelistet — nicht als Ratebasis, sondern nur als Beleg für Label-Abweichungen.
Titelei-/IV-Merkmale sind zeilengebundene STARKE Marker (z. B. ISBN/Auflage/
Impressum statt bloßem „Verlag" in Fließtext), damit Prosa keine Titelei
vortäuscht.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))  # standalone: `python tools/forensics_tool.py …`

import pymupdf  # type: ignore[reportMissingImports]  # noqa: E402

from tools import pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402

# STARKE Titelei/Impressum-Merkmale (Wortgrenzen; „Verlag" allein zählt
# NICHT — Prosa wie „erschien im Springer Verlag" darf keine Titelei sein).
_TITEL_LIKE = re.compile(
    r"\bimpressum\b|\bauflage\b|\bisbn\b|\bcopyright\b|©|\burheberrecht\b|"
    r"\btyposkript\b|\beigenverlag\b|\bdruckleitung\b",
    re.I,
)
# IV-Überschrift als starke Zeilen-Marker (Wortgrenzen, kein Teilstring-
# Match mehr: „Indexierung" ist kein Register). Punkte-Führungslinien
# erkennt stattdessen _looks_toc() zeilenweise über _TOC_ROW_RE.
_TOC_LIKE = re.compile(
    r"\binhaltsverzeichnis\b|\btable of contents\b|\bcontents\b|"
    r"\bsachverzeichnis\b|\bindex\b",
    re.I,
)
_FOLIO_CELL = re.compile(r"(?m)^\s*(\d{1,4})\s*$")
# IV-Zeile: Punkte-Führungslinie + optionale "Seite/N"-Spalte am Zeilenende.
_TOC_ROW_RE = re.compile(r"\.{2,}\s*(?:s\.?\s*)?(?:seite\s*)?\d+\s*$", re.I)


def _body_text(page: pymupdf.Page) -> str:
    return str(page.get_text("text"))


def page_truth(page: pymupdf.Page, lab: str) -> dict:
    """Roh-Evidenz einer Seite (nur Lesen); `lab` = Tier-1-Label (bereits
    IndexError-sicher via pdf_kernel.read_page_labels geladen)."""
    t = _body_text(page)
    chars = len(t.strip())
    zones = pdf_kernel._zone_texts(page)  # gleiche Zonen-Primitive wie der Kernel
    folio = None
    zonename = None
    beleg = ""
    for zone in ("header", "footer"):
        m = _FOLIO_CELL.search(zones[zone])
        if m:
            folio = m.group(1)
            zonename = zone
            beleg = zones[zone].strip()[:48]
            break
    is_titelei = bool(_TITEL_LIKE.search(t)) and folio is None
    is_toc = bool(_TOC_LIKE.search(t)) or (
        folio is None and chars > 0 and not is_titelei and _looks_toc(t)
    )
    return {
        "textchars": chars,
        "text_layer": chars > 0,
        "tier1_label": lab,
        "folio": folio,
        "folio_zone": zonename,
        "folio_beleg": beleg,
        "is_titelei": is_titelei,
        "is_toc": is_toc,
    }


def _looks_toc(t: str) -> bool:
    """Verdacht auf IV: Zeilen mit Punkte-Führungslinien + Seitenzahl-Ende."""
    n = 0
    for ln in t.splitlines():
        ln = ln.strip()
        if _TOC_ROW_RE.search(ln):
            n += 1
    return n >= 2


def build_map(pdf: str | Path) -> dict:
    doc = pymupdf.open(str(pdf))  # EIN Öffnen für Karte + IV-Zeilen
    labels = pdf_kernel.read_page_labels(str(pdf))  # IndexError-sicher
    pages = [page_truth(doc[i], labels[i]) for i in range(doc.page_count)]
    toc_lines = _collect_toc_lines(doc)
    doc.close()

    # Folio-Sequenz-Analyse (nur Hinweis, nirgends geraten):
    folio_vals = [p["folio"] for p in pages if p["folio"] is not None]
    folio_ints = [
        x for x in (pdf_kernel.to_int_or_none(v) for v in folio_vals) if x is not None
    ]
    pairs = list(zip(folio_ints, folio_ints[1:], strict=False))
    monotone = all(b == a + 1 for a, b in pairs)
    anomalies = []
    if folio_ints:
        for i in range(1, len(folio_ints)):
            if folio_ints[i] != folio_ints[i - 1] + 1:
                anomalies.append(
                    {
                        "prev_folio": folio_ints[i - 1],
                        "next_folio": folio_ints[i],
                        "gap": folio_ints[i] - folio_ints[i - 1] - 1,
                    }
                )

    titelei_count = sum(1 for p in pages if p["is_titelei"])
    toc_count = sum(1 for p in pages if p["is_toc"])

    return {
        "source": str(pdf),
        "page_count": len(pages),
        "pages": pages,
        "folio_sequence_monotonic": monotone,
        "folio_anomalies": anomalies[:20],
        "titelei_pages": titelei_count,
        "toc_pages": toc_count,
        "toc_lines": toc_lines[:30],
    }


def _collect_toc_lines(doc: pymupdf.Document) -> list[str]:
    """Alle Punkte-Führungszeilen aus den ersten Seiten (Titelei/IV-Block)."""
    lines = []
    limit = min(6, doc.page_count)
    for i in range(limit):
        for ln in str(doc[i].get_text("text")).splitlines():
            ln = ln.strip()
            if ln and _TOC_ROW_RE.search(ln):
                lines.append(f"seite_{i}: {ln[:60]}")
        if len(lines) > 40:
            break
    return lines


# Mindestlänge eines Folio-Ankerlaufs (Qualitäts-Tor): strenge
# seitengenaue +1-Läufe ab dieser Länge; vereinzelte Werte und Konstanten
# (z. B. Jahreszahlen) überleben das Tor nicht. Dokumentierte Grenze: ein
# DURCHGEHENDER +1-Zähler anderer Herkunft täuscht einen Lauf vor —
# Upgrade-Pfad wäre eine Zonen-/Typografie-Kreuzprüfung der Folio-Zellen.
ANCHOR_RUN_MIN_LEN = 5


def anchor_folio_run(m: dict, min_len: int = ANCHOR_RUN_MIN_LEN) -> list[dict]:
    """STELLE-1-Quelle (Owner-Ruling 23.08.): beweisbare Folio-Anker aus
    der Druckstruktur-Karte — der längste strenge seitengenaue +1-Lauf
    (min_len als Rausch-Filter). Vereinzelte Werte und Konstanten wie
    Jahreszahlen überleben das Tor nicht; dokumentierte Grenze: ein
    durchgehender +1-Zähler anderer Herkunft besteht das Tor. Rückgabe:
    [{page, folio}] des längsten Laufs; [] = NICHT messbar → Diagnose
    muss verweigern (kein Raten)."""
    folio = {}
    for i, p in enumerate(m["pages"]):
        v = pdf_kernel.to_int_or_none(p["folio"] or "")
        if v is not None:
            folio[i] = v
    if not folio:
        return []
    best: list[tuple[int, int]] = []
    cur: list[tuple[int, int]] = []
    for i in sorted(folio):
        if cur and i == cur[-1][0] + 1 and folio[i] == cur[-1][1] + 1:
            cur.append((i, folio[i]))
        else:
            cur = [(i, folio[i])]
        if len(cur) > len(best):
            best = list(cur)
    if len(best) < min_len:
        return []  # nur Rauschen — kein messbarer Ankerlauf
    return [{"page": i, "folio": str(v)} for i, v in best]


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="forensics_tool")
    p.add_argument("pdf")
    a = p.parse_args(argv)
    print(json.dumps(build_map(a.pdf), ensure_ascii=False, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
