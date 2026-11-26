"""#395: positions- und dominanzbasierte Folio-Messung + Label-Klassifikation.

Deterministische Stelle-1-Katalog-Regel für die T1-Label-Klasse
„Tree vorhanden, aber falsch" — heilt ohne Modell und ohne Owner, wenn
die Druckseite ein sauberes Signal trägt.

Messmodell (produktionsbewiesen an 9 christine-Büchern, Fixtures
tests/fixtures/fixer395/):

  · BÄNDER: Folio-Kandidaten sind NUR eigenständige Tokens
    (eine Zeile = eine Zahl bzw. römische Zahl) im Kopf-/Fußband
    (±9 % der Seitenhöhe). Alles andere (Jahreszahlen, Kapitelnummern,
    Verlags-IDs im Fließtext) ist kein Kandidat — die alte, chaotische
    Karte war ein Messartefakt, kein Buchdefekt.

  · DOMINANZ: Offset-Histogramm über alle Kandidaten; der dominante
    Delta ist der Signal-Anker. Ausreißer im Band (Jahre, Kapitelnummern)
    stützen den dominanten Lauf nicht — sie dürfen die Klassifikation
    nicht brechen, werden aber als Rauschen berichtet.

  · STUFENPROFIL: print-Nummerierung, die an unnummerierten
    Kapitel-Grenzen um genau EINE Nummer vorausläuft (gedroppte
    Beilage im Scan), erzeugt ein monoton steigendes Stufenprofil in
    konstanten +1-Schritten. Das ist ein STARKES Signal: ein Many-Range-
    Baum mit einer Range pro Segment heilt automatisch. Grenzkonvention
    (Owner, Produktion): Grenzlabel = next_start − 1, d. h. jede
    D-Range beginnt auf der unnummerierten Grenzseite selbst.

  · Range-Start-Regel (vereinheitlicht Front und Interior): eine
    D-Range beginnt eine Seite VOR ihrem ersten Anker (firstpagenum =
    Anker-Folio − 1), sofern diese Seite keine Ziffer trägt — sonst auf
    der Ankerseite selbst. Reproduziert die Referenzbäume aus
    ALL_TREES.json; der römische Vorspann ist gemäß Stil-Politik ein
    VORSCHLAG (r ab I), außer eine saubere, durchgehende römische Kette
    reicht bis an die Arabic-Grenze (dann adoptierte Startzahl + Case).

  · REFUSAL-DISZIPLIN bleibt: nicht erklärbare Stufen (Schritt ≠ +1,
    konsistente Restläufe, Echo-Nummern), zu dünne Ankerbasis oder
    widersprüchliches Band-Rauschen parken den Fall — der Fix ist
    besseres Messen, nicht ein niedrigeres Tor.

Nur pymupdf (Paket-Disziplin, import_audit).
"""

from __future__ import annotations

import re
from collections import Counter
from pathlib import Path

import pymupdf  # type: ignore[reportMissingImports]

# Kopf-/Fußband (±9 % Seitenhöhe) — die Sonde der Produktions-Evidenz.
BAND = 0.09
_DIGIT_TOKEN = re.compile(r"\d{1,4}")
# Römische Tokens: ≥ 2 Zeichen (Einzelbuchstaben D/C/M/V/X/I sind
# Kapitel-/Verlags-Rauschen, wie die Produktion gezeigt hat).
_ROMAN_TOKEN = re.compile(r"(?i)^(?=[mdclxvi]{2,7})m*(cm|cd|d?c{0,3})(xc|xl|l?x{0,3})(ix|iv|v?i{0,3})$")
_STRICT_ROMAN = re.compile(
    r"^m{0,4}(cm|cd|d?c{0,3})(xc|xl|l?x{0,3})(ix|iv|v?i{0,3})$", re.IGNORECASE
)

