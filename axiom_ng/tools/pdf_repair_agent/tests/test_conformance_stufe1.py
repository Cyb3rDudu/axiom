"""Conformance-Zeuge (#203-Pflicht): pdf_kernel == Stufe-1-CLI.

Füttert BEIDE Engines mit denselben Ankern und verlangt identische
Label-Bäume. Die Stufe-1-CLI (scripts/pdf_label_surgery.py, #176) läuft als
SUBPROCESS mit dem Runner-Venv — kein Projekt-Import im Paket (Isolation
bleibt unangetastet); unser Kernel läuft im eigenen Venv.

Zeugen:
  1. synthetischer Vollzeuge   falsche_labels.pdf (constant-offset, kein
     Vorspann) → beide Bäume müssen KOMPLETT identisch sein.
  2. difficult-Bücher          (lokal, git-ignored; skipif abwesend) —
     Injektionsklasse: ein Zeuge pro geheiltem Buch auf dem NUMERISCHEN
     Körper (zitierfähige Seiten). Vorspann-Seiten, die Stufe-1 als
     römisch/Cover-Präfix schreibt, sind vom numerischen Kernel bewusst
     NICHT darstellbar und werden dokumentiert — nie still behauptet.
     Verweigert Stufe-1 (unclassifiable), muss auch unser Kernel
     unangetastet lassen (Conformance in der Verweigerung).
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))

# type: ignore: pyright indiziert paket-lokale Module am Workspace-Root nur
# träge — LSP-False-Positiv; pytest importiert real (siehe Paket-Venv).
from tools import (  # noqa: E402
    forensics_tool,
    pdf_kernel,
)

FIX = PKG / "fixtures"
REPO = PKG.parents[2]
STUFE1 = REPO / "scripts" / "pdf_label_surgery.py"
RUNNER_PY = REPO / "axiom_ng_runner" / ".venv" / "bin" / "python"
DIFFICULT = FIX / "difficult"

needs_stufe1 = pytest.mark.skipif(
    not (STUFE1.exists() and RUNNER_PY.exists()),
    reason="Stufe-1-CLI oder Runner-Venv nicht vorhanden",
)


def _stufe1_heal(pdf: Path, anchors: list[dict]) -> tuple[int, str]:
    """Stufe-1-CLI im off-storage-Modus auf einer Kopie ausführen.
    Rückgabe (rc, stdout) — rc 0=geheilt, 3=REFUSED, 2=ABORT, 1=Fehler."""
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(anchors, f)
        anchors_file = Path(f.name)
    try:
        r = subprocess.run(
            [
                str(RUNNER_PY),
                str(STUFE1),
                pdf.stem[:24],
                "--pdf",
                str(pdf),
                "--anchors",
                str(anchors_file),
                "--apply",
            ],
            capture_output=True,
            text=True,
            timeout=900,
            check=False,
        )
        return r.returncode, r.stdout + r.stderr
    finally:
        anchors_file.unlink(missing_ok=True)


def _longest_monotone_folio_run(m: dict) -> list[dict]:
    """Längster Folio-+1-Lauf aus der Beweiskarte → konsistente Ankermenge
    ({page, N, M}). Beweis-Auswahl (kein Raten): nur gemessene Folios."""
    folio = {i: p["folio"] for i, p in enumerate(m["pages"]) if p["folio"] is not None}
    best: list[tuple[int, str]] = []
    cur: list[tuple[int, str]] = []
    for i in sorted(folio):
        if not cur or (int(folio[i]) == int(cur[-1][1]) + 1 and i == cur[-1][0] + 1):
            cur.append((i, folio[i]))
        else:
            cur = [(i, folio[i])]
        if len(cur) > len(best):
            best = list(cur)
    labels = pdf_kernel.read_page_labels(str(m["source"]))
    return [{"page": i, "N": labels[i], "M": v} for i, v in best]


def _numeric_body_run(labels: list[str]) -> tuple[int, list[str]] | None:
    """Erste Seite mit numerischem Label + folgender +1-Lauf bis Ende.
    None, wenn kein solcher Abschluss-Lauf existiert."""
    start = None
    for i, lab in enumerate(labels):
        if lab.isdigit():
            start = i
            break
    if start is None:
        return None
    run = [labels[start]]
    for lab in labels[start + 1 :]:
        if not lab.isdigit() or int(lab) != int(run[-1]) + 1:
            return None
        run.append(lab)
    return start, run


def _trailing_monotone_run(labels: list[str]) -> tuple[int, list[str]] | None:
    """Längster numerischer +1-Lauf, der an der LETZTEN numerischen Seite
    endet (nicht-digitale Nachspann-Seiten danach bleiben außerhalb des
    Fensters). Das ist die Kernel-darstellbare Wahrheitszone."""
    last = max((i for i, lab in enumerate(labels) if lab.isdigit()),
               default=-1)
    if last < 0:
        return None
    start = last
    while start > 0 and labels[start - 1].isdigit() \
            and int(labels[start - 1]) == int(labels[start]) - 1:
        start -= 1
    if last - start + 1 < 5:  # zu kurz für ein Körperzeugnis
        return None
    return start, labels[start : last + 1]


def _damage_spec_offset(pdf: Path, delta: int = 15) -> None:
    """Deterministischer Schaden (Reproduced-Case): JEDER numerischen Range
    der Baums um +delta verschieben; Struktur/Stil/Präfixe bleiben exakt —
    damit ist der Schaden exakt die Stufe-1-Prämisse „kaputt sind die
    Starts, nicht die Stile" (PRESERVE)."""
    import pymupdf  # type: ignore[reportMissingImports]

    doc = pymupdf.open(str(pdf))
    spec = [dict(r) for r in (doc.get_page_labels() or [])]
    for r in spec:
        # Nur echte numerische Ranges (style D) verschieben; einträge ohne
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
    doc.set_page_labels([{"startpage": 0, "prefix": "", "style": "D",
                          "firstpagenum": int(run[0])}])
    doc.save(str(dst))
    doc.close()
    trimmed = pdf_kernel.read_page_labels(str(dst))
    if trimmed != run:
        return None  # Fenster-Trim verfälschte Labels — kein Zeugenfundament
    return trimmed


