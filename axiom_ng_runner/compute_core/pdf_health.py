"""#184/#175 — shared PDF preflight verdict + analyze core (read-only).

ONE code path decides both directions of the self-healing loop: the same
function that REJECTS a broken book before queueing must return GREEN
after the heal — otherwise the repair counts as failed (no blind waving
through). The fix-service calls THIS module for both directions; the #175
runner preflight endpoint will wrap this same function (planned, not yet
wired). The sweep CLI (scripts/pdf_analyze.py) re-imports analyze_pdf
from here — the classification lives in exactly one place.

Layering note (review): analyze_pdf used to live in scripts/ and was
imported UPWARD from here — an inversion with a latent cycle. It moved
into compute_core where its deps (page_trust, pdf_processing) already
live; heavy imports stay function-local so this module stays importable
without the PDF stack for pure logic tests.
"""
from __future__ import annotations

import itertools
import re
from dataclasses import dataclass


@dataclass
class PreflightResult:
    ok: bool                      # False = reject (quality_hold / repair case)
    verdacht: str                 # 🔴/🟡/🟢 class
    grund: str                    # human-readable reason
    details: dict                 # analyze payload (labels, folio runs, versatz)

    def line(self, buch: str = "") -> str:
        kopf = f"[{buch}] " if buch else ""
        return f"{kopf}PREFLIGHT {'GRÜN' if self.ok else 'REJECT'} — {self.verdacht} — {self.grund}"