# Qualitäts-Tore (auf Inlier-Basis, #395-Ziel 2). Ankerzahl ≥ 5 wie das
# forensics_tool-Anchor-Tor; Rauschseiten ≤ 25 % der Ziffer-Seiten.
MIN_TOTAL_ANCHORS = 5
MIN_SEGMENT_ANCHORS = 2
MAX_NOISE_PAGE_RATIO = 0.25
# Eine konsistente Rest-Delta-Familie ab dieser Seitenzahl ist kein
# Rauschen mehr, sondern ein nicht erklärtes Segment (Echo/Drift-Rest)
# → Verweigerung statt Ratebasis.
MIN_UNEXPLAINED_FAMILY = 3
# Grenz-Lokalität: erster Anker der Nachbarszene liegt höchstens zwei
# Seiten nach dem letzten Anker der aktuellen (kanonisch: Anker,
# unnummerierte Grenzseite, Anker).
_MAX_BOUNDARY_DIST = 3


def _roman_value(tok: str) -> int | None:
    if not _STRICT_ROMAN.match(tok):
        return None
    vals = {"i": 1, "v": 5, "x": 10, "l": 50, "c": 100, "d": 500, "m": 1000}
    s = tok.lower()
    total = 0
    for ch, v in zip(s, s[1:] + " ", strict=False):
        nv = vals.get(v, 0)
        if vals[ch] < nv:
            total -= vals[ch]
        else:
            total += vals[ch]
    return total


def _roman_render(v: int, upper: bool) -> str:
    table = [(1000, "m"), (900, "cm"), (500, "d"), (400, "cd"), (100, "c"),
             (90, "xc"), (50, "l"), (40, "xl"), (10, "x"), (9, "ix"),
             (5, "v"), (4, "iv"), (1, "i")]
    out = ""
    for n, sym in table:
        while v >= n:
            out += sym
            v -= n
    return out.upper() if upper else out


def measure_bands(pdf: str | Path) -> dict[int, dict]:
    """Folio-Kandidaten je Seite (0-basiert): eigenständige Tokens im
    Kopf-/Fußband. Wort-Ebene (get_text 'words'), weil laufende Köpfe das
    Folio als letztes Wort der Zeile tragen (Produktion SEMXFCMK:
    'The power of algorithms…\u2003 45') — zeilenbasierte Voll-Matches
    verlören genau diese. Rückgabe {seite0: {"digits": [int],
    "roman": [str]}}."""
    doc = pymupdf.open(str(pdf))
    try:
        out: dict[int, dict] = {}
        for i in range(doc.page_count):
            page = doc[i]
            r = page.rect
            h = r.height
            digits: set[int] = set()
            roman: set[str] = set()
            for clip in (
                pymupdf.Rect(r.x0, r.y0, r.x1, r.y0 + h * BAND),
                pymupdf.Rect(r.x0, r.y1 - h * BAND, r.x1, r.y1),
            ):
                for w in page.get_text("words", clip=clip):
                    tok = w[4].strip()
                    if not tok:
                        continue
                    if _DIGIT_TOKEN.fullmatch(tok):
                        digits.add(int(tok))
                    elif _ROMAN_TOKEN.match(tok) and _roman_value(tok):
                        roman.add(tok)
            if digits or roman:
                out[i] = {"digits": sorted(digits), "roman": sorted(roman)}
        return out
    finally:
        doc.close()


def _anchors(meas: dict[int, dict], d0: int) -> set[int]:
    """Anker-Seiten (0-basiert) für 0-basiertes Delta d0: Seite trägt das
    erwartete Label (i + d0) als Ziffer-Token."""
    return {
        i for i, m in meas.items() if any(v == i + d0 for v in m["digits"])
    }


