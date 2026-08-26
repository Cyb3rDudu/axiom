"""#221 pilot — four-point alignment EPUB↔PDF (offline, read-only).

Produces, per run, into --out DIR:
  epub_markers.json     doc-pagebreak anchors -> (spine idx, href, CFI, marker no, text window)
  pdf_folios.json       physical -> printed folio map incl. gap classification + monotone runs
  alignment.csv         epub_cfi | epub_marker_page | pdf_physical | pdf_folio | source_flags
  divergences.json      classified divergences (feed for the #220 Stage-3 report format)

Verified interpreter: /opt/axiom/runner/current/env/bin/python (pymupdf).
Usage:
  four_point_pilot.py --epub <.epub> --pdf <.pdf> --out <dir>
"""
from __future__ import annotations

import argparse
import csv
import hashlib
import json
import re
import sys
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

NS = {
    "opf": "http://www.idpf.org/2007/opf",
    "dc": "http://purl.org/dc/elements/1.1/",
}
XHTML = "{http://www.w3.org/1999/xhtml}"
EPUB_TYPE = "{http://www.idpf.org/2007/ops}type"

# Apress/Springer credit boilerplate repeated on every chapter opener —
# pure noise for content anchors.
BOILERPLATE = """Jason Yip Nikhil Gupta Marcin Wojtyczka Redmond WA USA
Livingston NJ The Author(s) 2026 J. Yip et al.
https://doi.org/10.1007/979-8-8688-2524-8"""

_WINDOW = 120          # tokens harvested after each marker
_SIM_OK = 0.4          # containment threshold for a verified join
_HOP = 3               # physical-page radius for relocation search

_TAG = re.compile(rb"<[^>]+>")


def norm_tokens(s: str, n: int = 10_000) -> list[str]:
    s = s.replace("\u00a0", " ").replace("\u00ad", "")
    s = re.sub(r"[^A-Za-z0-9]+", " ", s)
    return [t.lower() for t in s.split()][:n]


def _roman(s: str) -> int:
    r = {"i": 1, "v": 5, "x": 10, "l": 50, "c": 100, "d": 500}
    vals = [r[c] for c in s]
    return sum(-a if a < b else a for a, b in zip(vals, vals[1:])) + vals[-1]


def containment(needles: list[str], page_tokens: list[str], stop: set[str]) -> float:
    """Fraction of marker-window tokens findable (substring, tag-join
    tolerant) in the PDF page text, boilerplate stripped."""
    toks = [t for t in needles if t not in stop]
    if not toks:
        return 0.0
    hay = " " + " ".join(t for t in page_tokens if t not in stop) + " "
    return sum(1 for t in toks if t in hay) / len(toks)


# ---------------------------------------------------------------- EPUB side

def harvest_epub(epub_path: Path, stop: set[str]) -> tuple[list[dict], dict]:
    with zipfile.ZipFile(epub_path) as z:
        opf_name = next(n for n in z.namelist() if n.endswith(".opf"))
        opf = ET.fromstring(z.read(opf_name))
        base = "/".join(opf_name.split("/")[:-1])
        manifest = {
            m.get("id"): (m.get("href"), m.get("media-type") or "")
            for m in opf.findall("opf:manifest/opf:item", NS)
        }
        spine_refs = [it.get("idref") for it in opf.findall("opf:spine/opf:itemref", NS)]

        markers: list[dict] = []
        for spine_idx, idref in enumerate(spine_refs):
            href, mtype = manifest.get(idref, (None, ""))
            if not href or "html" not in mtype:
                continue
            full = f"{base}/{href}" if base else href
            raw = z.read(full)
            stream = _TAG.sub(b"", raw).decode("utf-8", "replace")
            root = ET.fromstring(raw)
            for pb in root.iter(f"{XHTML}span"):
                if pb.get(EPUB_TYPE) != "pagebreak":
                    continue
                label = pb.get("aria-label") or ""
                if label.isdigit():
                    num = int(label)
                elif re.fullmatch(r"[ivxlcdm]+", label, re.I):
                    num = -_roman(label.lower())
                else:
                    continue
                pos = raw.find(f'id="{pb.get("id")}"'.encode())
                off = len(_TAG.sub(b"", raw[:max(pos, 0)]).decode("utf-8", "replace"))
                window = norm_tokens(stream[off:off + 900], _WINDOW)
                markers.append({
                    "marker_page": num,
                    "spine_idx": spine_idx,
                    "href": href,
                    "elem_id": pb.get("id") or "",
                    "cfi": f"epubcfi(/6/{2*(spine_idx+1)}!/4/2[{pb.get('id')}]/1:0)",
                    "window": window,
                    "para_hash": hashlib.sha1(" ".join(window[:25]).encode()).hexdigest()[:12],
                })
        meta = {
            "opf": opf_name,
            "spine_len": len(spine_refs),
            "title": opf.findtext("opf:metadata/dc:title", namespaces=NS) or "",
        }
    return markers, meta


# ----------------------------------------------------------------- PDF side

def map_pdf_folios(pdf_path: Path) -> list[dict]:
    import fitz  # pymupdf, runner dependency

    doc = fitz.open(pdf_path)
    pages = []
    for i, page in enumerate(doc):
        text = page.get_text("text")
        lines = [l.strip() for l in text.splitlines() if l.strip()]
        folio, cls = None, ("blank" if not lines else "unnumbered")
        if lines:
            m = re.fullmatch(r"(\d{1,3}|[ivxlcdm]{1,8})", lines[0], re.I)
            if m:
                folio = int(m.group(1)) if m.group(1).isdigit() else -_roman(m.group(1).lower())
                cls = "arabic" if m.group(1).isdigit() else "roman"
        pages.append({
            "physical": i + 1,
            "folio": folio,
            "class": cls,
            "tokens": norm_tokens(text, 900),
        })
    doc.close()
    return pages


