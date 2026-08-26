"""#222 twins sweep — run the four-point alignment over ALL Zotero EPUB+PDF
pairs, and derive+inject page-lists for unanchored EPUBs.

What it does:
  enumerate   query the Zotero local API for documents with >=1 epub and >=1
              pdf attachment; resolve storage paths via the /file redirect.
  sweep       per pair: harvest (dialect-aware) + PDF folio map + align.
              Anchored pairs -> divergence report; unanchored pairs ->
              derive the page map from the PDF sibling via content anchors
              and INJECT it (inline pagebreak spans + Adobe page-map.xml)
              into a WORKING COPY — originals are never modified
              (injection pattern proven in ProQuest crawl, 648/648).
  verify      re-harvest each injected copy and join against the PDF folios:
              the derivation is only accepted if the round-trip verifies.

Usage (needs the runner env for pymupdf):
  /opt/axiom/runner/current/env/bin/python twins_sweep.py \
      --zotero http://localhost:23119 --out /tmp/sweep222 [--inject-dir DIR]
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import re
import sys
import urllib.request
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from four_point_pilot import (  # noqa: E402
    _norm_path,
    BOILERPLATE, align, harvest_epub, map_pdf_folios, monotone_runs,
    norm_tokens,
)

STORAGE = Path.home() / "Zotero" / "storage"
_WORD = re.compile(r"[\w]{2,}", re.UNICODE)


# ------------------------------------------------------------ enumeration

def enumerate_twins(zotero: str) -> list[dict]:
    """Documents with >=1 epub and >=1 pdf attachment."""
    import urllib.error

    atts, start = [], 0
    while True:
        url = (f"{zotero}/api/users/0/items?itemType=attachment"
               f"&format=json&limit=100&start={start}")
        batch = json.load(urllib.request.urlopen(url, timeout=10))
        if not batch:
            break
        atts += batch
        start += 100
    byp: dict[str, list] = {}
    for a in atts:
        byp.setdefault(a["data"].get("parentItem"), []).append(a["data"])
    twins = []
    for parent, ds in byp.items():
        if parent is None:
            continue
        ep = next((d for d in ds if (d.get("filename") or "").lower().endswith(".epub")), None)
        pf = next((d for d in ds if (d.get("filename") or "").lower().endswith(".pdf")), None)
        if not (ep and pf):
            continue
        twins.append({
            "parent": parent,
            "title": ep["filename"].rsplit(".epub", 1)[0],
            "epub": _storage_file(ep["key"], ".epub"),
            "pdf": _storage_file(pf["key"], ".pdf"),
        })
    return twins


def _storage_file(key: str, ext: str) -> str:
    hits = glob.glob(str(STORAGE / key / f"*{ext}"))
    if not hits:
        raise FileNotFoundError(f"{STORAGE}/{key}/*{ext}")
    return hits[0]


# ------------------------------------------------------- derive + inject

MARKER = ('<span xmlns:epub="http://www.idpf.org/2007/ops" '
          'epub:type="pagebreak" id="PB{n}" role="doc-pagebreak" '
          'aria-label="{n}" title="{n}"/>')


def epub_token_stream(epub: Path) -> tuple[list, list[str], dict]:
    """Global token stream in spine order: [(token, file_idx, raw_pos)].
    Returns (stream, spine_files, opf_info)."""
    with zipfile.ZipFile(epub) as z:
        from xml.etree import ElementTree as ET
        NS = {"opf": "http://www.idpf.org/2007/opf"}
        opf_name = next(n for n in z.namelist() if n.endswith(".opf"))
        opf = ET.fromstring(z.read(opf_name))
        base = "/".join(opf_name.split("/")[:-1])
        man = {m.get("id"): (m.get("href"), m.get("media-type") or "")
               for m in opf.findall("opf:manifest/opf:item", NS)}
        spine = [it.get("idref") for it in opf.findall("opf:spine/opf:itemref", NS)]
        files, streams, raws = [], [], []
        for idref in spine:
            href, mtype = man.get(idref, (None, ""))
            if not href or "html" not in mtype:
                continue
            full = _norm_path(base, href)
            files.append(full)
            raws.append(z.read(full))
            streams.append([])
        tag = re.compile(rb"<[^>]+>")
        for fi, raw in enumerate(raws):
            # text runs between tags, with raw offsets
            pos = 0
            for m in tag.finditer(raw):
                seg = raw[pos:m.start()]
                if seg:
                    _emit(seg, pos, fi, streams[fi])
                pos = m.end()
            if pos < len(raw):
                _emit(raw[pos:], pos, fi, streams[fi])
        flat = [t for s in streams for t in s]
        return flat, files, {"opf_name": opf_name, "base": base}


def _emit(seg: bytes, base_pos: int, fi: int, out: list):
    try:
        text = seg.decode("utf-8")
    except UnicodeDecodeError:
        text = seg.decode("utf-8", "replace")
    from four_point_pilot import _fold
    for m in _WORD.finditer(text):
        tok = re.sub(r"[^A-Za-z0-9]", "", _fold(m.group(0))).lower()
        if tok:
            out.append((tok, fi, base_pos + m.start()))


def anchor_pdf_pages(pdf_pages: list[dict], stream: list) -> list[dict]:
    """For each arabic-folio PDF page, locate the EPUB stream position where
    that page begins. Tolerant matching: PDF text layers often glue words
    ("ÜberdieAutoren"), so EPUB token w matches PDF token t iff w==t,
    t.startswith(w) or w in t (>=5 chars). Index = 4-char prefixes."""
    from collections import defaultdict
    index: dict[str, list[int]] = defaultdict(list)
    for i, (tok, _, _) in enumerate(stream):
        if len(tok) >= 4:
            index[tok[:4]].append(i)

    def distinctive(tokens, page_freq):
        sel = [t for t in tokens
               if len(t) >= 5 and page_freq.get(t, 0) <= 0.3][:12]
        return sel or [t for t in tokens if len(t) >= 4][:6]

    def tok_match(w, t):
        return w == t or (len(w) >= 5 and (t.startswith(w) or w in t))

    def find(toks, cursor):
        for start in range(len(toks)):
            probe = toks[start]
            for p in index.get(probe[:4], []):
                if p < cursor:
                    continue
                # verify toks[start:] as tolerant subsequence from p;
                # return the position of the FIRST matched token
                j, first, matched, end = start, None, 0, min(p + 100, len(stream))
                for i in range(p, end):
                    if j < len(toks) and tok_match(stream[i][0], toks[j]):
                        if first is None:
                            first = i
                        matched += 1
                        j += 1
                if matched >= max(3, int(0.7 * (len(toks) - start))):
                    return first if first is not None else p
        return None

    # running-head/footer boilerplate = tokens on >30% of pages
    page_freq: dict[str, int] = {}
    body_pages = [p for p in pdf_pages if p["class"] == "arabic"]
    for p in body_pages:
        for t in set(p["tokens"]):
            page_freq[t] = page_freq.get(t, 0) + 1
    freq = {t: c / len(body_pages) for t, c in page_freq.items()} \
        if body_pages else {}

    results = []
    for p in pdf_pages:
        if p["class"] != "arabic":
            continue
        toks = distinctive(p["tokens"], freq)
        pos = find(toks, 0) if toks else None
        results.append({"folio": p["folio"], "physical": p["physical"],
                        "stream_idx": pos, "matched": pos is not None})
    # monotonicity by longest increasing subsequence: a wrong early anchor
    # must not cascade (cursor approach failed exactly there)
    _lis_filter(results)
    return results


def _lis_filter(results):
    """Keep the longest increasing run of stream_idx; flag the rest unmatched."""
    import bisect
    vals, idx = [], []
    for r in results:
        if r["stream_idx"] is None:
            continue
        vals.append(r["stream_idx"])
        idx.append(r)
    tails, tails_idx, prev = [], [], []
    for i, v in enumerate(vals):
        j = bisect.bisect_left(tails, v)
        if j == len(tails):
            tails.append(v); tails_idx.append(i)
        else:
            tails[j] = v; tails_idx[j] = i
        prev.append(tails_idx[j-1] if j > 0 else -1)
    # reconstruct
    keep = set()
    k = tails_idx[-1] if tails_idx else -1
    while k >= 0:
        keep.add(k); k = prev[k]
    ki = iter(range(len(vals)))
    for r in idx:
        i = next(ki)
        if i not in keep:
            r["stream_idx"] = None
            r["matched"] = False


def inject_pagelist(epub: Path, out_path: Path, anchors: list[dict]) -> dict:
    """Write a working copy with inline pagebreak markers + page-map.xml.
    Original untouched. anchors: [{folio, stream_idx}] against the same
    stream built by epub_token_stream."""
    stream, files, opf_info = epub_token_stream(epub)
    with zipfile.ZipFile(epub) as z:
        contents = {n: z.read(n) for n in z.namelist() if n != "mimetype"}
        names = z.namelist()

    by_file: dict[int, list[int]] = {}
    for a in anchors:
        if a["stream_idx"] is None:
            continue
        tok, fi, raw_pos = stream[a["stream_idx"]]
        by_file.setdefault(fi, []).append((raw_pos, a["folio"]))

    n_injected = 0
    for fi, plist in by_file.items():
        name = files[fi]
        raw = contents[name]
        # insert markers right after the closest preceding '>'
        chunks, last = [], 0
        for raw_pos, folio in plist:
            at = raw.rfind(b">", 0, raw_pos) + 1
            if at <= 0:
                continue
            chunks.append(raw[last:at])
            chunks.append(MARKER.format(n=folio).encode())
            last = at
            n_injected += 1
        chunks.append(raw[last:])
        contents[name] = b"".join(chunks)

    # page-map.xml (href relative to OPF dir) + OPF wiring
    base = opf_info["base"]
    pm = ['<?xml version="1.0" encoding="UTF-8"?>',
          '<page-map xmlns="http://www.idpf.org/2007/ops" name="print">', ""]
    import posixpath
    for a in anchors:
        if a["stream_idx"] is None:
            continue
        fi = stream[a["stream_idx"]][1]
        rel = posixpath.relpath(files[fi], base) if base else files[fi]
        pm.append(f'  <page name="{a["folio"]}" href="{rel}#PB{a["folio"]}"/>')
    pm += ["", "</page-map>"]
    pm_name = posixpath.join(base, "page-map.xml") if base else "page-map.xml"
    contents[pm_name] = "\n".join(pm).encode()

    opf_raw = contents[opf_info["opf_name"]].decode("utf-8")
    rel_pm = posixpath.relpath(pm_name, base) if base else "page-map.xml"
    if 'name="page-map"' not in opf_raw:
        opf_raw = opf_raw.replace(
            "</metadata>",
            f'<meta name="page-map" content="{rel_pm}"/></metadata>', 1)
    if 'id="pagemap"' not in opf_raw:
        opf_raw = opf_raw.replace(
            "</manifest>",
            f'<item id="pagemap" href="{rel_pm}" '
            f'media-type="application/oebps-page-map+xml"/></manifest>', 1)
    contents[opf_info["opf_name"]] = opf_raw.encode()

    with zipfile.ZipFile(out_path, "w") as z:
        zi = zipfile.ZipInfo("mimetype")
        z.writestr(zi, "application/epub+zip", compress_type=zipfile.ZIP_STORED)
        for n in names:
            if n == "mimetype":
                continue
            z.writestr(n, contents[n], compress_type=zipfile.ZIP_DEFLATED)
    return {"injected": n_injected, "anchors": len(anchors),
            "page_map": pm_name}


# ------------------------------------------------------------------ main

def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--zotero", default="http://localhost:23119")
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--inject-dir", type=Path,
                    help="write working copies of unanchored EPUBs here")
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    stop = set(norm_tokens(BOILERPLATE))

    twins = enumerate_twins(args.zotero)
    print(f"twins: {len(twins)}")
    report = []
    for t in twins:
        slug = re.sub(r"\W+", "_", t["title"])[:40]
        tdir = args.out / slug
        tdir.mkdir(exist_ok=True)
        try:
            markers, meta = harvest_epub(Path(t["epub"]), stop)
            pdf_pages = map_pdf_folios(Path(t["pdf"]))
        except Exception as e:
            report.append({"title": t["title"], "error": str(e)})
            print(f"  {slug}: ERROR {e}")
            continue
        n_ar = sum(1 for p in pdf_pages if p["class"] == "arabic")
        entry = {"title": t["title"], "dialects": meta.get("dialects"),
                 "markers": len(markers), "pdf_pages": len(pdf_pages),
                 "pdf_arabic_folios": n_ar}
        if markers and n_ar:
            rows, div = align(markers, pdf_pages, stop)
            ok = sum(1 for r in rows if r["source_flags"] == "ok")
            entry.update({"verified_joins": f"{ok}/{len(rows)}",
                          "divergence_types": _kinds(div)})
            json.dump(rows, open(tdir / "alignment.json", "w"), indent=1)
            json.dump(div, open(tdir / "divergences.json", "w"), indent=1)
        elif markers:
            entry["note"] = ("anchored, but PDF sibling has no printed "
                             "folios (e-book PDF) — folio join impossible; "
                             "marker range "
                             f"{min(m['marker_page'] for m in markers)}.."
                             f"{max(m['marker_page'] for m in markers)}")
        else:
            entry["derived"] = _derive(t, pdf_pages, tdir,
                                       args.inject_dir, stop)
        report.append(entry)
        print(f"  {slug}: markers={len(markers)} folios={n_ar} "
              f"joins={entry.get('verified_joins', entry.get('note', 'derived'))}")
    json.dump(report, open(args.out / "sweep_report.json", "w"), indent=1,
              ensure_ascii=False)
    print(f"report: {args.out / 'sweep_report.json'}")
    return 0


def _kinds(div):
    from collections import Counter
    return dict(Counter(d["type"] for d in div))


def _derive(t, pdf_pages, tdir, inject_dir, stop):
    """Derive page map from the PDF sibling and inject into a working copy."""
    stream, files, _ = epub_token_stream(Path(t["epub"]))
    anchors = anchor_pdf_pages(pdf_pages, stream)
    good = [a for a in anchors if a["matched"]]
    out = {"anchorable_folios": f"{len(good)}/{len(anchors)}"}
    if not inject_dir or len(good) < 10:
        return out
    inject_dir.mkdir(parents=True, exist_ok=True)
    dst = inject_dir / (Path(t["epub"]).stem + ".injected.epub")
    inj = inject_pagelist(Path(t["epub"]), dst, good)
    out["injected"] = inj
    # round-trip verification: re-harvest + join against the PDF
    markers, meta = harvest_epub(dst, stop)
    rows, div = align(markers, pdf_pages, stop)
    ok = sum(1 for r in rows if r["source_flags"] == "ok")
    out["verify"] = {"markers_found": len(markers),
                     "verified_joins": f"{ok}/{len(rows)}"}
    return out


if __name__ == "__main__":
    sys.exit(main())