def _expand(meas: dict[int, dict], d_star: int) -> list[dict] | None:
    """Baut die Segmentkette um das dominante Delta: +1-Schritte an
    unnummerierten Grenzen, je Seite der kanonischen Grenzform
    (Anker · unnummerierte Seite · Anker, Sprung +3) oder der
    Adjazenzform (Anker · Anker, Sprung +2). Rückgabe None, sobald eine
    Grenzform die Disziplin bricht (nummerierte Grenzseite,
    Kettenschritt ≠ +1) — die Aufruferin verwirft dann die gesamte
    Klassifikation (Refusal statt Ratebasis)."""
    digit_pages = {i for i, m in meas.items() if m["digits"]}
    seed = _anchors(meas, d_star)
    if not seed:
        return None
    # Same-Delta-Fenster verschmelzen: Label = Seite + Delta ist durch die
    # Seite bestimmt; Lücken (unnummerierte Opener) sind normal.
    segs: list[dict] = [{"d0": d_star, "first": min(seed), "last": max(seed),
                         "anchors": len(seed)}]

    def _step(direction: int) -> str:
        """Erweitert die Kette um ein Nachbarsgement (direction −1 links,
        +1 rechts). 'ok' = erweitert, 'end' = reguläres Kettenende,
        'break' = Disziplinbruch (→ Verweigerung)."""
        edge = segs[0]["first"] if direction < 0 else segs[-1]["last"]
        nd = (segs[0]["d0"] if direction < 0 else segs[-1]["d0"]) + direction
        cand = _anchors(meas, nd)
        near = [p for p in cand if 0 < (p - edge) * direction <= _MAX_BOUNDARY_DIST]
        if not near:
            return "end"
        b = min(near) if direction > 0 else max(near)
        # Grenzform: alle Seiten ZWISCHEN den Ankern müssen unnummeriert
        # sein (Opener, Blanks); eine nummerierte Grenzseite widerspricht
        # der Form und bricht die Disziplin.
        for gap in range(1, abs(b - edge)):
            if edge + gap * direction in digit_pages:
                return "break"
        local = [p for p in cand if p >= b] if direction > 0 else [p for p in cand if p <= b]
        if len(local) < MIN_SEGMENT_ANCHORS:
            # Einzelnker trägt keinen Drift-Beweis — reguläres Ende; der
            # Restanker fällt an die Rausch-/Widerspruchs-Rechnung.
            return "end"
        seg = {
            "d0": nd,
            "first": min(local),
            "last": max(local),
            "anchors": len(local),
        }
        if direction > 0:
            segs.append(seg)
        else:
            segs.insert(0, seg)
        return "ok"

    # Die +1-Treppe ist Konstruktion (nd = letztes Delta ± 1); ein
    # 'break' auf einer Seite verwirft die GESAMTE Kette.
    for direction in (1, -1):
        while True:
            st = _step(direction)
            if st == "break" or len(segs) > 64:
                return None
            if st == "end":
                break
    return segs


def _ranges_from_segments(
    meas: dict[int, dict], segs: list[dict], n: int
) -> tuple[list[dict], list[dict]]:
    """Range-Baum aus der Segmentkette: jede D-Range beginnt eine Seite
    vor ihrem ersten Anker (Grenzlabel = Anker-Folio − 1), sofern die
    Seite ziffernlos ist. Rückgabe (ranges, per-segment-Evidenz)."""
    digit_pages = {i for i, m in meas.items() if m["digits"]}
    ranges: list[dict] = []
    evidence = []
    for seg in segs:
        first_anchor = seg["first"]
        start = first_anchor
        if (
            first_anchor - 1 not in digit_pages
            and first_anchor - 1 + seg["d0"] >= 1
        ):
            # Grenzlabel-Konvention: die unnummerierte Opener-Seite vor dem
            # ersten Anker gehört zur Range (firstpagenum = Anker-Folio − 1);
            # nie hinter Folio 1 zurück (die Probe darf kein Label 0 erfinden).
            start = first_anchor - 1
        if start < 0:
            start = 0
        ranges.append({
            "startpage": start,
            "prefix": "",
            "style": "D",
            "firstpagenum": start + seg["d0"],
        })
        evidence.append({
            "delta0": seg["d0"],
            "anchor_pages": seg["anchors"],
            "range": [start, seg["last"]],
        })
    # Römischer Vorspann vor der ersten D-Range (Stil-Politik: VORSCHLAG
    # r ab I; adoptiert nur bei sauberer, durchgehender Kette, die bis
    # an die Arabic-Grenze reicht).
    first_d = ranges[0]["startpage"]
    if first_d > 0:
        ranges.insert(0, _roman_prefix(meas, first_d))
    return ranges, evidence