def monotone_runs(pdf_pages: list[dict]) -> list[dict]:
    runs, cur = [], None
    for p in pdf_pages:
        if p["class"] != "arabic":
            continue
        if cur and p["folio"] == cur["last"] + 1:
            cur["last"] = p["folio"]
        else:
            if cur:
                runs.append(cur)
            cur = {"first": p["folio"], "last": p["folio"]}
    if cur:
        runs.append(cur)
    return runs


# ------------------------------------------------------------------ align

def align(markers: list[dict], pdf_pages: list[dict], stop: set[str]):
    folio_index = {p["folio"]: p for p in pdf_pages if p["folio"]}
    rows, divergences = [], []
    prev_phys = 0
    for m in sorted(markers, key=lambda x: (x["spine_idx"], x["marker_page"])):
        num, flags = m["marker_page"], []
        pdf_p = folio_index.get(num)
        if pdf_p:
            sim = containment(m["window"], pdf_p["tokens"], stop)
            if sim < _SIM_OK:
                cand = [q for q in pdf_pages
                        if abs(q["physical"] - pdf_p["physical"]) <= _HOP]
                best = max(cand, key=lambda q: containment(m["window"], q["tokens"], stop),
                           default=None)
                bsim = containment(m["window"], best["tokens"], stop) if best else 0.0
                if best is not pdf_p and bsim >= _SIM_OK:
                    # marker at printed page bottom: following text lives on
                    # the next folio — benign boundary, not a mismatch
                    benign = best["folio"] == num + 1
                    divergences.append({
                        "type": ("marker_boundary_next_page" if benign
                                 else "marker_folio_mismatch"),
                        "marker": num,
                        "labeled_phys": pdf_p["physical"],
                        "matched_phys": best["physical"], "matched_folio": best["folio"],
                        "sim_labeled": round(sim, 2), "sim_matched": round(bsim, 2)})
                    pdf_p, folio = best, best["folio"]
                    flags.append("marker_boundary_next_page" if benign
                                 else "folio_mismatch_relocated")
                else:
                    flags.append(f"low_text_sim:{sim:.2f}")
            else:
                folio = num
            if pdf_p["physical"] <= prev_phys:
                flags.append("non_monotonic")
            prev_phys = pdf_p["physical"]
        else:
            folio = None
            flags.append("no_pdf_folio")
            # opener candidates: pages physically between folio num-1 and num+1
            lo = folio_index.get(num - 1)
            hi = folio_index.get(num + 1)
            cand = [q for q in pdf_pages
                    if (not lo or q["physical"] >= lo["physical"])
                    and (not hi or q["physical"] <= hi["physical"])]
            best = max(cand, key=lambda q: containment(m["window"], q["tokens"], stop),
                       default=None)
            bsim = containment(m["window"], best["tokens"], stop) if best else 0.0
            if best and bsim >= _SIM_OK:
                pdf_p = best
                flags.append("opener_unnumbered_in_pdf")
            divergences.append({
                "type": "marker_without_folio", "marker": num,
                "located_phys": pdf_p["physical"] if pdf_p else None,
                "sim": round(bsim, 2)})
        rows.append({
            "epub_cfi": m["cfi"],
            "epub_marker_page": num,
            "epub_spine_idx": m["spine_idx"],
            "pdf_physical": pdf_p["physical"] if pdf_p else "",
            "pdf_folio": folio if folio is not None else "",
            "source_flags": ";".join(flags) if flags else "ok",
        })
    marker_nums = {m["marker_page"] for m in markers}
    for p in pdf_pages:
        if p["folio"] and p["folio"] > 0 and p["folio"] not in marker_nums:
            divergences.append({
                "type": "folio_without_marker", "folio": p["folio"],
                "physical": p["physical"], "class": p["class"]})
    return rows, divergences


# ------------------------------------------------------------------- main

def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--epub", required=True, type=Path)
    ap.add_argument("--pdf", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    stop = set(norm_tokens(BOILERPLATE))

    markers, meta = harvest_epub(args.epub, stop)
    nums = [m["marker_page"] for m in markers]
    mono = nums == sorted(nums) and len(set(nums)) == len(nums)
    print(f"epub: {len(markers)} markers, spine {meta['spine_len']}, "
          f"monotone+unique={mono}, range {min(nums)}..{max(nums)}")
    json.dump({"meta": {**meta, "monotone_unique": mono},
               "markers": [{k: v for k, v in m.items()} for m in markers]},
              open(args.out / "epub_markers.json", "w"), indent=1)

    pdf_pages = map_pdf_folios(args.pdf)
    runs = monotone_runs(pdf_pages)
    n_ar = sum(1 for p in pdf_pages if p["class"] == "arabic")
    print(f"pdf: {len(pdf_pages)} pages, {n_ar} arabic folios, {len(runs)} runs")
    json.dump({"runs": runs,
               "pages": [{k: v for k, v in p.items() if k != "tokens"}
                         for p in pdf_pages]},
              open(args.out / "pdf_folios.json", "w"), indent=1)

    rows, div = align(markers, pdf_pages, stop)
    with open(args.out / "alignment.csv", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
        w.writeheader()
        w.writerows(rows)
    ok = sum(1 for r in rows if r["source_flags"] == "ok")
    from collections import Counter
    kinds = Counter(d["type"] for d in div)
    print(f"align: {ok}/{len(rows)} verified joins; divergences: {dict(kinds)}")
    json.dump(div, open(args.out / "divergences.json", "w"), indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
