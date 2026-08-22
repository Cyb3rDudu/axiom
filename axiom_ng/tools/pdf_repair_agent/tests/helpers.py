"""Gemeinsame Zeugen-Helfer (tests-intern, keine Tests hier): Fensterwahl
des numerischen Körperlaufs, deterministischer Spec-Schaden (+δ) und die
Fenster-Kopie daraus. Wiederverwendung zwischen Conformance- und
G4-Zeugen — Verhalten identisch zu den früheren lokalen Definitionen."""

from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
if str(PKG) not in sys.path:
    sys.path.insert(0, str(PKG))

from tools import pdf_kernel  # type: ignore[reportMissingImports]  # noqa: E402


def _trailing_monotone_run(labels: list[str]) -> tuple[int, list[str]] | None:
    """Längster numerischer +1-Lauf, der an der LETZTEN numerischen Seite
    endet (nicht-digitale Nachspann-Seiten danach bleiben außerhalb des
    Fensters). Das ist die Kernel-darstellbare Wahrheitszone."""
    last = max((i for i, lab in enumerate(labels) if lab.isdigit()), default=-1)
    if last < 0:
        return None
    start = last
    while (
        start > 0
        and labels[start - 1].isdigit()
        and int(labels[start - 1]) == int(labels[start]) - 1
    ):
        start -= 1
    if last - start + 1 < 5:  # zu kurz für ein Körperzeugnis
        return None
    return start, labels[start : last + 1]


def _damage_spec_offset(pdf: Path, delta: int = 15) -> None:
    """Deterministischer Schaden (Reproduced-Case): JEDE numerische Range
    des Baums um +delta verschieben; Struktur/Stil/Präfixe bleiben exakt —
    damit ist der Schaden exakt die Stufe-1-Prämisse „kaputt sind die
    Starts, nicht die Stile" (PRESERVE)."""
    import pymupdf  # type: ignore[reportMissingImports]

    doc = pymupdf.open(str(pdf))
    spec = [dict(r) for r in (doc.get_page_labels() or [])]
    for r in spec:
        # Nur echte numerische Ranges (style D) verschieben; Einträge ohne
        # style sind KONSTANTE Präfix-Label (z. B. 'C1') — unangetastet.
        if r.get("style") in ("D", "d"):
            r["firstpagenum"] = r.get("firstpagenum", 1) + delta
    doc.set_page_labels(spec)
    tmp = pdf.with_suffix(".dmg.pdf")
    doc.save(str(tmp))
    doc.close()
    tmp.replace(pdf)


def _trim_to_body_window(src: Path, dst: Path) -> list[str] | None:
    """Zeugen-Kopie auf das Fenster des abschließenden numerischen +1-Laufs
    stutzen ([start..letzte numerische Seite]) und als EINEN D-Range mit der
    Original-Wahrheit schreiben. Reale Inhaltsseiten, echter Seitenzustand,
    keine Präfix-/Style-Sonderfälle — beide Engines voll vergleichbar.
    pymupdf select() verwirft PageLabels, darum wird der Fenster-Spec
    explizit gesetzt. Rückgabe: Truth-Labels des Fensters."""
    import pymupdf  # type: ignore[reportMissingImports]

    labels = pdf_kernel.read_page_labels(str(src))
    body = _trailing_monotone_run(labels)
    if body is None:
        return None
    start, run = body
    last = start + len(run) - 1
    doc = pymupdf.open(str(src))
    doc.select(list(range(start, last + 1)))
    doc.set_page_labels(
        [{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": int(run[0])}]
    )
    doc.save(str(dst))
    doc.close()
    trimmed = pdf_kernel.read_page_labels(str(dst))
    if trimmed != run:
        return None  # Fenster-Trim verfälschte Labels — kein Zeugenfundament
    return trimmed