def _roman_prefix(meas: dict[int, dict], first_d: int) -> dict:
    roman_pages = {
        i: {v for v in ( _roman_value(t) for t in m["roman"] ) if v}
        for i, m in meas.items()
        if m["roman"]
    }
    best_v0, best_support, best_last = 0, 0, -1
    for v0 in range(1, min(first_d, 30) + 1):
        support = [
            i for i, vals in roman_pages.items()
            if i < first_d and (v0 + i) in vals
        ]
        if len(support) > best_support:
            best_v0, best_support, best_last = v0, len(support), max(support)
    upper = any(
        t[0].isupper()
        for i, m in meas.items() if i < first_d for t in m["roman"]
    )
    if best_support >= 3 and first_d - best_last <= _MAX_BOUNDARY_DIST:
        return {"startpage": 0, "prefix": "", "style": "R" if upper else "r",
                "firstpagenum": best_v0}
    return {"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1}


def classify(meas: dict[int, dict], n: int) -> dict:
    """Klassifiziert die Messung. Rückgabe mit `class`:
    'none' (kein Ziffer-Signal), 'refused' (Disziplin-Bruch), sonst
    'constant-offset' (1 Segment) bzw. 'drift' (+1-Stufentreppe)."""
    digit_pages = {i for i, m in meas.items() if m["digits"]}
    if not digit_pages:
        return {"class": "none", "reason": "keine Ziffer-Tokens im Band",
                "anchors": 0}

    support: Counter[int] = Counter()
    for i, m in meas.items():
        for v in m["digits"]:
            support[v - i] += 1  # 0-basiertes Delta, wie _anchors
    d_star = max(sorted(support), key=lambda d: support[d])

    segs = _expand(meas, d_star)
    if not segs:
        return {"class": "refused", "reason":
                "Stufenprofil bricht die +1-Disziplin (Schritt ≠ +1, "
                "nummerierte Grenzseite oder Einzelnker-Segment)"}

    ranges, evidence = _ranges_from_segments(meas, segs, n)
    total_anchors = sum(s["anchors"] for s in segs)
    if total_anchors < MIN_TOTAL_ANCHORS:
        return {"class": "refused", "reason":
                f"Ankerbasis zu dünn ({total_anchors} < {MIN_TOTAL_ANCHORS})"}

    # Rausch-/Widerspruchs-Rechnung: jede Ziffer-Seite muss ihr Label
    # tragen oder als Rauschseite zählen; konsistente Rest-FAMILIEN
    # (≥ MIN_UNEXPLAINED_FAMILY Seiten ohne jeden Label-Treffer, deren
    # Tokens ein gemeinsames Delta teilen) sind Echo-/Restsegmente
    # → Verweigerung. Rauschen auf sonst erklärten Seiten (Kapitel-
    # nummern, Verlags-IDs in der Band-Zeile) zählt hier nicht.
    spans = [(s["first"], s["last"], s["d0"]) for s in segs]
    explained = set()
    unexplained: Counter[int] = Counter()
    for i in digit_pages:
        vals = set(meas[i]["digits"])
        labels = {i + d0 for lo, hi, d0 in spans if lo <= i <= hi}
        if vals & labels:
            explained.add(i)
        else:
            for v in vals:
                unexplained[v - i] += 1
    noise_pages = len(digit_pages) - len(explained)
    if digit_pages and noise_pages / len(digit_pages) > MAX_NOISE_PAGE_RATIO:
        return {"class": "refused", "reason":
                f"Band-Rauschen dominiert ({noise_pages}/{len(digit_pages)} "
                "Ziffer-Seiten ohne Label-Treffer)"}
    residual = {d: c for d, c in unexplained.items() if c >= MIN_UNEXPLAINED_FAMILY}
    # Delta-Familien, die das Modell selbst schon trägt (gleiche Deltas wie
    # eine Segment), sind Modell, nicht Widerspruch; alles andere ab der
    # Familiengröße ist ein Echo-/Restsegment → Owner-Anker statt Ratebasis.
    model_deltas = {s["d0"] for s in segs}
    residual = {d: c for d, c in residual.items() if d not in model_deltas}
    if residual:
        top = max(residual.items(), key=lambda kv: kv[1])
        return {"class": "refused", "reason":
                f"nicht erklärter konsistenter Lauf (Delta {top[0]}, "
                f"{top[1]} Seiten) — Echo/Restsegment, Owner-Anker nötig"}

    cls = "constant-offset" if len(segs) == 1 else "drift"
    return {
        "class": cls,
        "plan_class": cls,
        "segments": len(segs),
        "ranges": ranges,
        "segment_evidence": evidence,
        "anchors": total_anchors,
        "pages_with_digits": len(digit_pages),
        "noise_pages": noise_pages,
        "dominant_delta": d_star,
    }


def labels_from_ranges(ranges: list[dict], n: int) -> list[str]:
    """Label-Liste (wie read_page_labels) aus einem Range-Baum rendern."""
    out = []
    for i in range(n):
        cur = None
        for r in ranges:
            if r["startpage"] <= i:
                cur = r
        if cur is None:
            out.append("")
            continue
        v = cur.get("firstpagenum", 0) + i - cur["startpage"]
        style = cur.get("style", "")
        if style == "D":
            out.append(str(v))
        elif style in ("r", "R"):
            out.append(_roman_render(v, style == "R") if v >= 1 else "")
        else:
            out.append("")
    return out


def would_heal(pdf: str | Path) -> dict:
    """Dry-Run-Verdiktion der Katalog-Regel (#395): Tree vorhanden, aber
    falsch — und die Druckseite trägt ein klassifizierbares Signal.
    would_heal=True liefert den Range-Baum direkt mit."""
    meas = measure_bands(pdf)
    doc = pymupdf.open(str(pdf))
    try:
        n = doc.page_count
    finally:
        doc.close()
    verdict = classify(meas, n)
    if verdict["class"] not in ("constant-offset", "drift"):
        return {**verdict, "would_heal": False,
                "reason": verdict.get("reason", verdict["class"])}
    labels = labels_from_ranges(verdict["ranges"], n)
    from . import pdf_kernel

    current = pdf_kernel.read_page_labels(pdf)
    if current == labels:
        return {**verdict, "would_heal": False,
                "reason": "labels already match measurement"}
    # Messebasierter No-Defect-Check (Produktionsfall 7ZMNXU6R: Labels
    # bereits korrekt): stimmt JEDES gemessene Folio mit dem aktuellen
    # Label überein, trägt die Druckseite keinen Defekt-Befund — dann ist
    # eine Abweichung nur Konvention (z. B. römischer Vorspann-Stil) und
    # KEIN heilbarer Defekt. Kein Operator-Run ohne Beweis.
    all_match = any(m["digits"] for m in meas.values())
    for i, m in meas.items():
        if not m["digits"]:
            continue
        expected = {
            r["firstpagenum"] + i - r["startpage"]
            for r in verdict["ranges"]
            if r["style"] == "D" and r["startpage"] <= i
        }
        if expected and current[i] not in {str(v) for v in expected}:
            all_match = False
            break
    if all_match and any(m["digits"] for m in meas.values()):
        return {**verdict, "would_heal": False,
                "reason": "all measured folios already correct"}
    return {**verdict, "would_heal": True, "op": "write_label_ranges",
            "labels": labels}