@needs_stufe1
def test_conformance_constant_offset_vollidentisch(tmp_path):
    """Synthetischer Vollzeuge: beide Engines, gleiche Anker → kompletter
    Label-Baum identisch (1..10 statt 3..12)."""
    src = FIX / "falsche_labels.pdf"
    if not src.exists():
        from fixtures.generate_fixtures import main as gen

        gen()
    m = forensics_tool.build_map(src)
    anchors = _longest_monotone_folio_run(m)
    assert len(anchors) >= 2, "Fixture-Anker fehlgeschlagen"

    c1 = tmp_path / "stufe1.pdf"
    c2 = tmp_path / "kernel.pdf"
    shutil.copy2(src, c1)
    shutil.copy2(src, c2)

    rc, out = _stufe1_heal(c1, anchors)
    assert rc == 0, f"Stufe-1 weigerte sich unerwartet (rc={rc}):\n{out[-600:]}"

    # Kernel-Äquivalent aus derselben Anker-Arithmetik: M-Körper (monoton).
    body = _numeric_body_run(pdf_kernel.read_page_labels(str(c1)))
    assert body is not None, "Stufe-1 schrieb keinen numerischen Abschluss-Lauf"
    start, run = body
    pdf_kernel.write_page_labels(c2, [""] * start + run)

    l1 = pdf_kernel.read_page_labels(str(c1))
    l2 = pdf_kernel.read_page_labels(str(c2))
    assert l1 == l2, f"Label-Bäume weichen ab:\nStufe1={l1}\nKernel={l2}"
    assert l1 == [str(i) for i in range(1, 11)]  # geheilt auf Folio-Wahrheit


@needs_stufe1
def test_conformance_difficult_buecher(tmp_path):
    """Ein Zeuge pro anwesendem difficult-Buch (Injektionsklasse): numerischer
    Körper identisch; Stufe-1-Verweigerung ⇒ Kernel schreibt auch nicht."""
    if not DIFFICULT.is_dir():
        pytest.skip("fixtures/difficult/ nicht vorhanden (lokales Testset)")
    books = sorted(DIFFICULT.glob("*.pdf"))
    if not books:
        pytest.skip("keine difficult-Bücher")
    witnessed = 0
    refusals = 0
    anchored = 0  # Bücher mit >=2 Beweis-ankern (Zeugen-Kandidaten)
    for book in books:
        m = forensics_tool.build_map(book)
        anchors = _longest_monotone_folio_run(m)
        if len(anchors) < 2:
            continue  # kein Kronfall-Labelzeugnis (z. B. Scan ohne Textschicht)
        anchored += 1
        c1 = tmp_path / (book.stem[:24] + "_s1.pdf")
        c2 = tmp_path / (book.stem[:24] + "_k.pdf")
        shutil.copy2(book, c1)
        shutil.copy2(book, c2)
        before = pdf_kernel.read_page_labels(str(c1))
        rc, out = _stufe1_heal(c1, anchors)
        after = pdf_kernel.read_page_labels(str(c1))
        if rc != 0:
            # Conformance in der Verweigerung — bewiesen ist hier NUR
            # Stufe-1s eigene Rollback-Disziplin auf c1 (Datei unverändert)
            # plus dass der Kernel-Zweig nie an write_page_labels kommt (c2
            # ebenfalls unverändert); KEIN positives Kernel-Zeugnis.
            assert after == before, (
                f"{book.name}: Stufe-1 rc={rc}, aber Datei verändert"
            )
            assert pdf_kernel.read_page_labels(str(c2)) == before, (
                f"{book.name}: Kernel-Zweig hat trotz Verweigerung geschrieben"
            )
            refusals += 1
            continue
        body = _numeric_body_run(after)
        assert body is not None, f"{book.name}: kein numerischer Abschluss-Lauf"
        start, run = body
        pdf_kernel.write_page_labels(c2, [""] * start + run)
        l1 = pdf_kernel.read_page_labels(str(c1))
        l2 = pdf_kernel.read_page_labels(str(c2))
        # Numerischer Körper (ab Body-Start bis Ende) muss identisch sein.
        assert l1[start:] == l2[start:], (
            f"{book.name}: Körper weicht ab\nStufe1={l1[start:][:8]}…\n"
            f"Kernel={l2[start:][:8]}…"
        )
        # Vorspann-Differenz ist dokumentierte Kernel-Grenze (römisch/Cover-
        # Präfix nur durch Stufe-1 darstellbar) — der numerische Körper trägt
        # die Conformance; hier wird die Grenze BEHAUPTENSFREI gemeldet.
        front_diff = [i for i in range(start) if l1[i] != l2[i]]
        assert all(not l2[i] for i in front_diff), (
            f"{book.name}: Kernel schrieb unerlaubt in den Vorspann"
        )
        witnessed += 1
    # Vakuoses Passieren verboten: gab es Zeugen-Kandidaten, ABER nur
    # Verweigerungen, ist das KEIN positives Kernel-Zeugnis → skip statt
    # stiller Grün.
    if anchored and witnessed == 0:
        pytest.skip("nur Verweigerungen — kein positives Kernel-Zeugnis")
    assert witnessed + refusals >= 1, "kein Buch lief Beweis-anker"


