"""#173 slice 3 — corpus locator re-trust (scan + re-persist, NO rechunk).

The legacy corpus predates page_source: every active chunk's locator is
re-derived from its document's PDF via the trust pipeline (build_page_trust)
and the locator JSONB is updated IN PLACE (locator field only — text,
embeddings, structure stay byte-identical).

  epub_cfi  -> page_source "none"
  page_span -> page_source from the start page's trust level; under
               folio_verified the page labels are CORRECTED to the printed
               folios (the qa2/f17/z5 faults heal); pdf_label_sane keeps the
               publisher labels; physical_only keeps physical indices.

Modes: default = dry scan (statistics + symptom list only);
--apply writes DB (processing_chunks.locator) + OpenSearch bulk updates;
--apply-epub-only writes ONLY the unambiguous epub_cfi -> none stamps
and leaves every PDF locator untouched until slice 4 (dudu's
Versatz-Tabelle) validates the heal set. NOTE (review-corrected): the
z5 spot check heals CORRECTLY (Altenburger: PDF page 12 -> folio '4',
folio_verified, 227/250 pages) — an earlier draft claimed a
misalignment based on a cross-book mix-up (Hentze is z3, not z5). The
real open question for the Tabelle: whether CHAPTER-RELATIVE folios
(World Bank class — front-section restarts the folio verifier
correctly resolves) are the citable page form dudu wants, and whether
all 3,735 heal candidates match the 41 verified references.
Idempotent by construction: a second run reports 0 changes (see the
DB/OS retry note at the bulk loop for the partial-failure caveat).
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

import psycopg2
import psycopg2.extras
import requests
from axiom_ng_runner.compute_core import page_trust as pt

OS_URL = os.environ.get("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200")
OS_INDEX = "axiom-ng-chunks-v1"
DSN = os.environ.get(
    "AXIOM_DATABASE_URL",
    "postgresql://axiom_user:axiom_password@127.0.0.1:5432/axiom_db?sslmode=disable",
)


def norm_path(p: str | None) -> str | None:
    if not p:
        return None
    return p.replace("file://", "")


def load_active_chunks(cur):
    """One row per active chunk: id, attachment pdf path, locator."""
    cur.execute(
        """
        SELECT c.id::text, c.locator, a.local_path
        FROM processing_chunks c
        JOIN processing_snapshots s ON s.id = c.snapshot_id AND s.active
        JOIN zotero_attachments a ON a.id = s.attachment_id
        ORDER BY a.id, c.chunk_index
        """
    )
    return cur.fetchall()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--apply", action="store_true", help="write DB + OS (default: dry)")
    ap.add_argument("--apply-epub-only", action="store_true",
                    help="write ONLY epub_cfi -> none stamps (safe half; PDF locators wait for the slice-4 counter-proof)")
    ap.add_argument("--evidence-file", type=argparse.FileType("r"),
                    help="JSON from locator_countercheck.py: {heal_books, skip_books}. "
                         "heal_books = counter-check-verified (full stamping incl. folio heals); "
                         "skip_books = stamp NOTHING (Amtsblatt etc.); other books get safe levels only "
                         "(no folio_verified claim, no label change) — silent-wrong stays impossible")
    args = ap.parse_args()
    if args.apply and args.apply_epub_only:
        ap.error("--apply and --apply-epub-only are mutually exclusive (scope vs full write)")
    args.apply = args.apply or args.apply_epub_only
    evidence = json.load(args.evidence_file) if args.evidence_file else None
    heal_books = set(evidence["heal_books"]) if evidence else set()
    skip_books = set(evidence["skip_books"]) if evidence else set()

    conn = psycopg2.connect(DSN)
    conn.set_session(readonly=not args.apply, autocommit=False)
    cur = conn.cursor()
    rows = load_active_chunks(cur)
    print(f"aktive Chunks: {len(rows)}")

    # Trust map per distinct PDF path (one build_page_trust pass per book).
    trust_cache: dict[str, tuple[dict, dict] | None] = {}
    for _, _, p in rows:
        key = norm_path(p)
        if key and key not in trust_cache and key.lower().endswith(".pdf") and os.path.exists(key):
            try:
                trust_cache[key] = pt.build_page_trust(key)
            except Exception as exc:  # noqa: BLE001 — one broken PDF must not stop the corpus
                print(f"  ! trust-Fehler {os.path.basename(key)[:50]}: {exc}")
                trust_cache[key] = None

    updates: list[tuple[str, dict]] = []
    dist: Counter[str] = Counter()
    label_heals = 0
    heals_by_doc: Counter[str] = Counter()
    reasons: Counter[str] = Counter()
    symptoms: list[str] = []
    for chunk_id, locator, path in rows:
        loc = locator if isinstance(locator, dict) else json.loads(locator or "{}")
        new_loc = dict(loc)
        base = os.path.basename(norm_path(path) or "")
        if loc.get("type") != "epub_cfi" and base in skip_books:
            continue  # held books: legacy locators stay untouched
        evidenced = base in heal_books
        if loc.get("type") == "epub_cfi":
            new_loc["page_source"] = pt.NONE
        elif loc.get("type") == "page_span":
            trust = trust_cache.get(norm_path(path) or "")
            phys = loc.get("physical_page_start")
            if trust and phys is not None:
                label_map, source_map = trust
                lvl = source_map.get(int(phys), pt.PHYSICAL_ONLY)
                if lvl == pt.FOLIO_VERIFIED and not evidenced:
                    # no counter-check evidence for this book: make no
                    # print-verified claim — leave legacy (renders sane)
                    continue
                new_loc["page_source"] = lvl
                if lvl == pt.FOLIO_VERIFIED:
                    # heal the labels to the printed folios
                    old = loc.get("page_label_start", "")
                    folio = label_map.get(int(phys), old)
                    if folio != old:
                        label_heals += 1
                        heals_by_doc[os.path.basename(norm_path(path) or "?")[:44]] += 1
                        if len(symptoms) < 12:
                            symptoms.append(f"{os.path.basename(norm_path(path) or '?')[:40]}: Label {old!r} -> Folio {folio!r}")
                    new_loc["page_label_start"] = str(folio)
                    pe = loc.get("physical_page_end")
                    if pe is not None:
                        new_loc["page_label_end"] = str(label_map.get(int(pe), folio))
            else:
                new_loc["page_source"] = pt.PHYSICAL_ONLY
        else:
            continue
        dist[new_loc["page_source"]] += 1
        if new_loc != loc and (
            not args.apply_epub_only
            or (new_loc.get("page_source") == pt.NONE and loc.get("type") == "epub_cfi")
        ):
            updates.append((chunk_id, new_loc))

    total = sum(dist.values())
    print("\nStufenverteilung (nach Re-Trust):")
    for lvl in (pt.FOLIO_VERIFIED, pt.PDF_LABEL_SANE, pt.PHYSICAL_ONLY, pt.NONE):
        n = dist.get(lvl, 0)
        print(f"  {lvl:16s} {n:6d}  ({(n / total * 100 if total else 0):5.1f} %)")
    print(f"\nLabel-Heilungen (Label != Folio): {label_heals}")
    for s in symptoms:
        print(f"  · {s}")
    print("\nHeilungen je Dokument (Top 10):")
    for doc, n in heals_by_doc.most_common(10):
        print(f"  {n:5d}  {doc}")
    print(f"zu aktualisierende Chunks: {len(updates)}")

    if not args.apply:
        print("DRY — nichts geschrieben (--apply schreibt DB + OS)")
        conn.rollback()
        return 0

    # DB: locator JSONB in place (locator-only — no rechunk, nothing else touched)
    psycopg2.extras.execute_batch(
        cur,
        "UPDATE processing_chunks SET locator = %s::jsonb WHERE id = %s::uuid",
        [(json.dumps(loc, ensure_ascii=False), cid) for cid, loc in updates],
        page_size=500,
    )
    conn.commit()
    print(f"DB aktualisiert: {len(updates)} Chunks")

    # OS: bulk partial updates of the locator field
    sess = requests.Session()
    done = 0
    os_exit_code = 0
    for i in range(0, len(updates), 500):
        batch = updates[i : i + 500]
        body = ""
        for cid, loc in batch:
            body += json.dumps({"update": {"_id": cid}}) + "\n"
            body += json.dumps({"doc": {"locator": loc}}) + "\n"
        r = sess.post(
            f"{OS_URL}/{OS_INDEX}/_bulk", data=body.encode("utf-8"),
            headers={"Content-Type": "application/x-ndjson"}, timeout=120,
        )
        r.raise_for_status()
        failed_ids = [cid for cid, it in zip((c for c, _ in batch), r.json().get("items", []))
                      if "error" in it.get("update", {})]
        if failed_ids:
            print(f"  OS-Batch {i}: {len(failed_ids)} fehlgeschlagen — ein Retry")
            locs = {c: l for c, l in batch}
            body = "".join(
                json.dumps({"update": {"_id": cid}}) + "\n" + json.dumps({"doc": {"locator": locs[cid]}}) + "\n"
                for cid in failed_ids
            )
            r2 = sess.post(f"{OS_URL}/{OS_INDEX}/_bulk", data=body.encode("utf-8"),
                           headers={"Content-Type": "application/x-ndjson"}, timeout=120)
            r2.raise_for_status()
            still = sum(1 for it in r2.json().get("items", []) if "error" in it.get("update", {}))
            done += len(failed_ids) - still
            if still:
                print(f"  OS-Batch {i}: {still} BLEIBEN fehlgeschlagen — DB ist vor OS; Konvergenz-Kontrolle nötig")
                os_exit_code = 1
        else:
            done += len(batch)
    print(f"OS aktualisiert: {done} Chunks")
    return os_exit_code


if __name__ == "__main__":
    raise SystemExit(main())