def _text_metrics(doc) -> dict:
    """#175 text-layer metrics: per-page text presence/density plus the
    suspicious patterns (blank-series, image-only pages) the preflight report
    must surface BEFORE chunking. All numbers are cheap pymupdf measures —
    no ML, exactly the "billig haltbar" contract. Returns a plain dict."""
    n = doc.page_count
    per_page: list[dict] = []
    blank: list[int] = []       # page indices with no extractable text at all
    image_only: list[int] = []  # pages with images but zero text chars
    total_chars = 0
    for i in range(n):
        page = doc[i]
        txt = page.get_text("text")
        chars = len(txt.strip())
        total_chars += chars
        has_images = bool(page.get_images(full=True))
        per_page.append({
            "page": i + 1,
            "chars": chars,
            "density": round(chars / max(1, page.rect.width * page.rect.height), 5),
        })
        if chars == 0:
            blank.append(i + 1)
            if has_images:
                image_only.append(i + 1)
    # Leere-Seiten-Serie: ≥3 aufeinanderfolgende leere Seiten.
    blank_series: list[list[int]] = []
    run: list[int] = []
    for pg in range(1, n + 1):
        if pg in blank:
            run.append(pg)
        else:
            if len(run) >= 3:
                blank_series.append(run)
            run = []
    if len(run) >= 3:
        blank_series.append(run)
    text_layer = total_chars > 0
    patterns: list[str] = []
    if text_layer and total_chars < n * 40:  # <40 chars/Seite im Schnitt ≈ fast leer
        patterns.append("sehr geringe Textdichte")
    if blank_series:
        patterns.append(
            "leere-Seite-Serie: " + ",".join(f"{r[0]}-{r[-1]}" for r in blank_series)
        )
    if image_only and len(image_only) >= max(1, n // 2):
        patterns.append(
            f"viele reine Bildseiten ohne OCR-Text ({len(image_only)}/{n})"
        )
    return {
        "text_layer": text_layer,
        "total_chars": total_chars,
        "mean_chars_per_page": round(total_chars / n, 1) if n else 0,
        "per_page": per_page,
        "blank_pages": blank,
        "image_only_pages": image_only,
        "blank_series": blank_series,
        "suspicious_patterns": patterns,
    }


def analyze_pdf(pdf_path: str) -> dict:
    import pymupdf  # type: ignore[import-not-found]

    from axiom_ng_runner.compute_core import page_trust as pt
    from axiom_ng_runner.compute_core.pdf_processing import extract_page_labels

    doc = pymupdf.open(pdf_path)
    try:
        n = doc.page_count
        labels = extract_page_labels(pdf_path)
        tm = _text_metrics(doc)
        tier1 = sum(1 for i in range(n) if (doc[i].get_label() or "").strip())
        if tier1 < n * 0.5 or (n > 0 and tier1 * 2 == n):
            label_verdict, label_reason = "kein Tier-1 (nur Footer/physisch)", "tier-2/3-fallback"
        else:
            trust, label_reason = pt.assess_labels(labels, n)
            label_verdict = {
                pt.PDF_LABEL_SANE: "gesund (unique, monoton, deckend)",
                pt.PHYSICAL_ONLY: f"KAPUTT: {label_reason}",
            }[trust]
        cands = pt.extract_folio_candidates(doc)
        runs = pt.folio_runs(cands)
        # W12: chapter-relative books (healed anchor sections corroborated by
        # the folio runs) verify per chapter; the per-chapter restart labels
        # are then the healed truth, not breakage.
        chapters = pt.chapter_restarts(doc, cands)
        verified = pt.verify_folio_sequence(cands, chapters)

        # Versatz: an folio-bewiesenen Seiten mit numeric Label vergleichen
        offs: list[int] = []
        for page, folio in verified.items():
            m = re.search(r"(\d{1,4})", labels.get(page, ""))
            if m:
                offs.append(int(folio) - int(m.group(1)))
        offset = max(set(offs), key=offs.count) if offs else None
        offset_consistent = offs and len([o for o in offs if o == offset]) >= len(offs) * 0.8

        labels_broken = label_verdict.startswith(("KAPUTT", "kein Tier-1")) and chapters is None
        folio_found = len(verified) >= 3
        if labels_broken and folio_found:
            suspicion = "🔴 reparierbar"
        elif labels_broken:
            suspicion = "🔴 unpaginiert"
        elif offset_consistent and offset not in (0, None):
            suspicion = "🟡 Versatz-Verdacht"
        elif offs and not offset_consistent:
            suspicion = "🟡 unklar (Label↔Folio uneinheitlich)"
        else:
            suspicion = "🟢 gesund"

        gaps = [b[0][0] - a[0][-1] - 1 for a, b in itertools.pairwise(runs)]
        return {
            "pages": n,
            "label_befund": label_verdict,
            "label_reason": label_reason,
            "tier1_anteil": round(tier1 / n, 2) if n else 0,
            "folio_laeufe": [
                {"start": r[0][0] + 1, "laenge": len(r[0]), "folio_von": r[1][0], "folio_bis": r[1][-1]}
                for r in runs
            ],
            "folio_verifiziert": len(verified),
            "luecken_zwischen_laeufen": gaps,
            "versatz": offset if offset_consistent else None,
            # #175 text-layer metrics (preflight, billig haltbar: pymupdf).
            "text_layer": tm["text_layer"],
            "mean_chars_per_page": tm["mean_chars_per_page"],
            "per_page_density": tm["per_page"],
            "blank_pages": tm["blank_pages"],
            "image_only_pages": tm["image_only_pages"],
            "blank_series": tm["blank_series"],
            "suspicious_patterns": tm["suspicious_patterns"],
            "verdacht": suspicion,
        }
    finally:
        doc.close()

def preflight(pdf_path: str) -> PreflightResult:
    """Verdict for one PDF. GREEN = 🟢 gesund or 🟡 (labels sane); everything
    🔴 rejects (kaputt-reparierbar goes to the repair queue, unpaginiert
    never enters the loop)."""
    d = analyze_pdf(pdf_path)
    v = d["verdacht"]
    if v.startswith("🟢"):
        return PreflightResult(True, v, d.get("label_befund", ""), d)
    if v.startswith("🟡"):
        return PreflightResult(True, v, "sanity-ok (Versatz/unklar, kein Reparatur-Fall)", d)
    # 🔴 Klassen: reject — reparierbar geht in die Queue, unpaginiert nie
    return PreflightResult(False, v, d.get("label_befund", d.get("grund", "")), d)
