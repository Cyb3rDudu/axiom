"""T5 scan_ocr_rebuild — #284: OCR-Rebuild der scan_ohne_textlayer-Klasse.

Heilt BEIDE Befunde der Klasse (Owner-Pilot 2026-09-17, Queckenberg 241 S.
und Bartscher 658 S. — Kommandoform end-to-end bewiesen):

  1. Reiner Bildscan (keine Textschicht):
     ocrmypdf -l <lang> --oversample 300 --output-type pdf --optimize 1
  2. Kaputte Textschicht (digital geboren, defekte Worttrennung —
     Wörter ohne Leerzeichen konkateniert; Produktionsfall Reder 2024
     SSOAR): zusätzlich --force-ocr, rasterisiert die kaputte Vektor-
     Textschicht weg und ersetzt sie durch die OCR-Schicht.

Owner-Rulings, festgenagelt:
  - ungedrosselt: keine --jobs/OMP-Drossel in der Normalverarbeitung
    (Pilot-Drossel war Wellen-Koexistenz, kein Produktbedarf);
  - Sprachdefault `deu` (deu schlug deu+eng im Pilot: „Universitit"-Klasse
    Fehler im Kombimodus), übersteuerbar je Case (--lang / Metadaten);
  - kein Deskew per Default (Pilot: Seiten waren gerade; unnötige Bild-
    Verarbeitung kostet Qualität) — als Option da.

Ehrlichkeits-Gates (KEINE stille Lüge, dieselbe Disziplin wie ocr_tool):
  fehlen tesseract/gs/ocrmypdf → Unbelegbarkeit gemeldet, nichts geschrieben.
  Das Ergebnis-Verifikat prüft: Seitenzahl UNVERÄNDERT, Seitengeometrie
  UNVERÄNDERT, Textschicht VORHANDEN (aggregate Mean, kein strenges
  Je-Seite-Tor — echte Vakat-/Titelei-Seiten dürfen leer bleiben).

Die Folio-Heilung (2-in-1: Scan + Labels in EINER Heilung) läuft beim
AUFRUFER (repair_agent-Katalog-Regel) über labeltree_heal auf der NEUEN
Textschicht — dieses Werkzeug liefert nur die neue Schicht.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))  # standalone: `python tools/scan_ocr_rebuild.py …`

from tools import ocr_tool, pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402

ocrmypdf_bin = ocr_tool.ocrmypdf_bin  # noqa: E402 — re-export (single import block, sorted)

# Owner-Ruling: deu schlug deu+eng im Pilot (Kombimodus erzeugte Fehler
# wie „Universitit"). Aufrufer (Invoker) übergibt den Metadaten-Default.
OCR_LANG_DEFAULT = "deu"

# Aggregates Textmaß der NEUEN Schicht: Mean-Zeichen je Seite (Vakat-Seiten
# einer echten Ausgabe dürfen den Aggregate nicht brechen — das strenge
# Je-Seite-Tor von ocr_tool (MIN_TEXT_CHARS) passt nicht auf echte Bücher).
MIN_MEAN_CHARS_PER_PAGE = 50

# Zeitbudget: Basis + je Seite. 658-Seiten-Rebuilds passen nicht in den
# 35-min-Fixer-Timeout des Normalfalls — der Invoker setzt das Klassen-
# Budget (Config.OCRTimeout); diese Deckelung ist die letzte Instanz im
# Werkzeug selbst (Aufrufer kann sie per Parameter/an Env anheben).
OCR_TIMEOUT_BASE_S = 120
OCR_TIMEOUT_PER_PAGE_S = 6


def _timeout_s(pages: int) -> int:
    import os

    if v := os.environ.get("AXIOM_OCR_TIMEOUT_S"):
        try:
            return int(v)
        except ValueError:
            pass
    return OCR_TIMEOUT_BASE_S + pages * OCR_TIMEOUT_PER_PAGE_S


def _bins_available() -> dict:
    """Ehrliche Binär-Bilanz: env-relativ gebündelte Binaries zuerst
    (#286, Artifact-Standard — Carrier ohne Host-tesseract/gs), dann
    PATH-Fallback (Dev-Maschine)."""
    return {
        "tesseract": ocr_tool.bundled_bin("tesseract") is not None,
        "gs": ocr_tool.bundled_bin("gs") is not None,
        "ocrmypdf": ocrmypdf_bin() is not None,
    }


def _text_layer_metrics(pdf: str | Path) -> dict:
    counts = pdf_kernel.page_char_count(pdf)
    total = sum(counts)
    n = len(counts)
    return {
        "pages": n,
        "total_chars": total,
        "mean_chars_per_page": round(total / n, 1) if n else 0,
    }


def _page_dims(pdf: str | Path) -> list[tuple[float, float]]:
    import pymupdf  # type: ignore[import-not-found] — lazy, venv-only

    doc = pymupdf.open(str(pdf))
    try:
        return [(round(p.rect.width, 2), round(p.rect.height, 2)) for p in doc]
    finally:
        doc.close()


def _is_spaceless_script_text(text: str) -> bool:
    """Anteil space-loser Schriften (CJK u. a.): Diese Schriften trennen
    Wörter NOTORISCH ohne Leerzeichen — space_ratio ~0 ist dort INTAKT,
    nicht kaputt. Schwellen: >30% CJK/Hangul/Kana-Zeichen (reine
    Bopomofo-Texte fängt der Han-Anteil realer Dokumente mit auf)."""
    if not text:
        return False
    spaceless = 0
    for ch in text:
        o = ord(ch)
        if (
            0x4E00 <= o <= 0x9FFF      # CJK Unified
            or 0x3400 <= o <= 0x4DBF   # CJK Ext A
            or 0x3040 <= o <= 0x30FF   # Hiragana/Katakana
            or 0xAC00 <= o <= 0xD7AF   # Hangul syllables
            or 0x31F0 <= o <= 0x31FF   # Katakana phonetic ext
        ):
            spaceless += 1
    return spaceless / len(text) > 0.30


def diagnose(pdf: str | Path) -> dict:
    """Dry-Run-Befund: Textschicht da? Kaputt (Worttrennung)? Werkzeuge da?

    Die defekte Worttrennung (Reder-Klasse) ist deterministisch messbar:
    Leerzeichen-Dichte und mittlere Wortlänge. Deutsche Prosa liegt bei
    ~0.15 Leerzeichen/Zeichen und ~6 Zeichen/Wort; konkatenierter Text
    bricht beide Schwellen deutlich. Space-lose Schriften (CJK) treffen
    dieselben Schwellen INTAKT — sie werden explizit erkannt und NIEMALS
    als kaputt klassifiziert (force-ocr --lang deu würde sie zerstören).
    """
    bins = _bins_available()
    import pymupdf  # type: ignore[import-not-found]

    doc = pymupdf.open(str(pdf))
    try:
        n = doc.page_count
        total_chars = 0
        total_spaces = 0
        image_pages = 0
        for page in doc:
            t = str(page.get_text("text"))
            total_chars += len(t.strip())
            total_spaces += t.count(" ")
            if page.get_images(full=True):
                image_pages += 1
        text_layer = total_chars > 0
        space_ratio = (total_spaces / total_chars) if total_chars else 0.0
        words = [w for page in doc for w in page.get_text("words")]
        mean_word_len = sum(len(w[4]) for w in words) / len(words) if words else 0.0
        sample = "".join(str(p.get_text("text"))[:2000] for p in doc)
        spaceless_script = _is_spaceless_script_text(sample)
    finally:
        doc.close()
    # Schwellen (dokumentiert, konservativ): normale Prosa kommt nie in
    # die Nähe; konkatenierter Text (Reder-Pilot) bricht beide.
    broken_segmentation = (
        text_layer and space_ratio < 0.06 and mean_word_len > 12 and not spaceless_script
    )
    # Reiner Scan braucht Raster-Evidenz (wie das Preflight-Muster „viele
    # reine Bildseiten“): ein text- UND bildfreies Vektor-PDF ist kein
    # Scan — OCR könnte nichts liefern, die Datei fällt an die normale
    # Klassifikation zurück (die #258-Gate-Tests der Blank-PDFs bleiben
    # unberührt).
    raster_scan = not text_layer and n > 0 and image_pages >= max(1, n // 2)
    ocr_ok = all(bins.values())
    if raster_scan:
        mode, verdict = "plain", "scan_ohne_textlayer"
    elif broken_segmentation:
        mode, verdict = "force", "kaputte_textschicht"
    else:
        mode, verdict = None, "not_scan_class"
    return {
        "source": str(pdf),
        "pages": n,
        "text_layer": text_layer,
        "total_chars": total_chars,
        "image_pages": image_pages,
        "raster_scan": raster_scan,
        "space_ratio": round(space_ratio, 4),
        "mean_word_len": round(mean_word_len, 2),
        "spaceless_script": spaceless_script,
        "broken_segmentation": broken_segmentation,
        "mode": mode,
        "class": verdict,
        "ocr_available": ocr_ok,
        "ocr_binaries": bins,
        "ocr_verdict": "possible"
        if ocr_ok and mode
        else (
            "not_needed"
            if not raster_scan and not broken_segmentation
            else "unable_no_binaries"
        ),
    }


def run_rebuild(
    pdf: str | Path,
    dst: Path,
    lang: str = OCR_LANG_DEFAULT,
    force: bool = False,
    deskew: bool = False,
    timeout_s: int | None = None,
) -> dict:
    """--apply: OCRmyPDF-Rebuild; Original bleibt (dst ist Kopie).

    Kommandoform = Owner-Pilot (wörtlich): oversample 300, output-type
    pdf, optimize 1; force-Modus rasterisiert die kaputte Vektorschicht.
    """
    bins = _bins_available()
    if not all(bins.values()):
        fehlen = [k for k, ok in bins.items() if not ok]
        return {
            "applied": False,
            "cause": f"OCR-Werkzeuge fehlen: {', '.join(fehlen)} — Textschicht "
            f"nicht baubar; nichts geschrieben (keine stille Lüge)",
            "ocr_binaries": bins,
        }
    src = Path(pdf)
    pages = len(pdf_kernel.page_char_count(src))
    dst = Path(dst)
    dst.parent.mkdir(parents=True, exist_ok=True)
    cmd = [
        ocrmypdf_bin() or "ocrmypdf",
        "--language",
        lang,
        "--oversample",
        "300",
        "--output-type",
        "pdf",
        "--optimize",
        "1",
    ]
    if force:
        cmd.append("--force-ocr")
    if deskew:
        cmd.append("--deskew")
    cmd += ["-q", str(src), str(dst)]
    budget = timeout_s or _timeout_s(pages)
    try:
        # #286: Kind-Umgebung aus dem gebündelten Env (PATH + TESSDATA_PREFIX)
        # — der Rebuild läuft ohne Host-tesseract/gs (Carrier-Szenario).
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=budget, env=ocr_tool.ocr_child_env())
    except subprocess.TimeoutExpired:
        return {
            "applied": False,
            "cause": f"ocrmypdf timeout nach {budget}s (Budget überschritten — Aufrufer-Klassen-Budget prüfen)",
            "cmd": cmd[:-2],
            "timeout_s": budget,
        }
    if r.returncode != 0:
        return {
            "applied": False,
            "cause": f"ocrmypdf rc={r.returncode}: {r.stderr.strip()[:300]}",
            "cmd": cmd[:-2],
            "timeout_s": budget,
        }
    # Ergebnis-Verifikat (ehrlich, aggregate):
    m = _text_layer_metrics(dst)
    dims_ok = _page_dims(dst) == _page_dims(src)
    pages_ok = m["pages"] == pages
    text_ok = m["mean_chars_per_page"] >= MIN_MEAN_CHARS_PER_PAGE
    if not (pages_ok and dims_ok and text_ok):
        dst.unlink(missing_ok=True)  # ABGELEHNT: nichts weitergeben
        return {
            "applied": False,
            "cause": (
                f"Verifikat NICHT bestanden: pages_ok={pages_ok} "
                f"dims_ok={dims_ok} text_ok={text_ok} ({m})"
            ),
            "quality": m,
            "timeout_s": budget,
        }
    return {
        "applied": True,
        "output": str(dst),
        "mode": "force" if force else "plain",
        "language": lang,
        "quality": m,
        "pages": pages,
        "timeout_s": budget,
    }


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="scan_ocr_rebuild")
    p.add_argument("pdf")
    p.add_argument("--apply", action="store_true")
    p.add_argument("-o", "--output")
    p.add_argument("--lang", default=OCR_LANG_DEFAULT)
    p.add_argument(
        "--force",
        action="store_true",
        help="Kaputte Textschicht wegrastern (--force-ocr)",
    )
    p.add_argument("--deskew", action="store_true")
    a = p.parse_args(argv)
    d = diagnose(a.pdf)
    print(json.dumps(d, ensure_ascii=False, indent=1))
    if a.apply:
        dst = (
            Path(a.output) if a.output else HERE.parent / "runs" / "ocr_rebuild_out.pdf"
        )
        res = run_rebuild(a.pdf, dst, lang=a.lang, force=a.force, deskew=a.deskew)
        print(json.dumps(res, ensure_ascii=False, indent=1))
        if not res.get("applied"):
            return 1  # ehrliche Ablehnung/Fehlschlag = Tool-Misserfolg
    return 0


if __name__ == "__main__":
    sys.exit(main())
