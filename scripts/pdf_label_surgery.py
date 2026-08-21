#!/usr/bin/env python
"""#176 — deterministic in-place page-label surgery for Zotero storage PDFs.

Heals broken /PageLabels trees from measured probe truth (#195 campaign,
31 books, Cambridge-night procedure): classification is pure arithmetic on
the integrity-probe anchors (PDF label N vs chunk page M), repair is a
rebuilt label tree via pymupdf, proof is the three-way probe before AND
after. The tool never invents truth — chunk truth comes exclusively from
the probe (annotation label ≡ chunk page ≡ PDF label); folio runs from
the PDF text layer serve only as boundary evidence (the measured print
jump), exactly how the 31-book campaign localized print gaps.

Classes (issue #176):
  constant-offset   label(N) = chunk(M) + delta everywhere → patch firstpagenum
  reprint-start     internal numbering vs print start (labels == physical+1)
                    → set the range start value
  two-range         delta changes once at a print gap → two ranges, boundary
                    pinned by the measured folio jump (never guessed)
  injection         label-less or label-broken tree (e.g. C1-block), chunk
                    truth consistent → inject tree from chunk truth
  unclassifiable    variable/chaotic evidence → REFUSE with report

Style policy (C1 vs roman, decided 2026-08-22 — never invent a style):
  1. PRESERVE: existing ranges keep style/prefix verbatim when healing
     offsets (the broken part is ranges/starts, not style).
  2. ADOPT: styles carried by the chunk truth (roman front matter,
     M='viii') are taken from the measurement.
  3. STYLE VACUUM (injection, no evidence): roman front matter is the
     marked proposal (corpus convention) — an OPEN DECISION in the report;
     --style-override roman|arabic|prefix:C resolves it explicitly.

Discipline: dry-run is the DEFAULT (fs + DB fingerprint before/after as
proof it touched nothing). --apply is the only write path:
backup → write → read-back validation (auto-rollback on mismatch,
anchor truths enforced — the rot gate) → hash-sync zotero_attachments
(sha256/file_size/mtime_ms — prevents an unwanted re-harvest) → probe
re-verify (three-way green).

Exit codes: 0 ok · 3 REFUSED (unclassifiable / nothing to heal · never a
guess) · 2 ABORT (read-back mismatch, rolled back) · 1 error.

Usage:
  scripts/pdf_label_surgery.py KEY                # dry-run (default)
  scripts/pdf_label_surgery.py KEY --apply        # the only write path
  scripts/pdf_label_surgery.py KEY --pdf /tmp/copy.pdf --anchors a.json
      # off-storage mode (reproduced-case proof on a temp copy):
      # --pdf implies --no-probe and --no-db; measurement comes from
      # --anchors (JSON [{page, N, M}, ...], same fields as the probe).
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

BACKUP_DIR = Path("/tmp/axiom_runs/backups/pdf_labels")
DSN = os.environ.get(
    "AXIOM_DATABASE_URL",
    "postgresql://axiom_user:axiom_password@127.0.0.1:5432/axiom_db",
)
STORAGE = Path.home() / "Zotero" / "storage"
PROBE = Path(__file__).resolve().parent / "integrity_probe.py"

EXIT_OK, EXIT_ERROR, EXIT_ABORT, EXIT_REFUSE = 0, 1, 2, 3

_NUM = re.compile(r"^\d{1,4}$")
_ROMAN_VALS = {"i": 1, "v": 5, "x": 10, "l": 50, "c": 100, "d": 500, "m": 1000}


# --------------------------------------------------------------- basics ----

def sha256_hex(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def parse_roman(s: str) -> int | None:
    s = (s or "").strip().lower()
    if not s or any(ch not in _ROMAN_VALS for ch in s):
        return None
    total = 0
    for i, ch in enumerate(s):
        v = _ROMAN_VALS[ch]
        total += -v if i + 1 < len(s) and _ROMAN_VALS[s[i + 1]] > v else v
    return total if total > 0 else None


def to_roman(n: int) -> str:
    out: list[str] = []
    for v, sym in ((1000, "m"), (900, "cm"), (500, "d"), (400, "cd"), (100, "c"),
                   (90, "xc"), (50, "l"), (40, "xl"), (10, "x"), (9, "ix"),
                   (5, "v"), (4, "iv"), (1, "i")):
        while n >= v:
            out.append(sym)
            n -= v
    return "".join(out)


def fmt_label(rng: dict, value: int) -> str:
    style = rng.get("style", "D")
    core = to_roman(value) if style in ("r", "R") else str(value)
    if style == "R":
        core = core.upper()
    return rng.get("prefix", "") + core


def range_in_effect(spec: list[dict], page: int) -> dict | None:
    cur = None
    for r in spec:
        if r["startpage"] <= page:
            cur = r
    return cur


def expected_labels(spec: list[dict], n_pages: int) -> dict[int, str]:
    """Total label map a spec produces (PDF semantics: last range with
    startpage <= p wins; pages before the first range carry no label)."""
    out: dict[int, str] = {}
    for p in range(n_pages):
        r = range_in_effect(spec, p)
        if r is None:
            out[p] = ""
        else:
            out[p] = fmt_label(r, r.get("firstpagenum", 1) + p - r["startpage"])
    return out


def read_spec(pdf: Path) -> list[dict]:
    import pymupdf  # type: ignore[import-not-found]

    doc = pymupdf.open(str(pdf))
    try:
        return [dict(r) for r in (doc.get_page_labels() or [])]
    finally:
        doc.close()


def folio_evidence(pdf: Path) -> tuple[dict[int, int], list]:
    """Measured print numbers from the text layer (boundary evidence only —
    the same folio machinery the probe-trusted pipeline uses). Returns the
    raw numeric map (for gap pinning) and the proven +1 runs (for body-start
    localization)."""
    import pymupdf  # type: ignore[import-not-found]

    from axiom_ng_runner.compute_core import page_trust as pt

    doc = pymupdf.open(str(pdf))
    try:
        cands = pt.extract_folio_candidates(doc)
        runs = pt.folio_runs(cands)
    finally:
        doc.close()
    out: dict[int, int] = {}
    for p, v in cands.items():
        try:
            out[p] = int(v)
        except (TypeError, ValueError):
            continue
    return out, runs


# ------------------------------------------------------- classification ----

def fit_segments(pv: list[tuple[int, int]]) -> list[tuple[int, int]] | None:
    """[(page, value)] → [(first_page, value)] for a single constant
    segment or exactly two (monotone page split). Anything else: None."""
    if len({v for _, v in pv}) == 1:
        return [(pv[0][0], pv[0][1])]
    for k in range(1, len(pv)):
        if len({v for _, v in pv[:k]}) == 1 and len({v for _, v in pv[k:]}) == 1:
            return [(pv[0][0], pv[0][1]), (pv[k][0], pv[k][1])]
    return None


def pin_boundary(folio: dict[int, int], lo: int, hi: int, jump: int) -> int | None:
    """Physical page where the print arithmetic changes: the q→q+1 step
    inside [lo, hi] whose folio advance is 1+jump. Measured evidence only;
    no hit → None (the caller refuses rather than guess)."""
    for q in range(lo, hi):
        if q in folio and q + 1 in folio and folio[q + 1] - folio[q] == 1 + jump:
            return q + 1
    return None


def classify(anchors: list[dict], spec: list[dict], n_pages: int) -> dict:
    """Pure arithmetic on probe anchors → class + parameters. Anchors:
    [{"page": 0-based, "N": current pdf label, "M": chunk page}]."""
    arabic = sorted(
        (a["page"], int(a["M"]), int(a["N"]))
        for a in anchors if _NUM.match(str(a.get("M", ""))) and _NUM.match(str(a.get("N", "")))
    )
    arabic_m = sorted(
        (a["page"], int(a["M"]))
        for a in anchors if _NUM.match(str(a.get("M", "")))
    )
    roman = sorted(
        (a["page"], parse_roman(a["M"]), a["M"])
        for a in anchors if parse_roman(a.get("M", "")) is not None
    )
    roman = [(p, v, raw) for p, v, raw in roman if v]

    if len(arabic_m) < 2:
        return {"klasse": "unclassifiable",
                "grund": f"nur {len(arabic_m)} arabische Messpunkte (≥2 nötig)"}

    labels_numeric = len(arabic) == len(arabic_m) and bool(spec)
    if labels_numeric:
        deltas = [(p, m - n) for p, m, n in arabic]
        segs = fit_segments(deltas)
        if segs is None:
            return {"klasse": "unclassifiable",
                    "grund": f"Versatz variabel: {sorted({d for _, d in deltas})}"}
        identity = all(n == p + 1 for p, _, n in arabic)  # labels == physical+1
        klasse = ("reprint-start" if identity else "constant-offset") if len(segs) == 1 \
            else "two-range"
        return {"klasse": klasse,
                "deltas": sorted({d for _, d in deltas}),
                "segs": segs, "arabic": arabic, "roman": roman,
                "delta0": deltas[0][1]}
    # label-broken (C1-Block, leer, nicht-numerisch): injection aus Chunk-Wahrheit
    shifted = [(p, m - p) for p, m in arabic_m]
    segs = fit_segments(shifted)
    if segs is None:
        return {"klasse": "unclassifiable",
                "grund": "Chunk-Wahrheit nicht als Bereichs-Arithmetik darstellbar "
                         f"({sorted({v for _, v in shifted})})"}
    return {"klasse": "injection",
            "segs": segs, "arabic": arabic_m, "roman": roman}


def build_plan(klas: dict, spec: list[dict], n_pages: int,
               folio: dict[int, int], runs: list, style_override: str | None) -> dict:
    """Class + evidence → the new label spec + expected anchor truths.

    Style policy: PRESERVE untouched ranges (offset healing), ADOPT roman
    from chunk truth (injection), style vacuum → roman proposal flagged as
    an OPEN DECISION (--style-override resolves it)."""
    arabic, roman = klas["arabic"], klas["roman"]
    segs = klas["segs"]
    klasse = klas["klasse"]
    open_decision = None

    if klasse in ("constant-offset", "reprint-start"):
        if klas["delta0"] == 0:
            return {"refuse": "Labels already ≡ chunk truth (delta 0) — kein Eingriff nötig"}
        new_spec = []
        for r in spec:
            r2 = dict(r)
            if any(r["startpage"] <= p and (range_in_effect(spec, p) is r) for p, _, _ in arabic):
                r2["firstpagenum"] = r.get("firstpagenum", 1) + klas["delta0"]
            new_spec.append(r2)
        return {"spec": new_spec, "open_decision": None,
                "style_note": "vorhandene Bereiche unverändert erhalten (PRESERVE)"}

    if klasse == "two-range":
        p1, p2 = segs[0][0], segs[1][0]
        d1, d2 = klas["deltas"][0], klas["deltas"][-1]
        if range_in_effect(spec, p1) is not range_in_effect(spec, p2):
            return {"refuse": "Bestandsbaum hat eine eigene Bereichsgrenze im "
                             "Messfenster — kein zwei-Bereiche-Fall, verweigert"}
        b = pin_boundary(folio, p1, p2, d2 - d1)
        if b is None:
            return {"refuse": f"two-range boundary between Anker {p1} und {p2} nicht "
                              f"messbar (kein Folio-Sprung 1{d2 - d1:+d}) — verweigert, "
                              "nie raten"}
        new_spec = []
        for r in spec:
            r2 = dict(r)
            if any(r["startpage"] <= p and (range_in_effect(spec, p) is r)
                   for p, _, _ in arabic if p < b):
                r2["firstpagenum"] = r.get("firstpagenum", 1) + d1
            new_spec.append(r2)
        r1 = range_in_effect(new_spec, p1) or {}
        m2 = next(m for p, m, _ in arabic if p >= b)
        new_spec.append({"startpage": b, "prefix": r1.get("prefix", ""),
                         "style": r1.get("style", "D"),
                         "firstpagenum": m2 - (p2 - b)})
        new_spec.sort(key=lambda r: r["startpage"])
        return {"spec": new_spec, "open_decision": None,
                "style_note": f"Bereichsgrenze S.{b + 1} aus Folio-Sprung 1{d2 - d1:+d} "
                              "(PRESERVE für Stile)"}

    # injection
    p0, m0 = arabic[0]
    run = next(((pages, vals) for pages, vals in runs if pages[0] <= p0 <= pages[-1]), None)
    body_start = run[0][0] if run else p0
    gap_note = None
    if run and roman and run[0][0] in folio:
        # Echte Druck-Lücke (fehlende Fußzeilen) zwischen letzter römischer
        # Messung und Folio-Lauf: der Lauf trägt seine Arithmetik selbst —
        # der Body-Start ist die Seite, an der der Rücklauf den Wert 1
        # erreicht (Korpus-Konvention, deterministisch aus der Messung).
        # Die Lücke wird markiert, nie verschwiegen.
        last_roman_q = max(q for q, _, _ in roman)
        one_page = run[0][0] - (folio[run[0][0]] - 1)
        if last_roman_q + 1 <= one_page < run[0][0]:
            body_start = one_page
            gap_note = (f"Folio-Lücke S.{last_roman_q + 2}–{run[0][0]}: Body-Start "
                        f"S.{one_page + 1} aus arithmetischem Rücklauf (Wert 1) — "
                        "Lücken-Seiten sind nicht ankerbewiesen")
    front = []
    if body_start > 0:
        if roman:
            if len({v - q for q, v, _ in roman}) > 1:
                return {"refuse": "romanische Anker untereinander inkonsistent — verweigert"}
            q, v, raw = roman[-1]
            rstyle = "R" if raw.isupper() else "r"   # Schreibweise aus Wahrheit (ADOPT)
            first_roman_page = q - v + 1  # page where roman 'i' begins
            if v < 1 or first_roman_page > body_start:
                return {"refuse": f"romanischer Anker (S.{q + 1}, '{to_roman(v)}') "
                                  "inkonsistent zum Body-Beginn — verweigert"}
            front = []
            if first_roman_page <= 0:
                front.append({"startpage": 0, "prefix": "", "style": rstyle,
                              "firstpagenum": v - q})   # römisch ab Cover
            else:
                # pymupdf kann Seiten vor der ersten Range nicht lesen (Index-
                # Error) — der Baum MUSS Seite 1 decken. Unnummerierte Cover-
                # Seiten bekommen den Cover-Präfix: das im Korpus bewährte
                # Muster (C1 Cover → i–vii → 1–244, Fallstudie #2), nie eine
                # zitierfähige Seite. Bestehender Präfix-Bereich bleibt (PRESERVE).
                cover = next((dict(r) for r in spec if r["startpage"] == 0
                              and r.get("prefix")), None)
                front.append(cover or {"startpage": 0, "prefix": "C", "style": "D",
                                       "firstpagenum": 1})
                front.append({"startpage": first_roman_page, "prefix": "", "style": rstyle,
                              "firstpagenum": 1})
            style_note = f"Vorspann römisch ({rstyle}) aus Chunk-Wahrheit (ADOPT: M='{raw}')"
        elif style_override == "arabic":
            front = [{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}]
            style_note = "Vorspann arabisch (--style-override arabic)"
        elif style_override and style_override.startswith("prefix:"):
            front = [{"startpage": 0, "prefix": style_override[7:], "style": "D",
                      "firstpagenum": 1}]
            style_note = f"Vorspann '{style_override[7:]}' (--style-override)"
        elif style_override == "roman":
            front = [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1}]
            style_note = "Vorspann römisch (--style-override roman)"
        else:
            front = [{"startpage": 0, "prefix": "", "style": "r", "firstpagenum": 1}]
            style_note = "Vorspann-Stil nicht ableitbar — Vorschlag römisch (häufigste Konvention)"
            open_decision = ("Vorspann-Stil aus den Daten NICHT entscheidbar (kein romanischer "
                             "Anker, kein bestehender Baum). Vorschlag: römisch. Explizit "
                             "machen: --style-override roman|arabic|prefix:C")
    else:
        style_note = "kein Vorspann (Body ab Seite 1)"

    body: list[dict] = [{"startpage": body_start, "prefix": "", "style": "D",
                         "firstpagenum": m0 - (p0 - body_start)}]
    if len(segs) == 2:
        (s1_page, _), (s2_page, _) = segs
        v1, v2 = shifted_values(klas)
        b = pin_boundary(folio, s1_page, s2_page, v2 - v1)
        if b is None:
            return {"refuse": f"two-range boundary zwischen S.{s1_page + 1} und S.{s2_page + 1} "
                              f"nicht messbar (Folio-Sprung 1{v2 - v1:+d} fehlt) — verweigert"}
        m2 = next(m for p, m in arabic if p >= s2_page)
        body = [{"startpage": body_start, "prefix": "", "style": "D",
                 "firstpagenum": m0 - (p0 - body_start)},
                {"startpage": b, "prefix": "", "style": "D",
                 "firstpagenum": m2 - (s2_page - b)}]
        style_note += f" · Bereichsgrenze S.{b + 1} aus Folio-Sprung 1{v2 - v1:+d}"

    spec_new = front + body
    spec_new.sort(key=lambda r: r["startpage"])
    if gap_note:
        style_note += " · " + gap_note
    return {"spec": spec_new, "open_decision": open_decision, "style_note": style_note}


def shifted_values(klas: dict) -> tuple[int, int]:
    """The two distinct M−p values of an injection two-range case."""
    vals = sorted({m - p for p, m in klas["arabic"]})
    return vals[0], vals[-1]


# ------------------------------------------------------------- surgery ----

def write_labels(pdf: Path, spec: list[dict], backup: Path) -> dict:
    """Backup → write → read-back validation. Returns {"ok": ...}; any
    mismatch restores the backup (auto-rollback)."""
    import pymupdf  # type: ignore[import-not-found]

    backup.parent.mkdir(parents=True, exist_ok=True)
    pristine = not (backup.exists() and sha256_hex(backup) == sha256_hex(pdf))
    if pristine:
        if backup.exists():  # stale backup from an earlier session — never
            # overwrite the original pristine copy, roll back THIS run via a
            # side-by-side copy instead
            backup = backup.with_name(f"{backup.stem}.{int(time.time())}.pdf")
        shutil.copy2(pdf, backup)

    doc = pymupdf.open(str(pdf))
    n = doc.page_count
    try:
        doc.set_page_labels(spec)
        tmp = pdf.with_suffix(".surgery.tmp.pdf")
        doc.save(str(tmp))
        doc.close()
        os.replace(tmp, pdf)
    except Exception:  # Rollback, dann re-raise
        doc.close()
        tmp = pdf.with_suffix(".surgery.tmp.pdf")
        tmp.unlink(missing_ok=True)
        if pristine:
            shutil.copy2(backup, pdf)
        raise

    doc2 = pymupdf.open(str(pdf))
    try:
        written = {p: doc2[p].get_label() for p in range(doc2.page_count)}
    finally:
        doc2.close()
    expected = expected_labels(spec, n)
    mismatches = [p for p in range(n) if written.get(p, "") != expected.get(p, "")]
    if mismatches:
        shutil.copy2(backup, pdf)  # Auto-Rollback
        return {"ok": False, "mismatches": mismatches[:10], "n_mismatch": len(mismatches),
                "backup": str(backup), "rollback": True}
    return {"ok": True, "pages": n, "backup": str(backup), "rollback": False,
            "pristine_backup_created": pristine}


def validate_anchor_truths(pdf: Path, anchors: list[dict]) -> list[str]:
    """The rot gate: after writing, every probe anchor's label MUST equal
    the chunk truth M. A crippled classifier producing a self-consistent
    but wrong tree dies HERE, not in silence."""
    import pymupdf  # type: ignore[import-not-found]

    doc = pymupdf.open(str(pdf))
    try:
        bad = []
        for a in anchors:
            lbl = doc[a["page"]].get_label()
            if lbl.strip().lower() != str(a["M"]).strip().lower():
                bad.append(f"S.{a['page'] + 1}: label='{lbl}' != chunk M='{a['M']}'")
        return bad
    finally:
        doc.close()


def db_row(dsn: str, key: str):
    import psycopg2

    conn = psycopg2.connect(dsn)
    conn.set_session(readonly=True)
    try:
        cur = conn.cursor()
        cur.execute(
            "SELECT content_hash, file_size, mtime_ms FROM zotero_attachments "
            "WHERE zotero_key = %s AND NOT deleted", (key,))
        row = cur.fetchone()
        return tuple(row) if row else None
    finally:
        conn.close()


def db_hash_sync(dsn: str, key: str, pdf: Path) -> dict:
    """Pflicht nach jedem Schreibzugriff: sonst löst der nächste Sync einen
    ungewollten Re-Harvest aus."""
    import psycopg2

    new_hash, size, mtime = sha256_hex(pdf), pdf.stat().st_size, int(time.time() * 1000)
    conn = psycopg2.connect(dsn)
    try:
        cur = conn.cursor()
        cur.execute(
            "UPDATE zotero_attachments SET content_hash=%s, file_size=%s, mtime_ms=%s, "
            "updated_at=now() WHERE zotero_key=%s AND NOT deleted",
            (new_hash, size, mtime, key))
        conn.commit()
        return {"rowcount": cur.rowcount, "content_hash": new_hash,
                "file_size": size, "mtime_ms": mtime}
    finally:
        conn.close()


# --------------------------------------------------------------- probe ----

def run_probe(probe: Path, key: str) -> dict:
    """Vorher/Nachher-Beweis via integrity_probe (--dry): das Messwerkzeug
    bleibt die einzige Wahrheitsquelle. Returns parsed probe JSON."""
    cmd = [sys.executable, str(probe), "--dry", key]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=600,
                          check=False)  # rc landet im Bericht (Beweis), kein Raise
    out = proc.stdout
    try:
        start = out.index("{")
        payload, _ = json.JSONDecoder().raw_decode(out[start:])
    except (ValueError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"probe output nicht parsebar: {exc}\nstdout: {out[:400]}\n"
                           f"stderr: {proc.stderr[:400]}") from exc
    payload["_cmd"] = " ".join(cmd)
    payload["_rc"] = proc.returncode
    return payload


def anchors_from_probe(probe_out: dict) -> list[dict]:
    out = []
    for a in probe_out.get("anchors", []):
        if a.get("verdict") == "BLOCKER":
            continue
        page, m = a.get("page_index"), (a.get("chunk") or {}).get("page")
        if page is None or m is None:
            continue
        out.append({"page": int(page), "N": str(a.get("pdf_label") or ""),
                    "M": str(m)})
    return out


def probe_verdict(probe_out: dict) -> str:
    return str(probe_out.get("verdict", "?"))


# -------------------------------------------------------------- report ----

def hr(title: str) -> None:
    print(f"\n──── {title} " + "─" * max(0, 66 - len(title)))


def fingerprint(pdf: Path) -> str:
    st = pdf.stat()
    return f"sha256={sha256_hex(pdf)[:16]}… size={st.st_size} mtime_ns={st.st_mtime_ns}"


def main() -> int:
    ap = argparse.ArgumentParser(description="deterministic PDF label surgery (#176)")
    ap.add_argument("key", help="Zotero storage KEY")
    ap.add_argument("--apply", action="store_true", help="der einzige Schreibpfad (Default: Dry-Run)")
    ap.add_argument("--pdf", help="off-storage Zielpfad (impliziert --no-probe/--no-db; Reproduced-Case-Beweis)")
    ap.add_argument("--anchors", type=Path, help="Messung als JSON [{page,N,M}] statt Probe (Tests/offline)")
    ap.add_argument("--style-override", help="roman|arabic|prefix:C — stilisiert den offenen Vorspann-Entscheidungspunkt")
    ap.add_argument("--probe", type=Path, default=PROBE)
    ap.add_argument("--dsn", default=DSN)
    ap.add_argument("--no-db", action="store_true")
    args = ap.parse_args()

    use_db = not args.no_db and not args.pdf
    use_probe = not args.pdf

    pdf = Path(args.pdf) if args.pdf else next(
        (STORAGE / args.key).glob("*.pdf"), None)
    if not pdf or not pdf.exists():
        print(f"ERROR: PDF nicht gefunden ({args.key})")
        return EXIT_ERROR

    print(f"════ pdf_label_surgery — {args.key} ════")
    print(f"PDF: {pdf}  ·  Modus: {'APPLY' if args.apply else 'DRY-RUN (Default)'}")

    # 1) Messung — nie eigene Wahrheit
    hr("1) MESSUNG (integrity_probe — die einzige Wahrheitsquelle)")
    probe_before = None
    if use_probe:
        try:
            probe_before = run_probe(args.probe, args.key)
        except Exception as exc:  # noqa: BLE001 — Beweisforderung: ohne Sonde kein Schnitt
            print(f"ERROR: Probe nicht erreichbar — {exc}")
            return EXIT_ERROR
        print(f"cmd: {probe_before['_cmd']}  (rc={probe_before['_rc']})")
        print(f"verdict: {probe_verdict(probe_before)}")
        anchors = anchors_from_probe(probe_before)
        for a in probe_before.get("anchors", []):
            print(f"  {a.get('position', '?'):7s} S.{(a.get('page_index', 0) or 0) + 1:<4d} "
                  f"N={a.get('N', a.get('pdf_label', '—'))!r:<8} M={a.get('M', '—')!r} "
                  f"[{a.get('verdict', '?')}]")
        if probe_verdict(probe_before) == "MATCH":
            print("→ Dreifachmessung bereits MATCH — kein Eingriff indiziert")
            return EXIT_OK
    else:
        if not args.anchors:
            print("ERROR: --pdf-Modus braucht --anchors (Messung einer fremden Datei)")
            return EXIT_ERROR
        anchors = json.loads(args.anchors.read_text())
        print(f"Messung: --anchors {args.anchors} ({len(anchors)} Anker, offline)")

    spec = read_spec(pdf)
    import pymupdf  # type: ignore[import-not-found]
    doc = pymupdf.open(str(pdf))
    n_pages = doc.page_count
    doc.close()
    print(f"aktueller Label-Baum: {spec or '—'}")

    # 2) Klassifikation — reine Arithmetik
    hr("2) KLASSIFIKATION (Deltas-Anker-Arithmetik)")
    klas = classify(anchors, spec, n_pages)
    deltas = sorted({int(a["M"]) - int(a["N"]) for a in anchors
                     if _NUM.match(str(a.get("M", ""))) and _NUM.match(str(a.get("N", "")))})
    print(f"Anker-Deltas (M−N): {deltas or '—'}")
    print(f"Klasse: {klas['klasse']}")
    if klas["klasse"] == "unclassifiable":
        print(f"GRUND: {klas['grund']}")
        print("→ REFUSE. Variabel/unentscheidbar ist meldungspflichtig — Raten ist verboten.")
        return EXIT_REFUSE

    # 3) Plan
    hr("3) HEILUNGSPLAN")
    folio, runs = folio_evidence(pdf)
    plan = build_plan(klas, spec, n_pages, folio, runs, args.style_override)
    if "refuse" in plan:
        print(f"→ REFUSE: {plan['refuse']}")
        return EXIT_REFUSE
    new_spec = plan["spec"]
    expected = expected_labels(new_spec, n_pages)
    print(f"geplante Ranges: {json.dumps(new_spec, ensure_ascii=False)}")
    print(f"Stil-Politik: {plan['style_note']}")
    if plan["open_decision"]:
        print(f"\n⚠ OFFENER ENTSCHEIDUNGSPUNKT: {plan['open_decision']}")
    for a in anchors:
        print(f"  Anker S.{a['page'] + 1}: label → '{expected.get(a['page'], '?')}' "
              f"(Chunk-Wahrheit M='{a['M']}')")

    backup = BACKUP_DIR / f"{args.key}.pdf" if not args.pdf else \
        BACKUP_DIR / f"{pdf.stem}.pdf"

    # 4) Dry-run / Apply
    if not args.apply:
        hr("4) DRY-RUN — kein einziger Schreibzugriff (Beweis)")
        fs0, row0 = fingerprint(pdf), (db_row(args.dsn, args.key) if use_db else None)
        tmp = Path(f"/tmp/axiom_runs/surgery_dryrun_{args.key}.pdf")
        tmp.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(pdf, tmp)
        res = write_labels(tmp, new_spec, tmp.with_suffix(".bak"))
        target_hash = sha256_hex(tmp) if res["ok"] else None
        for p in (tmp, tmp.with_suffix(".bak")):
            p.unlink(missing_ok=True)
        fs1, row1 = fingerprint(pdf), (db_row(args.dsn, args.key) if use_db else None)
        print(f"Filesystem vor:  {fs0}")
        print(f"Filesystem nach: {fs1}  {'✓ unverändert' if fs0 == fs1 else '✗ VERÄNDERT!'}")
        if use_db:
            print(f"DB-Row vor:  {row0}")
            print(f"DB-Row nach: {row1}  {'✓ unverändert' if row0 == row1 else '✗ VERÄNDERT!'}")
            if row0 is None:
                print("ERROR: keine zotero_attachments-Row — Apply würde ohne Hash-Sync enden")
                return EXIT_ERROR
        if not res["ok"]:
            print(f"Simulation auf Temp-Kopie GESCHEITERT: {res}")
            return EXIT_ABORT
        print(f"Ziel-Hash (Simulation auf Temp-Kopie, verworfen): {target_hash}")
        print("→ APPLY mit: --apply")
        return EXIT_OK

    hr("4) APPLY — backup → schreiben → read-back → hash-sync")
    fs0 = fingerprint(pdf)
    res = write_labels(pdf, new_spec, backup)
    print(f"backup: {res['backup']}{' (neu, pristine)' if res.get('pristine_backup_created') else ' (existierte — pristine erhalten)'}")
    if not res["ok"]:
        print(f"READ-BACK MISMATCH ({res['n_mismatch']} Seiten, z.B. {res['mismatches']})")
        print("→ AUTO-ROLLBACK ausgeführt (Backup zurückkopiert). Abbruch.")
        return EXIT_ABORT
    print(f"read-back: ✓ alle {res['pages']} Seiten Label == Erwartung")

    bad = validate_anchor_truths(pdf, anchors)
    if bad:
        print("ROT-SONDE: Anker-Wahrheit verletzt — Klassifikator/Plan war falsch:")
        for b in bad:
            print(f"  ✗ {b}")
        shutil.copy2(res["backup"], pdf)   # der tatsächlich verwendete Backup-Pfad
        print("→ AUTO-ROLLBACK ausgeführt. Abbruch (stille Falsheit ist verboten).")
        return EXIT_ABORT
    print("Anker-Wahrheit: ✓ jedes gemessene M == geschriebenem Label (Rot-Gate grün)")

    if use_db:
        sync = db_hash_sync(args.dsn, args.key, pdf)
        if sync["rowcount"] != 1:
            print(f"ERROR: hash-sync rowcount={sync['rowcount']} (erwartet 1)")
            return EXIT_ERROR
        print(f"hash-sync: ✓ content_hash={sync['content_hash'][:16]}… "
              f"file_size={sync['file_size']} mtime_ms={sync['mtime_ms']} "
              "(Re-Harvest verhindert)")
    print(f"Filesystem vor:  {fs0}")
    print(f"Filesystem nach: {fingerprint(pdf)}  (Absicht — Heilung)")

    # 5) Nachher-Beweis
    if use_probe:
        hr("5) NACHHER — integrity_probe erneut")
        probe_after = run_probe(args.probe, args.key)
        print(f"cmd: {probe_after['_cmd']}  (rc={probe_after['_rc']})")
        print(f"verdict: {probe_verdict(probe_after)}")
        for a in probe_after.get("anchors", []):
            print(f"  {a.get('position', '?'):7s} S.{(a.get('page_index', 0) or 0) + 1:<4d} "
                  f"N={a.get('N', a.get('pdf_label', '—'))!r:<8} M={a.get('M', '—')!r} "
                  f"[{a.get('verdict', '?')}]")
        if probe_verdict(probe_after) != "MATCH":
            print("→ DREIFACH NICHT GRÜN — Heilung nicht bewiesen. "
                  "Backup liegt bereit: " + str(backup))
            return EXIT_ABORT
        print("→ DREIFACH GRÜN: Annotation-Label ≡ Chunk-Seite ≡ PDF-Label")

    print("\n════ Heilung bewiesen · RAG unberührt (kein Rechunk nötig — Hash-Sync "
          "verhindert ihn) ════")
    return EXIT_OK


if __name__ == "__main__":
    raise SystemExit(main())
