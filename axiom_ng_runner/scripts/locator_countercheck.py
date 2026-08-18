"""#173 slice 4 — Versatz-Tabelle counter-check (read-only).

dudu's physical verification (VERIFIKATION_ERGEBNIS.md, 45 gold refs across
29 books) is the Golden Reference for folio-run alignment. For every entry
with a numeric PDF↔print pair we check: does build_page_trust's folio map
reproduce dudu's MEASURED printed page at his PDF page?

Output per book: ALIGNS (my folios = his measurements at EVERY measured
entry) or ABWEICHEND (any miss / no resolvable PDF). Books classified constant-offset vs
chapter-relative (his offsets vary per entry within one book = restarts).
The resulting evidence file (basenames) feeds locator_rescan.py --evidence-file.
"""
from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import psycopg2
from axiom_ng_runner.compute_core import page_trust as pt

DSN = os.environ.get(
    "AXIOM_DATABASE_URL",
    "postgresql://axiom_user:axiom_password@127.0.0.1:5432/axiom_db?sslmode=disable",
)

# dudu's measurements: id -> (status, pdf_page_1based, printed_page_str)
# parsed from VERIFIKATION_ERGEBNIS.md — kept inline as the immutable record.
VERSATZ = {
    "qa1": ("ok", 5, "V"), "qa2": ("falsch", 180, "152"), "qa3": ("ok", 210, "209"),
    "qa5": ("ok", 13, "XIII"), "c6": ("ok", 34, "3"), "c7": ("ok", 20, "8"),
    "c8": ("ok", 10, "XI"), "c9": ("ok", 139, "135"), "c10": ("ok", 245, "245"),
    "c13": ("ok", 1, "C1"), "c14": ("ok", 12, "1"), "f16": ("ok", 34, "23"),
    "f17": ("falsch", 182, "154"), "n18": ("ok", 72, "55"), "n19": ("ok", 175, "162"),
    "n20": ("ok", 114, "99"), "a22": ("ok", 91, "82"), "a23": ("ok", 302, "291"),
    "a24": ("ok", 25, "1"), "f25": ("ok", 188, "167"), "z1": ("ok", 23, "5"),
    "z2": ("ok", 29, "11"), "z3": ("ok", 30, "19"), "z4": ("ok", 569, "551"),
    "z5": ("falsch", 12, "4"), "z6": ("ok", 17, "4"), "z7": ("ok", 269, "258"),
    "w1": ("ok", 677, "663"), "w3": ("ok", 8, "7"), "w4": ("ok", 366, "342"),
    "w5": ("ok", 63, "34"), "w7": ("ok", 28, "6"), "w8": ("ok", 243, "242"),
    "w9": ("ok", 266, "244"), "w11": ("ok", 633, "603"), "w14": ("ok", 96, "87"),
    "w15": ("ok", 944, "914"), "w18": ("ok", 223, "199"), "w24": ("ok", 244, "243"),
    "o1": ("ok", 3, "20"), "o2": ("ok", 238, "228"), "o3": ("ok", 45, "43"),
    "o4": ("falsch", 17, "L 333/96"), "o5": ("ok", 132, "110"), "o6": ("ok", 81, "81"),
}


def main() -> int:
    with open(os.environ.get("AXIOM_GOLDSTELLEN", "/Users/dudu/Desktop/GOLDSTELLEN_ZUR_VERIFIKATION.md")) as f:
        gold_txt = f.read()
    id2book = dict(
        re.findall(r"### (\w+\d+) — .*?\n> \*\*Buch:\*\* (.*?) · \*\*S\.\*\*", gold_txt)
    )

    out_books: list[str] = []
    conn = psycopg2.connect(DSN)
    conn.set_session(readonly=True)
    cur = conn.cursor()
    # unambiguous anchor: dudu's chunk short-ids -> snapshot -> attachment PDF
    cur.execute(
        "SELECT left(c.id::text, 8), a.local_path FROM processing_chunks c "
        "JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active "
        "JOIN zotero_attachments a ON a.id = s.attachment_id"
    )
    chunk2pdf = {k: (v or "").replace("file://", "") for k, v in cur.fetchall()}
    conn.close()


    id2chunk = dict(
        re.findall(r"### (\w+\d+) — .*?\n> \*\*Buch:\*\* .*? · \*\*S\.\*\* .*? · \*\*Chunk\*\* `(\w+)`", gold_txt)
    )
    per_book: dict[str, list] = {}
    for gid, (status, pdf_page, printed) in VERSATZ.items():
        book = id2book.get(gid)
        if not book:
            print(f"?? {gid}: kein Buch in Gold-Mapping")
            continue
        cid = id2chunk.get(gid)
        if not cid:
            per_book.setdefault(book, []).append((gid, status, "KEIN-MAPPING"))
            continue
        pdf = chunk2pdf.get(cid)
        if pdf is None:
            per_book.setdefault(book, []).append((gid, status, "KEIN-CHUNK"))
            continue
        idx = int(pdf_page) - 1
        if not os.path.exists(pdf):
            per_book.setdefault(book, []).append((gid, status, "PDF-FEHLT"))
            continue
        labels, sources, _chapters = pt.build_page_trust(pdf)
        mine = labels.get(idx)
        lvl = sources.get(idx)
        want = printed.upper() if not printed[0].isdigit() else str(int(printed)) if printed.isdigit() else printed
        got = str(mine).upper() if mine is not None and not str(mine)[0].isdigit() else str(mine) if mine is not None else None
        match = got == want
        per_book.setdefault(book, []).append((gid, status, pdf_page, printed, mine, lvl, "TREFF" if match else "ABW"))

    print(f"{'Buch':52s} Einträge  Ergebnis")
    aligned, misaligned = [], []
    for book, rows in per_book.items():
        hits = sum(1 for r in rows if r[-1] == "TREFF")
        offs = {r[2] - int(r[3]) for r in rows if r[-1] == "TREFF" and len(r) > 4 and str(r[3]).isdigit() and r[4] is not None and str(r[4]).isdigit()}
        tag = "ALIGNS" if hits == len(rows) else ("TEILWEISE" if hits else "ABWEICHEND")
        form = f"Versatz {'konstant ' + str(sorted(offs)) if len(offs) == 1 else 'variabel=' + str(sorted(offs))}" if offs else ""
        print(f"{book[:52]:52s} {hits}/{len(rows):>2d}      {tag}  {form}")
        for r in rows:
            if r[-1] != "TREFF":
                detail = (
                    f"PDF {r[2]} — dudu {r[3]} vs meine {r[4]} [{r[5]}]"
                    if len(r) > 4
                    else r[2]  # short row: the reason (KEIN-MAPPING/KEIN-CHUNK/PDF-FEHLT), not a page
                )
                print(f"     · {r[0]} ({r[1]}): {detail}")
        (aligned if tag == "ALIGNS" else misaligned).append(book)
        if tag == "ALIGNS" and rows:
            b = chunk2pdf.get(id2chunk.get(rows[0][0], ""))
            if b and os.path.exists(b):
                out_books.append(os.path.basename(b))
    print(f"\nALIGNS: {len(aligned)} Bücher · nicht voll: {len(misaligned)}")
    ev = {"heal_books": sorted(out_books),
          "skip_books": ["EU+-+2022+-+NIS-2-Richtlinie+(2022-2555).pdf"]}
    out = Path(__file__).parent / "evidence_books.json"
    with out.open("w") as f:
        json.dump(ev, f, ensure_ascii=False, indent=1)
    print(f"Evidenz-Datei: {out} ({len(out_books)} Bücher + NIS2-Skip)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
