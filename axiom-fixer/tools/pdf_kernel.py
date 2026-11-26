"""pdf_kernel — deterministische PDF-Primitiven (PAKET-LOKAL).

Kapselt die Stufe-1-kompatiblen Konzepte (Tier-1-Labels via
`page.get_label()`, gedruckte Folio-Extraktion) OHNE Projekt-Import:
NUR pymupdf, im eigenen Venv gepinnt. Label-Schreiben ist der einzige
Schreibpfad (von T4/agent genutzt): setzt den /Root/PageLabels-Baum über
die pymupdf-Range-Semantik (maximal-fortlaufender numerischer Lauf nach
eventueller unbenannter Titelei). Read-back via page.get_label().
"""

from __future__ import annotations

import os
import tempfile
from pathlib import Path
from typing import cast

# Vom LSP gegen das Workspace-Root-Venv nicht auflösbar (Isolation): das
# eigene Venv (bootstrap.sh) hält es — Laufzeit-Probe im Venv vorausgesetzt.
import pymupdf  # type: ignore[reportMissingImports]

# Gedruckte Folio: eigenständige Zahl (1-4 stellig) in der Kopf-/Fußzone
# (oberste bzw. unterste 18 % der Seitenhöhe — siehe _zone_texts).
_HEADER = "header"
_FOOTER = "footer"


def read_page_labels(pdf: str | Path) -> list[str]:
    """Tier-1-Labels pro physischer Seite (get_label; '' = unbenannt).

    Seiten VOR der ersten Label-Range (Titelei) werfen in pymupdf einen
    IndexError (get_label_pno indiziert eine leere Liste) — das ist der
    unterstützte unbenannte Fall und wird als '' gelesen, nicht als Absturz.
    """
    doc = pymupdf.open(str(pdf))
    labels = []
    for i in range(doc.page_count):
        try:
            labels.append(doc[i].get_label() or "")
        except IndexError:
            labels.append("")
    doc.close()
    return labels


def to_int_or_none(lab: str) -> int | None:
    """Ziffernstring → int; sonst None (gemeinsam genutzt von T1/T3)."""
    s = lab.strip()
    try:
        return int(s)
    except ValueError:
        return None


def _build_ranges(labels: list[str]) -> list[dict]:
    """PDF-Label-Baum: nur sinnvolle numerisch-fortlaufende Ranges.

    PDF-PageLabels sind RANGE-basiert: ein Startpunkt deckt alle Folgeseiten
    bis zum nächsten Startpunkt. Eine „leere“ Seite innerhalb oder NACH dem
    belegten Körper ist so NICHT ausdrückbar (der Range würde sie füllen).
    Deshalb wird genau EIN geschlossener numerischer Lauf als /D-Range
    gesetzt; jede Restseite danach (leer oder belegt) macht das Mapping
    unimplementierbar → [] als Meldung — nie falsch befüllt.

    Erlaubt (realistisch, der Reparaturfall): beliebig viele unbenannte
    Seiten AM ANFANG (Titelei), dann ein numerischer Lauf BIS Dokumentende.
    """
    n = len(labels)
    i = 0
    while i < n and not labels[i]:
        i += 1  # führende unbenannte Seiten (Titelei) bleiben leer
    if i == n:
        return []  # völlig unbenannt
    # Der (erste) geschlossene Lauf nach der Titelei:
    run: list[tuple[int, int]] = []
    j = i
    prev: int | None = None
    while j < n:
        v = to_int_or_none(labels[j])
        if v is None or (prev is not None and v != prev + 1):
            break  # nicht-numerisch oder Sprung → Lauf endet hier
        run.append((j, v))
        prev = v
        j += 1
    # ALLE Seiten nach dem Lauf — ob unbenannt ('') oder belegt — sind nicht
    # darstellbar: der Range-Knoten läuft bis Dokumentende und würde sie
    # fälschlich befüllen. Verweigern ([]) statt falsch schreiben
    # (Kodex: keine stille Falschheit).
    if j < n or not run:
        return []
    # (#251 E2E) Deckungs-Entry für führende unbenannte Seiten: ein Baum,
    # dessen erster Range erst bei Seite >0 beginnt, ist PDF-spec-legal,
    # stürzt aber pymupdfs get_label-util im Runner-Chunker ab (IndexError
    # für Seiten vor dem ersten Entry). Ein prefix-loser, nummerierungs-
    # freier Entry an Seite 0 macht die Seiten ausdrücklich unbenannt —
    # semantisch identisch, aber für jeden Konsumenten lesbar.
    if i > 0:
        return [
            {"startpage": 0, "prefix": ""},  # Titelei ausdrücklich unbenannt
            {"startpage": run[0][0], "prefix": "", "style": "D",
             "firstpagenum": run[0][1]},  # ein Range deckt den ganzen Lauf
        ]
    start, first = run[0]
    return [{"startpage": start, "prefix": "", "style": "D", "firstpagenum": first}]


