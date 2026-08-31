"""#176 Phase 1 — read-only PDF analyze + repair-plan verify (CLI).

analyze <pdf>        : Label-Befund, Folio-Läufe (Start/Länge/Lücken),
                       Versatz zwischen Label und Folio, Verdachtsklasse.
verify <pdf> --plan  : kills a repair plan that contradicts the folio truth
                       (rot-vorher witness: a deliberately wrong plan MUST
                       be rejected; the plan op never writes anything).
sweep                : analyze every active preferred PDF, join chunk counts,
                       write ~/Desktop/ANALYZE_SWEEP.md and cross-check dudu's
                       REPATUR_KANDIDATEN.md Stufe-1 list (confirmed vs
                       exonerated). Read-only end to end.

Verdachtsklassen:
  🔴 reparierbar   Labels nachweislich kaputt (Wiederholung/nicht-monoton)
                  UND ein Folio-Lauf als Reparaturquelle vorhanden
  🔴 unpaginiert  weder Labels noch Folio-Lauf — Relabel kann nicht helfen
  🟡 Versatz-Verdacht  Labels sanity-ok, aber Folio widerspricht (Offset-Klasse)
  🟢 gesund       Labels sanity-ok, kein Folio-Widerspruch
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

# Heavy imports (pymupdf, compute_core) stay INSIDE the functions: the pure
# plan checker check_plan_against_folio is importable and testable without a
# PDF stack (tests/test_pdf_analyze_verify.py).

from axiom_ng_runner.compute_core.pdf_health import (
    analyze_pdf,
)

DSN = os.environ.get(
    "AXIOM_DATABASE_URL",
    "postgresql://axiom_user:axiom_password@127.0.0.1:5432/axiom_db?sslmode=disable",
)



def check_plan_against_folio(plan: dict[str, str], verified: dict[int, str]) -> dict:
    """Pure decision core of verify_plan — no PDF stack needed.

    Plan keys are 1-BASED PDF pages (the same convention analyze reports:
    "start": run[0][0] + 1). A key that is not a folio-verified page is a
    PLAN MISMATCH, not a vacuous pass: a plan naming pages we cannot prove
    must go to dudu, not slip through unchecked (review E1 — the old code
    skipped unknown keys, so the off-by-one witness {12:'4'} and every
    0-based key passed without being checked at all).
    """
    killed: list[dict] = []
    for page_s, new_label in plan.items():
        try:
            page = int(str(page_s))
        except (TypeError, ValueError):
            killed.append({"page": page_s, "plan": new_label, "folio_wahrheit": None,
                           "reason": "plan-key ist keine Seitenzahl"})
            continue
        if page < 1 or page - 1 not in verified:
            killed.append({"page": page, "plan": new_label, "folio_wahrheit": None,
                           "reason": "seite nicht folio-verifiziert"})
            continue
        if str(new_label).strip() != str(verified[page - 1]).strip():
            killed.append({"page": page, "plan": new_label, "folio_wahrheit": verified[page - 1],
                           "reason": "widerspricht folio"})
    # killed[].page uses the SAME 1-based convention as the plan keys.
    return {"accepted": not killed, "killed": killed, "geprueft_gegen": len(verified)}


def verify_plan(pdf_path: str, plan: dict[str, str]) -> dict:
    """Kill a relabel plan that contradicts folio-verified pages. Never writes."""
    import pymupdf  # type: ignore[import-not-found]
    from axiom_ng_runner.compute_core import page_trust as pt

    doc = pymupdf.open(pdf_path)
    try:
        verified = pt.verify_folio_sequence(pt.extract_folio_candidates(doc))
    finally:
        doc.close()
    return check_plan_against_folio(plan, verified)


def cmd_sweep(out_path: str, kandidaten_path: str | None) -> None:
    import psycopg2

    conn = psycopg2.connect(DSN)
    conn.set_session(readonly=True)
    cur = conn.cursor()
    cur.execute(
        """
        SELECT d.title, a.local_path, count(c.id) AS chunks
        FROM zotero_documents d
        JOIN zotero_attachments a ON a.document_id = d.id AND a.preferred AND NOT a.deleted
        JOIN processing_snapshots s ON s.attachment_id = a.id AND s.active
        JOIN processing_chunks c ON c.snapshot_id = s.id
        WHERE a.local_path ILIKE '%.pdf'
        GROUP BY d.title, a.local_path ORDER BY count(c.id) DESC
        """
    )
    rows = cur.fetchall()
    conn.close()

    reports = []
    for title, path, chunks in rows:
        p = (path or "").replace("file://", "")
        if not os.path.exists(p):
            reports.append({"titel": title, "chunks": chunks, "finding": "⚠️ PDF fehlt", "detail": p})
            continue
        try:
            r = analyze_pdf(p)
        except Exception as exc:  # noqa: BLE001 — one broken book must not stop the sweep
            reports.append({"titel": title, "chunks": chunks, "finding": "⚠️ Analysefehler", "detail": str(exc)[:80]})
            continue
        r["titel"] = title
        r["chunks"] = chunks
        reports.append(r)
        print(f"  {r['finding']:>22s}  {chunks:5d}  {title[:52]}", flush=True)

    order = {"🔴 reparierbar": 0, "🔴 unpaginiert": 1, "⚠️ PDF fehlt": 2, "⚠️ Analysefehler": 2,
             "🟡 Versatz-Verdacht": 3, "🟡 unklar (Label↔Folio uneinheitlich)": 3, "🟢 gesund": 4}
    reports.sort(key=lambda r: (order.get(r["finding"], 9), -r.get("chunks", 0)))

    with open(out_path, "w") as f:
        f.write(f"# ANALYZE-SWEEP — ganze Bibliothek ({len(reports)} preferred-PDFs)\n\n")
        f.write(f"Erzeugt: {__import__('datetime').datetime.now():%Y-%m-%d %H:%M} · read-only · Verdacht zuerst, dann Chunk-Zahl\n\n")
        f.write("| Verdacht | Chunks | Buch | Label-Befund | Folio-Läufe | Versatz |\n|---|---|---|---|---|---|\n")
        for r in reports:
            laufe = "; ".join(
                f"S.{l['start']} n={l['laenge']} ({l['folio_von']}–{l['folio_bis']})"
                for l in r.get("folio_laeufe", [])[:4]
            ) or "—"
            vers = f"{r['versatz']:+d}" if r.get("versatz") is not None else "—"
            f.write(f"| {r['finding']} | {r.get('chunks', 0)} | {r['titel'][:60]} | "
                    f"{r.get('label_befund', r.get('detail', ''))[:46]} | {laufe[:52]} | {vers} |\n")

        if kandidaten_path and os.path.exists(kandidaten_path):
            with open(kandidaten_path) as kf:
                kt = kf.read()
            stufe1 = re.findall(r"^- \*\*(.+?)\*\*", kt.split("STUFE 2")[0], re.MULTILINE)
            f.write(f"\n## Abgleich REPATUR_KANDIDATEN — Stufe 1 ({len(stufe1)} Bücher)\n\n")
            confirmed, exonerated, ungeprueft = [], [], []
            def _tokens(t: str) -> set[str]:
                return set(re.findall(r"[a-zäöüß]{3,}", t.lower()))
            seen = set()
            for k in stufe1:
                if k in seen:
                    continue
                seen.add(k)
                ktok = _tokens(k)
                hit = None
                best = 0
                for r in reports:
                    rt = _tokens(r["titel"])
                    # token-set subset match in either direction; exact short
                    # titles ("Controlling") and long ones both work, while
                    # series families (Demystifying vs Hill ESG) stay apart
                    if ktok and rt and (ktok <= rt or rt <= ktok):
                        ov = len(ktok & rt)
                        if ov > best:
                            best, hit = ov, r
                if hit is None:
                    ungeprueft.append(k)
                elif hit["finding"].startswith("🔴"):
                    confirmed.append((k, hit["finding"]))
                else:
                    exonerated.append((k, hit["finding"]))
            f.write(f"**Maschinell bestätigt kaputt: {len(confirmed)}** · entlastet (nur ungeprüft): "
                    f"{len(exonerated)} · im Sweep nicht gefunden: {len(ungeprueft)}\n\n")
            for k, v in confirmed:
                f.write(f"- 🔴 bestätigt: **{k}** — {v}\n")
            for k, v in exonerated:
                f.write(f"- 🟢/🟡 entlastet: {k} — {v}\n")
            for k in ungeprueft:
                f.write(f"- ? nicht gefunden: {k}\n")
    print(f"\nBericht: {out_path} ({len(reports)} Bücher)")


def main() -> int:
    ap = argparse.ArgumentParser(description=(__doc__ or "pdf_analyze").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    a = sub.add_parser("analyze"); a.add_argument("pdf")
    v = sub.add_parser("verify"); v.add_argument("pdf"); v.add_argument("--plan", required=True, help="JSON {page: label}")
    s = sub.add_parser("sweep"); s.add_argument("--out", default=os.path.expanduser("~/Desktop/ANALYZE_SWEEP.md"))
    s.add_argument("--kandidaten", default=os.path.expanduser("~/Desktop/REPATUR_KANDIDATEN.md"))
    args = ap.parse_args()
    if args.cmd == "analyze":
        print(json.dumps(analyze_pdf(args.pdf), ensure_ascii=False, indent=1))
    elif args.cmd == "verify":
        print(json.dumps(verify_plan(args.pdf, json.loads(args.plan)), ensure_ascii=False, indent=1))
    else:
        cmd_sweep(args.out, args.kandidaten)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
