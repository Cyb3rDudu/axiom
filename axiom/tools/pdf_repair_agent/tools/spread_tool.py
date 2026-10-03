"""T1 spread_tool — Doppelseiten-Erkennung & geometrische Trennung (Dry-Run-Default).

Eigenständig aufrufbar (Dry-Run = Split-Plan OHNE Schreibzugriff):
    .venv/bin/python tools/spread_tool.py fixtures/doppelseiten.pdf
    .venv/bin/python tools/spread_tool.py fixtures/doppelseiten.pdf --apply

Erkennt auf jeder Seite, ob sie ein 2-up-Scan ist (Landschafts-Doppelseite):
  · Verhältnis w/h ≥ 1.25 (Landschaft)
  · leeres zentrales Bundsteg-Band im Fenster 30–70 % der Breite: die
    vertikale Dunkel-Projektion (Anteil dunkler Pixel je Spalte, geglättet)
    hat ihr Minimum dort, wo kein Text steht — der Bundsteg zwischen den
    beiden gedruckten Seiten. Beide Signale sind NÖTIG (Stärke-Furche UND
    Lage-Nähe zur Mitte): eine zufällig mittige Lücke ohne nachweisbare
    Furchen-Stärke klassifiziert NICHT als Spread.

Split-Plan (Roh-Evidenz) je Seite: Gutter-Fraktion, Konfidenz, Schätzung der
gedruckten Zugehörigkeit. label_formula liefert die erwarteten Blatt-Labels
über die Owner-Formel (left = 2*spread + offset) — die Zahl-Basis, keine
OCR, keine Schöpfung. --apply trennt geometrisch (Clip je Halbseite, ohne
OCR! OCR ist T2). Die Labels der Blätter werden ausschließlich über
pdf_kernel.write_page_labels gesetzt (der EINZIGE Label-Schreibpfad).
Nicht PDF-darstellbare Soll-Mappings (z. B. numerischer Lauf mit
nachfolgender unbenannter Durchreichseite) werden EHRLICH verweigert und
berichtet (labels_written: false) — nie falsch befüllt.

Hinweis zur "verlustarmen" Trennung: die Halbseiten werden mit 150 dpi
gerastert (Pixmaps) — Text bleibt gut lesbar, ist aber keine 1:1-Kopie
der Original-Inhaltsströme.

Kein Modell schreibt Bytes: Trennung = deterministische geometrische Clips.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
import tempfile
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))  # standalone: `python tools/spread_tool.py …`

from tools import pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402

DEFAULT_OFFSET = 56  # Owner-Formel #188: left = 2*spread + OFFSET
_IDS = "landscape2up"
# C5: Beide Signale nötig — ohne echte Furchen-Stärke (leeres Mittiband über
# dem Rauschen) ist die Lage-Nähe allein KEIN Beweis für einen Bundsteg.
MIN_GUTTER_STRENGTH = 1.0
CONF_THRESHOLD = 0.35


def detect_spread(page: pymupdf.Page) -> dict:
    """Bundsteg-Band per vertikaler Dunkel-Projektion (Fenster 30–70 %).

    Die Projektion zählt dunkle Pixel je Spalte; geglättet ihr Minimum =
    die leerste zentrale Spalte (dort steht kein Text — der Bundsteg).
    is_spread verlangt BOTH: Furchen-Stärke ≥ MIN_GUTTER_STRENGTH
    (Mindestabstand zur Spalten-Mediane) UND Konfidenz ≥ CONF_THRESHOLD
    (Stärke + Nähe zur Mitte) — damit eine zentrierte Lücke ohne echte
    Furche (z. B. Wortlücke) nicht fälschlich splittet.
    """
    w, h = page.rect.width, page.rect.height
    ratio = w / h
    if ratio < 1.25:  # nicht deutlich breiter als hoch => kein Doppelseiten-Scan
        return {
            "id": _IDS,
            "is_spread": False,
            "why": f"ratio {ratio:.2f} < 1.25 (kein Breit-Scan)",
        }
    pix = page.get_pixmap(dpi=40, colorspace=pymupdf.csGRAY)
    width, height, stride = pix.width, pix.height, pix.stride
    samples = pix.samples
    cols = []
    for x in range(width):
        dark = sum(1 for y in range(0, height, 2) if samples[y * stride + x] < 128)
        cols.append(dark)
    # box-smoothed (Fenster 4) über die Projektion
    win = 4
    smooth = [sum(cols[max(0, i - win) : i + win]) for i in range(width)]
    lo, hi = width * 30 // 100, width * 70 // 100
    band = smooth[lo:hi]
    g = lo + band.index(min(band))
    gutter_frac = g / width
    # Furchen-Stärke: wieviel leerer ist das Mittiband gegen die Spalten-Mediane
    med = sorted(cols)[len(cols) // 2]
    trough = cols[g]
    strength = max(0.0, med - trough)
    # Konfidenz: 0..1, gewichtet Furchen-Stärke + Nähe zu 0.5 (echter Bindung)
    closeness = 1.0 - abs(gutter_frac - 0.5) / 0.2
    conf = math.tanh(strength) * 0.6 + max(0.0, closeness) * 0.4
    conf = round(min(1.0, conf), 2)
    is_spread = _classify(strength, conf)
    if not is_spread and strength < MIN_GUTTER_STRENGTH:
        why = f"Furchen-Stärke {strength:.1f} < {MIN_GUTTER_STRENGTH} (kein Bundsteg-Beweis)"
    else:
        why = f"Konfidenz {conf} < {CONF_THRESHOLD}"
    out = {
        "id": _IDS,
        "is_spread": is_spread,
        "gutter": round(gutter_frac, 3),
        "strength": round(strength, 1),
        "confidence": conf,
        "why": why if not is_spread else "ratio+Furche erfüllt",
    }
    if is_spread:
        out["labelled_leaves"] = 2  # nur echte Spreads erzeugen Blatt-Labels
    return out


def _classify(strength: float, conf: float) -> bool:
    """Spread-Urteil als reine Funktion: BEIDE Signale nötig — Stärke-Furche
    ≥ MIN_GUTTER_STRENGTH UND Konfidenz ≥ CONF_THRESHOLD. (Löst den C5-Fall:
    zentrierte Lücke ohne nachweisbare Furche klassifiziert NICHT als Spread.)"""
    return strength >= MIN_GUTTER_STRENGTH and conf >= CONF_THRESHOLD


def label_formula(page_idx: int, offset: int = DEFAULT_OFFSET) -> tuple[int, int]:
    """Owner-Formel (#188): Blatt-Labels der zwei Halbseiten einer Spread.
    spread (1-basiert) = page_idx + 1; left = 2*spread + offset; right = left+1."""
    spread = page_idx + 1
    left = 2 * spread + offset
    return left, left + 1


def _plan(pdf: str | Path, offset: int) -> dict:
    doc = pymupdf.open(str(pdf))
    pages = []
    for i in range(doc.page_count):
        d = detect_spread(doc[i])
        if d["is_spread"]:
            left, right = label_formula(i, offset)
            d["labels"] = [left, right]
            d["label_formula"] = f"left=2*spread+{offset}, right=left+1"
        pages.append({"page_index": i, **d})
    ratio = (doc[0].rect.width / doc[0].rect.height) if doc.page_count else 0
    doc.close()
    return {
        "source": str(pdf),
        "landscape_ratio": round(ratio, 2),
        "offset": offset,
        "pages": pages,
        "assessment": _assess(pages),
    }


def _assess(pages) -> str:
    n = sum(1 for p in pages if p["is_spread"])
    total = len(pages)
    if total and n == total:
        return "2-up-Scan über alle Seiten (Reparaturfall: Spread)"
    if n == 0:
        return "keine Landschafts-2-up-Spreads erkannt"
    return f"teilweise Spreads: {n}/{total} Seiten"


def split_and_write(
    pdf: str | Path, dst: Path, offset: int, detections: list[dict] | None = None
) -> dict:
    """--apply: geometrische Trennung je Spread -> einzelne Blatt-Seiten.

    `detections` (optional) = bereits berechnete Plan-Einträge (_plan), damit
    main() die Erkennung nicht doppelt läuft. Die Blatt-Labels werden
    ausschließlich über pdf_kernel.write_page_labels gesetzt (EINZIGER
    Schreibpfad): nicht PDF-darstellbare Soll-Mappings (z. B. numerischer
    Lauf + nachfolgende unbenannte Durchreichseite) werden verweigert und
    EHRLICH berichtet (labels_written: false) — nie falsch befüllt.
    """
    dst = Path(dst)
    src = pymupdf.open(str(pdf))
    out = pymupdf.open()
    labels: list[str] = []
    for i in range(src.page_count):
        d = detections[i] if detections is not None else detect_spread(src[i])
        if not d["is_spread"]:
            # kein Spread: Seite 1:1 übernehmen, Label unverändert lassen ('' )
            out.insert_pdf(src, start_at=i, from_page=i, to_page=i)
            cur = src[i].get_label() or ""
            labels.append(cur)
            continue
        page = src[i]
        w, h = page.rect.width, page.rect.height
        g = d["gutter"]
        left_lab, right_lab = label_formula(i, offset)
        halves = [((0, g * w), left_lab), ((g * w, w), right_lab)]
        for (x0, x1), lab in halves:
            clip = pymupdf.Rect(x0, 0, x1, h)
            pix = page.get_pixmap(dpi=150, clip=clip)
            pbytes = pix.tobytes("png")
            temp = pymupdf.open()
            tpage = temp.new_page(width=pix.width, height=pix.height)
            tpage.insert_image(tpage.rect, stream=pbytes)
            out.insert_pdf(temp)
            temp.close()
            labels.append(str(lab))
    src.close()
    dst.parent.mkdir(parents=True, exist_ok=True)
    # Crash-atomar: volle Save auf Temp im Zielverzeichnis, dann os.replace.
    fd, tmp = tempfile.mkstemp(suffix="-split.pdf", dir=dst.parent)
    os.close(fd)
    try:
        out.save(tmp, deflate=True)
        os.replace(tmp, dst)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    out.close()
    # Label-Schreibzugriff NUR über den Kernel (Range-Repräsentabilitäts-Wächter).
    report: dict = {
        "output": str(dst),
        "leaf_count": len(labels),
        "labels": labels,
    }
    try:
        pdf_kernel.write_page_labels(dst, labels)
    except ValueError as e:
        report["labels_written"] = False
        report["labels_cause"] = str(e)
    else:
        report["labels_written"] = True
    got = pdf_kernel.read_page_labels(dst)
    report["readback"] = got
    if report["labels_written"]:
        if got != labels:
            raise RuntimeError(  # Kodex: niemals stille Falschheit ausliefern
                f"Label-Readback != Soll nach Split-Schreibzugriff: {got} != {labels}"
            )
        report["readback_matches"] = True
    else:
        report["readback_matches"] = got == labels
    return report


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="spread_tool", description="Doppelseiten-Erkennung/-Trennung"
    )
    p.add_argument("pdf", help="Eingabe-PDF")
    p.add_argument("--apply", action="store_true", help="wirklich trennen")
    p.add_argument("-o", "--output", help="Ziel für --apply")
    p.add_argument(
        "--offset",
        type=int,
        default=DEFAULT_OFFSET,
        help="Owner-Formel-Offset (Default 56)",
    )
    a = p.parse_args(argv)
    plan = _plan(a.pdf, a.offset)
    print(json.dumps(plan, ensure_ascii=False, indent=1))
    if a.apply:
        if not plan["pages"] or not any(pp["is_spread"] for pp in plan["pages"]):
            print(
                json.dumps(
                    {
                        "applied": False,
                        "cause": "keine Spreads erkannt — nichts zu trennen",
                    },
                    ensure_ascii=False,
                )
            )
            return 1
        dst = Path(a.output) if a.output else HERE.parent / "runs" / "spread_split.pdf"
        res = split_and_write(a.pdf, dst, a.offset, detections=plan["pages"])
        res["applied"] = True
        print(json.dumps(res, ensure_ascii=False, indent=1))
        if not res["labels_written"]:
            return 1  # Teil-Ergebnis: Trennung da, Labels ehrlich verweigert
    return 0


if __name__ == "__main__":
    sys.exit(main())