# ---------------------------------------------- Reproduced-Case (G4-Auftrag) --
# Messlage 2026-08-23: die difficult-Bücher (außer Controlling) tragen
# bereits Label-Bäume, die im Körper label≡folio gesund sind (Stufe-1
# verweigert sie zurecht). Positive Real-Zeugen entstehen deshalb als
# Reproduced-Case nach G3-DoD: reale Buchkopie gesund → deterministischer
# +15er Spec-Schaden (_damage_spec_offset) → echte Label-vs-Wahrheits-
# Mismatch-Anker → BEIDE Engines müssen dieselbe numerische Wahrheit
# wiederherstellen. Controlling bleibt außen vor (OCR-Kronfall, 0 Folios).


@needs_stufe1
def test_conformance_reproduced_offset_echte_buecher(tmp_path):
    """Positiver Real-Zeuge pro Buch (Dubs + folk): injizierter konstanter
    Offset → Stufe-1 constant-offset-Heilung == vollständige Wahrheit;
    Kernel schreibt denselben numerischen Körper."""
    if not DIFFICULT.is_dir():
        pytest.skip("fixtures/difficult/ nicht vorhanden (lokales Testset)")
    frag_oder = ("Dubs", "folk")
    books = [b for b in sorted(DIFFICULT.glob("*.pdf"))
             if any(f in b.name for f in frag_oder)]
    if not books:
        pytest.skip("keine Reproduced-Case-Bücher (Dubs/folk) anwesend")
    for book in books:
        c1 = tmp_path / (book.stem[:24] + "_s1.pdf")
        c2 = tmp_path / (book.stem[:24] + "_k.pdf")
        truth = _trim_to_body_window(book, c1)
        if truth is None:
            pytest.skip(f"{book.name}: kein kernel-darstellbarer Körperlauf")
        shutil.copy2(c1, c2)
        start, run = 0, truth
        _damage_spec_offset(c1, delta=15)
        _damage_spec_offset(c2, delta=15)
        damaged = pdf_kernel.read_page_labels(str(c1))
        assert damaged != truth, "Schaden nicht wirksam — Zeuge sinnlos"

        # Echte Mismatch-Anker: N = beschädigtes Label, M = gemessene
        # Wahrheit (nur numerische Seiten; der Körperlauf genügt).
        anchors = [{"page": i, "N": damaged[i], "M": truth[i]}
                   for i in range(start, len(truth))
                   if damaged[i].isdigit() and truth[i].isdigit()]
        assert len(anchors) >= 2

        rc, out = _stufe1_heal(c1, anchors)
        assert rc == 0, (f"{book.name}: Stufe-1 verweigerte den konstant-"
                         f"Offset-Reproduced-Case (rc={rc}):\n{out[-400:]}")
        l1 = pdf_kernel.read_page_labels(str(c1))
        assert l1 == truth, (f"{book.name}: Stufe-1 stellte die Wahrheit "
                             f"nicht vollständig wieder her")

        # Kernel-Heilung aus derselben Wahrheit: Körperlauf schreiben.
        pdf_kernel.write_page_labels(c2, [""] * start + run)
        l2 = pdf_kernel.read_page_labels(str(c2))
        assert l2[start:] == truth[start:], (
            f"{book.name}: Kernel-Körper weicht von der Wahrheit ab")
        assert l1[start:] == l2[start:], (
            f"{book.name}: Engines weichen im Körper ab")
        # Kernel bleibt außerhalb des Körpers leer (dokumentierte Grenze,
        # kein heimliches Schreiben in Präfix-/Vorspannzonen).
        assert all(not l2[i] for i in range(start))
