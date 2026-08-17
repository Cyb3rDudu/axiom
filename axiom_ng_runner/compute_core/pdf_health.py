"""#184/#175 — shared PDF preflight verdict (read-only).

ONE code path decides both directions of the self-healing loop: the same
function that REJECTS a broken book before queueing must return GREEN
after the heal — otherwise the repair counts as failed (no blind waving
through). The runner preflight endpoint (#175) and the fix-service both
call THIS module; the sweep CLI mirrors its classification.
"""
from __future__ import annotations

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


def preflight(pdf_path: str) -> PreflightResult:
    """Verdict for one PDF. GREEN = 🟢 gesund or 🟡 (labels sane); everything
    🔴 rejects (kaputt-reparierbar goes to the repair queue, unpaginiert
    never enters the loop)."""
    import sys
    from pathlib import Path

    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
    from axiom_ng_runner.scripts.pdf_analyze import analyze_pdf

    d = analyze_pdf(pdf_path)
    v = d["verdacht"]
    if v.startswith("🟢"):
        return PreflightResult(True, v, d.get("label_befund", ""), d)
    if v.startswith("🟡"):
        return PreflightResult(True, v, "sanity-ok (Versatz/unklar, kein Reparatur-Fall)", d)
    # 🔴 Klassen: reject — reparierbar geht in die Queue, unpaginiert nie
    return PreflightResult(False, v, d.get("label_befund", d.get("grund", "")), d)
