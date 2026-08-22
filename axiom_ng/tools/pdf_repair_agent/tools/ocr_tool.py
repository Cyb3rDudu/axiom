"""T2 ocr_tool — Textschicht via ocrmypdf (Qualitätstor, Abhängigkeits-Ehrlichkeit).

Eigenständig aufrufbar (Dry-Run = Befund OHNE Schreiben):
    .venv/bin/python tools/ocr_tool.py fixtures/ohne_textschicht.pdf
    .venv/bin/python tools/ocr_tool.py fixtures/ohne_textschicht.pdf --apply

Pflicht: KEINE stille Lüge. Wenn tesseract, ghostscript oder ocrmypdf
fehlen, wird das als Unbelegbarkeit gemeldet (Textschicht NICHT baubar)
statt zu raten. Wenn sie vorhanden sind, läuft ocrmypdf mit Sprachprofil;
danach erzwingt das Qualitätstor ein Mindestmaß an Textschicht JE SEITE
(Zeichenzahl als Stellvertreter-Messgröße — per-Wort-Konfidenz liefert
tesseract über das PDF nicht): Unterschreitet das Ergebnis die Schwelle,
wird es ABGELEHNT (das Ergebnis verworfen, das Original bleibt), nicht als
geheilt akzeptiert.

Nur dieses Werkzeug ruft ocrmypdf auf; es schreibt keine Labels (das ist
ausschließlich pdf_kernel.write_page_labels über T4).
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))  # standalone: `python tools/ocr_tool.py …`

from tools import pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402

# Qualitätsschwelle (Erfahrungswert, dokumentiert): JE Seite nach OCR
# mindestens so viele Textzeichen — sonst gilt die Seite (und damit das
# Ergebnis) als nicht brauchbar. Das Tor ist streng: leere Vakat-/Titelei-
# Seiten können es auslösen; dann zeigt `pages_below_min` die Belegstellen
# und der Agent entscheidet (Roh-Evidenz statt Pauschalurteil).
MIN_TEXT_CHARS = 80

# tesseract/ocrmypdf-Sprachcodes: "deu" (tesseract-Alt) bzw. "de"
# (neuere Paketnamen wie tesseract-lang-deu). Default hier "deu".
OCR_LANG_DEFAULT = "deu"
OCRMYPDF_TIMEOUT = 900  # s — harte Deckelung statt 1-h-Hang


def ocrmypdf_bin() -> str | None:
    """ocrmypdf-Auflösung: ZUERST im eigenen Venv (sys.prefix/bin — die
    Venv-bin liegt NICHT im PATH, wenn man `.venv/bin/python` direkt
    aufruft; sys.executable.resolve() wäre FALSCH, denn es folgt dem
    Interpreter-Symlink in den nix-Store), dann PATH-Fallback."""
    cand = Path(sys.prefix) / "bin" / "ocrmypdf"
    if cand.is_file():
        return str(cand)
    return shutil.which("ocrmypdf")


def _bins_available() -> dict:
    """Ehrliche Binär-Bilanz: ALLE drei nötigen Werkzeuge, kein Raten.
    ocrmypdf wird venv-bewusst aufgelöst (siehe ocrmypdf_bin)."""
    return {
        "tesseract": shutil.which("tesseract") is not None,
        "gs": shutil.which("gs") is not None,
        "ocrmypdf": ocrmypdf_bin() is not None,
    }


def plan(pdf: str | Path) -> dict:
    """Dry-Run-Befund: fehlt die Textschicht? Sind die OCR-Werkzeuge da?"""
    counts = pdf_kernel.page_char_count(pdf)
    layers = [c > 0 for c in counts]
    missing = [i for i, ok in enumerate(layers) if not ok]
    bins = _bins_available()
    ocr_ok = all(bins.values())
    return {
        "source": str(pdf),
        "pages": len(counts),
        "text_layer_missing_pages": missing,
        "text_layer_ok": all(layers),
        "raster_scan_hypothesis": len(missing) > 0,
        "ocr_available": ocr_ok,
        "ocr_binaries": bins,
        "ocr_verdict": "possible"
        if ocr_ok and missing
        else ("not_needed" if all(layers) else "unable_no_binaries"),
        "deps_status": "ok" if ocr_ok else "unable_no_binaries",
    }


def _quality_report(pdf: str | Path) -> dict:
    """Qualitätstor: Zeichenzahl je Seite (Stellvertreter, kein tesseract-Call).

    Streng JE SEITE: eine einzige dünn bleibende Seite senkt das Tor.
    Per-Seite-Zahlen bleiben als Roh-Evidenz im Bericht.
    """
    counts = pdf_kernel.page_char_count(pdf)
    below = [i for i, c in enumerate(counts) if c < MIN_TEXT_CHARS]
    passed = not below
    return {
        "min_text_chars": MIN_TEXT_CHARS,
        "textchars_pages": counts,
        "textchars_total": sum(counts),
        "pages_below_min": below,
        "quality_gate_pass": passed,
        "reason": "too_few_textchars_pages" if below else "ok",
    }


def run_ocr(pdf: str | Path, dst: Path, lang: str = OCR_LANG_DEFAULT) -> dict:
    """--apply: ocrmypdf Textschicht; Original bleibt (dst ist Kopie)."""
    bins = _bins_available()
    if not all(bins.values()):
        fehlen = [k for k, ok in bins.items() if not ok]
        return {
            "applied": False,
            "cause": f"OCR-Werkzeuge fehlen: {', '.join(fehlen)} — Textschicht "
            f"nicht baubar; nichts geschrieben (keine stille Lüge)",
            "ocr_binaries": bins,
        }
    dst = Path(dst)
    dst.parent.mkdir(parents=True, exist_ok=True)
    ocr_bin = ocrmypdf_bin()
    if ocr_bin is None:  # Doppelte Sicherung — oben schon über bins geprüft
        return {"applied": False,
                "cause": "ocrmypdf-Binär nicht auflösbar (venv-bin/PATH)",
                "ocr_binaries": bins}
    r = subprocess.run(
        [ocr_bin, "--language", lang, "--redo-ocr", "-q", str(pdf), str(dst)],
        capture_output=True,
        text=True,
        timeout=OCRMYPDF_TIMEOUT,
    )
    if r.returncode != 0:
        return {
            "applied": False,
            "cause": f"ocrmypdf rc={r.returncode}: {r.stderr.strip()[:200]}",
        }
    # Qualitätstor:
    q = _quality_report(dst)
    if not q["quality_gate_pass"]:
        dst.unlink(missing_ok=True)  # ABGELEHNT: nichts weitergeben
        return {
            "applied": False,
            "cause": f"Qualitätstor NICHT bestanden ({q['reason']}: Seiten"
            f" {q['pages_below_min']}); Ergebnis verworfen, Original unberührt",
            "quality": q,
        }
    return {"applied": True, "output": str(dst), "quality": q}


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="ocr_tool")
    p.add_argument("pdf")
    p.add_argument(
        "--apply",
        action="store_true",
        help="Textschicht bauen (nur falls Werkzeuge da)",
    )
    p.add_argument("-o", "--output")
    p.add_argument("--lang", default=OCR_LANG_DEFAULT)
    a = p.parse_args(argv)
    pl = plan(a.pdf)
    print(json.dumps(pl, ensure_ascii=False, indent=1))
    if a.apply:
        dst = Path(a.output) if a.output else HERE.parent / "runs" / "ocr_out.pdf"
        res = run_ocr(a.pdf, dst, a.lang)
        print(json.dumps(res, ensure_ascii=False, indent=1))
        if res.get("applied") != True:  # noqa: E712
            return 1  # ehrliche Ablehnung/Fehlschlag = Tool-Misserfolg
    return 0


if __name__ == "__main__":
    sys.exit(main())
