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


def analyze_pdf(pdf_path: str) -> dict:
    import pymupdf  # type: ignore[import-not-found]

    from axiom_ng_runner.compute_core import page_trust as pt
    from axiom_ng_runner.compute_core.pdf_processing import extract_page_labels

    doc = pymupdf.open(pdf_path)
    try:
        n = doc.page_count
        labels = extract_page_labels(pdf_path)
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
