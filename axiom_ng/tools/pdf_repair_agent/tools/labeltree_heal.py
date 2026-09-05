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
    das Mapping nicht deterministisch darstellbar ist (dann Agent)."""
    from . import forensics_tool

    doc = pymupdf.open(str(pdf))
    try:
        labels: list[str] = []
        for page in doc:
            truth = forensics_tool.page_truth(page, "")
            folio = truth.get("folio")
            v = pdf_kernel.to_int_or_none(folio) if folio else None
            labels.append("" if v is None else str(v))
    finally:
        doc.close()
    if not any(labels):
        return None
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
