"""#253: deterministische Stelle-1-Heilung der Label-Tree-Defektklasse.

Katalog-Regel (beweisbar-sicher, keine Stelle-2/3-Vorbedingung):

    Page-Label-Tree FEHLT oder ist ein leerer Strunk (z. B.
    ``<</Nums[0<</P()>>]>>``)  UND  die Textschicht ist vorhanden
    →  write_labels aus den gedruckten Folios der Textschicht.

Die Evidenz liegt komplett IN der Datei: Tree-Zustand (Catalog) + Folio-
Zellen in Kopf-/Fußzonen (die Stellen-1-Forensik-Primitive). Der klassische
Agentenlauf (Stelle 2/3, Modell) wird für diese Klasse NICHT gebraucht —
sie ist deterministisch entscheidbar und die Chirurgie in-Strand bewiesen
(#251-E2E).

Nur darstellbare Mappings werden geheilt (gleiche Range-Semantik wie
pdf_kernel._build_ranges): beliebig viele unbenannte Seiten am Anfang
(Titelei/Inhaltsverzeichnis), dann ein geschlossener +1-Lauf bis
Dokumentende. Alles andere fällt an den Agenten zurück.
"""

from __future__ import annotations

from pathlib import Path

import pymupdf

from . import pdf_kernel

# Anteil der Seiten mit Textschicht, der die Klasse als "Textlayer
# vorhanden" gelten lässt (Einzelblanko-Seiten dürfen vorkommen).
_TEXT_COVERAGE_MIN = 0.8


def label_tree_state(pdf: str | Path) -> str:
    """Zustand des Page-Label-Tree am Catalog.

    'missing'  — kein /PageLabels-Eintrag
    'empty'    — Tree existiert, liefert aber KEINE benannte Seite
                 (leerer Strunk wie ``<</Nums[0<</P()>>]>>``)
    'present'  — mindestens eine Seite hat ein Tier-1-Label
    """
    labels = pdf_kernel.read_page_labels(pdf)
    doc = pymupdf.open(str(pdf))
    try:
        catalog = doc.pdf_catalog()
        has_tree, _ = doc.xref_get_key(catalog, "PageLabels")
        has_tree = has_tree != "null"
    finally:
        doc.close()
    if not has_tree:
        return "missing"
    if any(lab.strip() for lab in labels):
        return "present"
    return "empty"


def readback_proof(pdf: str | Path) -> dict | None:
    """#258: Readback-Beweis am (ggf. geheilten) Exemplar — Katalog-Tree
    EXISTIERT und die Labels sind NICHT-leer. Rückgabe None = KEIN
    Beweis (dann ist eine "Heilung" ein ehrliches FAIL: kein healed-
    Verdict, kein Upload — der einzige Prädikats-Punkt für surgery_exec
    UND repair_agent, damit beide Stellen dieselbe Schema sprechen)."""
    if label_tree_state(pdf) != "present":
        return None
    labels = pdf_kernel.read_page_labels(pdf)
    named = [lab for lab in labels if lab.strip()]
    if not named:
        return None
    return {"tree": True, "named_pages": len(named), "labels_sample": named[:3]}


def diagnose(pdf: str | Path) -> dict:
    """Stelle-1-Diagnose der Defektklasse (nur Lesen)."""
    state = label_tree_state(pdf)
    chars = pdf_kernel.page_char_count(pdf)
    covered = sum(1 for c in chars if c > 0)
    n = len(chars)
    textlayer = n > 0 and covered / n >= _TEXT_COVERAGE_MIN
    return {
        "class": (
            "labeltree-missing"
            if state in ("missing", "empty") and textlayer
            else None
        ),
        "tree_state": state,
        "text_layer": textlayer,
        "pages": n,
        "pages_with_text": covered,
    }


def heal_labels(pdf: str | Path) -> list[str] | None:
    """Labels-Plan aus den gedruckten Folios der Textschicht — None, wenn
    das Mapping nicht deterministisch darstellbar ist (dann Agent).

    Zweistufig (#253-Forensik an den 9 echten Dateien):
    1. Roh-Zellen: Folio-Kandidat je Seite aus Kopf-/Fußzone.
    2. Anker-Ketten-Rekonstruktion: Rauschen (Jahreszahlen, Referenz-
       Nummern in der Footer-Zelle) darf den Lauf nicht zerbrechen.
       Gesucht ist die (start_page, start_value)-Hypothese mit maximaler
       Anker-Unterstützung: eine Seite ist Anker, wenn ihre Zelle exakt
       dem erwarteten Wert entspricht. Beweisbar-sicher durch:
       - der LETZTE Anker liegt auf der letzten Seite (bewiesenes Ende,
         kein Extrapolieren), und
       - zwischen Ankern folgt jedes Label der +1-Interpolation — der
         Lauf ist durch Anker BEIDSEITIG gepinnt, eine interpolierte
         Seite kann nicht abweichen, ohne einen Anker zu brechen.
       Seiten vor dem ersten Anker bleiben unbenannt (Titelei/Verzeichnis).

    Ernte #258: dieselbe Disziplin wie die #254-Preflight-Klassifikation
    (folio_harvest — vendored mirror von page_trust): Bänder 12%/75%, alle
    Formen, Konstanten-Drop, Stärke+Ketten-Picking. Was die Preflight als
    reparierbar klassifiziert, muss mit denselben Folios auch heilbar
    sein (Plantin-Laufkopf-Folios bei ~75% Höhe, in-toto-Fließtext-Folios).
    """
    from . import folio_harvest

    doc = pymupdf.open(str(pdf))
    try:
        n = doc.page_count
        picked = folio_harvest.extract_folio_candidates(doc)
    finally:
        doc.close()
    cells = [
        pdf_kernel.to_int_or_none(str(v)) if v is not None else None
        for v in (picked.get(i) for i in range(n))
    ]
    if not any(c is not None for c in cells) or n == 0:
        return None

    best: tuple[int, int, int] | None = None  # (anchors, start_page, value)
    named = [(i, c) for i, c in enumerate(cells) if c is not None]
    for start_page, start_value in named:
        # Anker zählen: pages whose cell equals the running expectation
        anchors = 0
        for i, c in named:
            expected = start_value + (i - start_page)
            if c == expected:
                anchors += 1
        if best is None or anchors > best[0]:
            best = (anchors, start_page, start_value)
    if best is None:
        return None
    _, sp, sv = best
    # Bewiesenes Ende: die letzte Seite muss selbst Anker sein.
    if cells[n - 1] != sv + (n - 1 - sp):
        return None
    labels = [
        "" if i < sp else str(sv + (i - sp)) for i in range(n)
    ]
    # Darstellbarkeits-Gate: exakt die Range-Semantik der Chirurgie
    if not pdf_kernel._build_ranges(labels):
        return None
    return labels


def would_heal(pdf: str | Path) -> dict:
    """Dry-Run-Verdiktion: gehört die Datei zur beweisbar-sicheren Klasse
    und ist ihr Folio-Mapping darstellbar?"""
    d = diagnose(pdf)
    if d["class"] is None:
        return {**d, "would_heal": False, "reason": "not-labeltree-class"}
    labels = heal_labels(pdf)
    if labels is None:
        return {**d, "would_heal": False, "reason": "folio-mapping-not-representable"}
    return {**d, "would_heal": True, "op": "write_labels", "labels": labels}