def write_page_labels(pdf: str | Path, labels: list[str]) -> None:
    """Überschreibt die Tier-1-Labels. Erwartet GENAU `page_count` Einträge
    (Längen-Abweichung → ValueError); '' bedeutet unbenannt. Nur ein
    geschlossener numerischer Lauf nach optionaler Titelei wird gesetzt
    (PDF-Range-Semantik, siehe _build_ranges). Nicht darstellbare Mappings
    (Lücke/Sprung/Restseiten) werden mit ValueError verweigert — nie falsch
    befüllt.
    """
    pdf = str(pdf)
    doc = pymupdf.open(pdf)
    try:
        n = doc.page_count
        if len(labels) != n:
            raise ValueError(
                f"labels hat {len(labels)} Einträge, PDF hat {n} Seiten —"
                f" Verweigerung: Range-Semantik würde fehlende Seiten fälschlich"
                f" befüllen bzw. überzählige still verwerfen."
            )
        ranges = _build_ranges(labels)
        if not ranges and any(labels):
            raise ValueError(
                "PageLabels für diese Mapping nicht darstellbar (Lücke/Sprung"
                " im belegten Körper oder nicht-numerischer Lauf) — nichts"
                " geschrieben."
            )
        doc.set_page_labels(ranges)
        # Inkrementelles Schreiben über die geöffnete Datei lehnt pymupdf bei
        # Strukturänderung (Seiten-Labels = /Root-Objektumbau) ab. Voller Save
        # auf eine Temp-Datei im selben Verzeichnis, dann atomar ersetzen —
        # die Arbeitskopie (Backup-Pflicht des Agenten) bleibt unangetastet.
        _atomic_save(doc, pdf)
    finally:
        doc.close()


def _atomic_save(doc: pymupdf.Document, pdf: str) -> None:
    """Voller Save auf Temp + atomares Ersetzen (Range-Schreiben ist
    /Root-Objektumbau — inkrementell lehnt pymupdf das ab)."""
    d = Path(pdf).parent
    fd, tmp = tempfile.mkstemp(suffix="-labels.pdf", dir=d)
    os.close(fd)
    try:
        doc.save(tmp)
        os.replace(tmp, pdf)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def _validate_ranges(ranges: list[dict], n: int) -> list[dict] | None:
    """Strukturprüfung eines Range-Baums (#395): erste Range bei Seite 0
    (Deckungspflicht — Seiten vor dem ersten Entry blieben still
    unbenannt), Startpages strikt steigend, Style aus der PDF-Semantik,
    firstpagenum der nummerierten Ranges ist int >= 1 (bool ist kein
    int-Anwärter: True == 1; pymupdf normalisiert <= 0 still und
    divergierte damit von labels_from_ranges). Rückgabe None = nicht
    darstellbar."""
    if not isinstance(ranges, list) or not ranges:
        return None
    prev = -1
    for k, r in enumerate(ranges):
        sp = r.get("startpage")
        style = r.get("style", "")
        fpn = r.get("firstpagenum")
        if isinstance(sp, bool) or not isinstance(sp, int) or not (0 <= sp < n):
            return None
        if k == 0 and sp != 0:
            return None  # Deckung ab Seite 0 (keine stille Titelei-Lücke)
        if sp <= prev:
            return None
        if r.get("prefix", "") != "":
            return None  # Präfixe trägt dieser Baum bewusst nicht
        if style not in ("", "D", "r", "R"):
            return None
        if style and (
            isinstance(fpn, bool) or not isinstance(fpn, int) or fpn < 1
        ):
            return None
        prev = sp
    return ranges


def write_label_ranges(pdf: str | Path, ranges: list[dict]) -> None:
    """#395: setzt einen MEHRFACH-Range-Baum (römischer Vorspann + mehrere
    D-Segmente — das Drift-/Two-Range-Heilungsformat). Validierung über
    _validate_ranges (Deckung ab Seite 0, strikt steigend, int >= 1 als
    firstpagenum — nie falsch befüllen), atomarer Save wie in
    write_page_labels."""
    pdf = str(pdf)
    doc = pymupdf.open(pdf)
    try:
        n = doc.page_count
        if _validate_ranges(ranges, n) is None:
            raise ValueError(
                "Range-Baum nicht darstellbar (leer/falsche Reihenfolge/"
                "unzulässiger Style) — nichts geschrieben."
            )
        doc.set_page_labels(ranges)
        _atomic_save(doc, pdf)
    finally:
        doc.close()


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


def labels_from_ranges(ranges: list[dict], n: int) -> list[str]:
    """Label-Liste (read_page_labels-Form) aus einem Range-Baum — die
    gemeinsame Render-Sicht (#395) von Klassifikator (label_drift),
    surgery_exec-Validator und Readback-Vergleich. Range->Label ist
    PDF-Semantik, lebt deshalb hier im Kernel (einzig).
    """
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


def _zone_texts(page: pymupdf.Page) -> dict[str, str]:
    h = page.rect.height
    # get_text(option="text") liefert laut pymupdf-Stub je nach Option eine
    # Union (str/list/dict); für "text" ist es immer str — cast belegt das.
    top = cast(
        str, page.get_text("text", clip=pymupdf.Rect(0, 0, page.rect.width, h * 0.18))
    )
    bottom = cast(
        str, page.get_text("text", clip=pymupdf.Rect(0, h * 0.82, page.rect.width, h))
    )
    return {_HEADER: top, _FOOTER: bottom}


def page_char_count(pdf: str | Path) -> list[int]:
    """Zeichenzahl der Textschicht je Seite (T2-Tor: 0 = keine Textschicht)."""
    doc = pymupdf.open(str(pdf))
    counts = [len(doc[i].get_text("text")) for i in range(doc.page_count)]
    doc.close()
    return counts


def doc_page_count(pdf: str | Path) -> int:
    doc = pymupdf.open(str(pdf))
    n = doc.page_count
    doc.close()
    return n
