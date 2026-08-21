#!/usr/bin/env python
"""#195 — corpus-wide three-way citation-integrity probe.

Per Zotero attachment: pick 3 deterministic anchors (front/middle/back,
distinctive unique sentences), create a highlight annotation + a linked
template-derived note (blueprint VBFLCQLA, surgical slot replacement),
then compare three readings:

    annotation pageLabel == chunk exact page (paragraph_pages
    via /api/passage/{id}/page?at=N) == PDF embedded label

Measurement only — no heals, no re-chunks, no deletions. Everything tagged
`integritäts-check`.

Usage:
    python integrity_probe.py --list                 # corpus overview
    python integrity_probe.py --dry KEY...           # anchors + 3-way, no writes
    python integrity_probe.py --write KEY            # create artifacts + 3-way
    python integrity_probe.py --report [FILE.json]   # summarize results JSON

Conventions: Zotero writes ONLY via local API (write key +
Zotero-Server-ID headers). Run with axiom_ng_runner/.venv/bin/python
(pymupdf + requests).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

import pymupdf

ZOTERO = "http://localhost:23119"
RAG = "http://localhost:8011"
TAG = "integritäts-check"
STORAGE = Path.home() / "Zotero" / "storage"
URI_PREFIX = "http://zotero.org/users/local/xMzoeMYN/items/"
BLUEPRINT_KEY = "VBFLCQLA"
RESULTS = Path(os.environ.get("INTRESULTS", "/tmp/integrity_probe_results.json"))

MIN_WORDS = 8
MAX_WORDS = 45
MIN_PAGE_CHARS = 300  # a "body page" for anchor thirds
PROSE_LINE_FRAC = 0.35  # median line width vs page width for a prose page
ABBREV_END = re.compile(r"(?:^|\s)[a-zA-ZäöüÄÖÜ]{1,3}\.$")  # "z.", "B.", "Nr."


# ---------------------------------------------------------------- helpers ---

def zotero_headers():
    key = Path.home().joinpath(".axiom-ng/write-api-key").read_text().strip()
    req = urllib.request.Request(ZOTERO + "/api/")
    with urllib.request.urlopen(req, timeout=10) as r:
        sid = r.headers["Zotero-Server-ID"]
    return {"Zotero-API-Key": key, "Zotero-Server-ID": sid,
            "Content-Type": "application/json"}


def zotero_get(path, headers):
    req = urllib.request.Request(ZOTERO + path, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())


def zotero_post(path, payload, headers):
    body = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(ZOTERO + path, data=body, headers=headers,
                                 method="POST")
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())


def rag_search(query, doc_id=None, top_n=5):
    body = {"query": query, "top_n": top_n}
    if doc_id:
        body["filters"] = {"document_ids": [doc_id]}
    req = urllib.request.Request(RAG + "/api/search",
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"},
                                 method="POST")
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())


def rag_passage_page(chunk_id, at):
    url = f"{RAG}/api/passage/{chunk_id}/page?at={at}"
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.loads(r.read().decode())


def norm(s: str) -> str:
    """Whitespace-normalized comparison form (single spaces)."""
    return re.sub(r"\s+", " ", s).strip()


def doc_map():
    req = urllib.request.Request(RAG + "/api/zotero/documents")
    with urllib.request.urlopen(req, timeout=60) as r:
        docs = json.loads(r.read().decode())["documents"]
    return {d["attachment"]["zotero_key"]: {
        "doc_id": d["document_id"],
        "item_key": d["zotero_key"],
        "title": d["title"],
        "filename": d["attachment"].get("filename", ""),
    } for d in docs if d.get("attachment")}


# ------------------------------------------------------- PDF text + lines ---

class PageText:
    """Reconstructed page text with a char-range -> line-rect map.

    Lines are kept in pymupdf's natural block/line order — the content
    stream order is the semantic reading order even on pages whose layout
    transform flips content vertically (LWY53EWV case: y-sorting scrambles
    the paragraph; content order reads correctly). Hard hyphenation
    ("word-" at EOL) is joined; soft hyphens (\xad) are removed. Corpus
    locatability gates the final anchor selection.
    """

    def __init__(self, page: pymupdf.Page):
        self.H = page.rect.height
        self.W = page.rect.width
        self.label = page.get_label()
        d = page.get_text("dict")
        ordered = []
        for block in d["blocks"]:
            if block.get("type") != 0:
                continue
            for line in block["lines"]:
                txt = "".join(s["text"] for s in line["spans"])
                txt = txt.replace("\u00ad", "")  # soft hyphen: invisible
                txt = txt.rstrip()
                if not txt.strip():
                    continue
                x0, y0, x1, y1 = line["bbox"]
                # pymupdf bottom-up -> Zotero/pdf.js top-down (proven)
                zrect = (x0, self.H - y1, x1, self.H - y0)
                ordered.append((txt, zrect))
        # join lines with dehyphenation, tracking per-line offsets
        out = []
        self.line_offsets = []
        pos = 0
        prev_endhyph = False
        for txt, zrect in ordered:
            t = txt
            if out:
                if prev_endhyph and t:
                    pass  # dehyphenate: join directly
                else:
                    out.append(" ")
                    pos += 1
            self.line_offsets.append((pos, pos + len(t), zrect))
            out.append(t)
            pos += len(t)
            prev_endhyph = t.endswith("-")
        self.text = "".join(out)

    def rects_for_range(self, start: int, end: int):
        """Zotero rects for the lines overlapping [start, end)."""
        rs = []
        for s, e, zrect in self.line_offsets:
            if e > start and s < end:
                rs.append(zrect)
        return rs

    def norm_index(self):
        """(normalized_text, index list) mapping norm positions -> raw."""
        if getattr(self, "_norm", None) is None:
            out, idx = [], []
            for i, ch in enumerate(self.text):
                if ch.isspace():
                    if out and out[-1] != " ":
                        out.append(" ")
                        idx.append(i)
                else:
                    out.append(ch)
                    idx.append(i)
            self._norm = ("".join(out), idx)
        return self._norm

    def rects_for_norm_range(self, a: int, b: int):
        """Rects for a span given in normalized-text offsets."""
        ns, idx = self.norm_index()
        if b < len(ns):
            b2 = idx[b]
        else:
            b2 = len(self.text)
        a2 = idx[a] if a < len(idx) else len(self.text)
        return self.rects_for_range(a2, b2)


def sentence_candidates(text: str):
    """Sentence spans on . ! ? — terminator must be followed by
    space+uppercase or end (skips abbreviation splits like „z. B.")."""
    for m in re.finditer(r"[^.!?…]{25,600}?[.!?…](?=\s|$)", text):
        rest = text[m.end():].lstrip()
        if rest and not re.match(r"[\u201e\u201c\u201d\)\u2016\[A-ZÄÖÜ0-9\*]", rest):
            continue  # continues lowercase -> was an abbreviation split
        s = m.group(0)
        yield m.start(), m.end(), s


def word_count(s: str) -> int:
    return len(s.split())


# ------------------------------------------------------- anchor selection ---

def is_prose_page(pt: PageText) -> bool:
    """Diagram pages: many short scattered lines. Prose: long body lines."""
    if not pt.line_offsets:
        return False
    widths = sorted(z[2][2] - z[2][0] for z in pt.line_offsets)
    median = widths[len(widths) // 2]
    return median >= PROSE_LINE_FRAC * pt.W


def prose_like_rects(rects) -> bool:
    """Prose sentences flow down one column: most rects share x0 (±15pt).
    Diagram-label quotes scatter across x."""
    if not rects:
        return False
    xs = sorted(r[0] for r in rects)
    mode_x = xs[len(xs) // 2]
    consistent = sum(1 for x in xs if abs(x - mode_x) <= 15)
    return consistent / len(xs) >= 0.6


def strip_running_prefix(s: str, label):
    """Strip leading running-head junk from a candidate sentence:
    all-caps words, page-label tokens, separators, stray drop-cap letters.
    Returns (stripped, n_stripped_chars)."""
    lab = re.escape(str(label))
    tok = re.compile(
        rf"^(?:[A-ZÄÖÜ][A-ZÄÖÜ0-9\-/&.]{{7,}}\s*"      # running-head caps word
        rf"|[A-ZÄÖÜ]\s+"                              # drop-cap letter
        rf"|{lab}[.\)]?\s*"                           # page-label token
        rf"|[|·•\u2013\u2014-]\s*"                     # separators
        rf")+")
    n = 0
    for _ in range(6):
        m = tok.match(s)
        if not m:
            break
        # never strip everything
        rest = s[m.end():]
        if len(rest) < 40:
            break
        n += m.end()
        s = rest
    return s, n


def pick_anchor_candidates(pdf_path: Path):
    """Deterministic ranked anchor candidates per position (front/middle/
    back). v2: two-stage selection — strict prose filters first; if a
    position stays empty, RELAXED pass (table-heavy docs: WEO/IMF etc.).
    Returns ({position: [candidate, ...]}, err)."""
    doc = pymupdf.open(pdf_path)
    pages = [PageText(doc[i]) for i in range(len(doc))]
    doc.close()

    body = [i for i, pt in enumerate(pages)
            if len(norm(pt.text)) >= MIN_PAGE_CHARS and is_prose_page(pt)]
    if len(body) < 4:
        return None, "too few prose body pages"
    thirds = [body[0: max(1, len(body) // 3)],
              body[len(body) // 3: 2 * len(body) // 3],
              body[2 * len(body) // 3:]]

    # corpus of full text for uniqueness (+ precomputed per-page norms:
    # the running-head killer consults every page per candidate)
    norm_pages = [norm(pt.text) for pt in pages]
    full = "\n".join(norm_pages)

    out = {}
    used_pages = set()
    for pos_name, idxs in zip(("front", "middle", "back"), thirds):
        scan = list(idxs) + [i for i in body if i not in idxs]  # fallback: whole body
        cands = []
        for i in scan:
            if i in used_pages:
                continue
            pt = pages[i]
            ns, _ = pt.norm_index()
            page_cands = []
            for a, b, s in sentence_candidates(ns):
                # strip running heads / page numbers from the start
                s2, n = strip_running_prefix(s, pt.label)
                a += n
                s = s2
                w = word_count(s)
                if not (MIN_WORDS <= w <= MAX_WORDS):
                    continue
                if not re.match(r"^[A-ZÄÖÜ0-9]", s):
                    continue  # fragment start (column/quote continuation)
                if re.search(r"\d{3,}", s):
                    continue
                # boilerplate: license/CC/DOI/URL sentences are not anchors
                if re.search(r"creativecommons|unrestricted reuse|distribution, and reproduction|doi\.org|https?://|copyright|open access|alle rechte|verlag|impressum|isbn|auflage|gedruckt auf|herstellung", s, re.I):
                    continue
                if ABBREV_END.search(s):
                    continue
                if max((len(x) for x in s.split()), default=0) < 8:
                    continue
                if full.count(s) != 1:
                    continue
                # running-head killer: the mid-sentence fragment must appear
                # on exactly ONE pdf page (heads repeat on every other page)
                fwords = s.split()
                if len(fwords) >= 4:
                    fprobes = (" ".join(fwords[1:5]), " ".join(fwords[2:6]))
                    pg_hits = 0
                    for ppt in norm_pages:
                        for fp in fprobes:
                            if fp in ppt:
                                pg_hits += 1
                                break
                    if pg_hits > 1:
                        continue  # must be unique within the PDF
                rects = pt.rects_for_norm_range(a, b)
                if not prose_like_rects(rects):
                    continue
                page_cands.append({
                    "page_index": i,
                    "pdf_label": pt.label,
                    "quote": s,
                    "rects": rects,
                    "words": w,
                })
            # deterministic rank within page: longest first
            page_cands.sort(key=lambda c: -c["words"])
            cands.extend(page_cands)
        if not cands:
            # v2 RELAXED pass: table-heavy docs — drop prose-only filters,
            # keep uniqueness + boilerplate (safety net stays)
            for i in scan:
                if i in used_pages:
                    continue
                pt = pages[i]
                ns, _ = pt.norm_index()
                for a, b, s in sentence_candidates(ns):
                    s2, n = strip_running_prefix(s, pt.label)
                    a += n
                    s = s2
                    w = word_count(s)
                    if not (6 <= w <= MAX_WORDS):
                        continue
                    if re.search(r"creativecommons|unrestricted reuse|distribution, and reproduction|doi\.org|https?://|copyright|open access|alle rechte|verlag|impressum|isbn|auflage|gedruckt auf|herstellung", s, re.I):
                        continue
                    if ABBREV_END.search(s):
                        continue
                    if full.count(s) != 1:
                        continue
                    fwords = s.split()
                    if len(fwords) >= 4:
                        fprobes = (" ".join(fwords[1:5]), " ".join(fwords[2:6]))
                        pg_hits = 0
                        for ppt in norm_pages:
                            for fp in fprobes:
                                if fp in ppt:
                                    pg_hits += 1
                                    break
                        if pg_hits > 1:
                            continue
                    rects = pt.rects_for_norm_range(a, b)
                    if not rects:
                        continue
                    cands.append({
                        "page_index": i,
                        "pdf_label": pt.label,
                        "quote": s,
                        "rects": rects,
                        "words": w,
                        "relaxed": True,
                    })
            cands.sort(key=lambda c: -c["words"])
        out[pos_name] = cands
        if cands:
            used_pages.add(cands[0]["page_index"])
    return out, None


# (rects_for_range_raw kept for compatibility — same as rects_for_range)
def _rects_for_range_raw(self, a, b):
    return self.rects_for_range(a, b)
PageText.rects_for_range_raw = _rects_for_range_raw


def fmt_rects(rects):
    return [[round(v, 2) for v in r] for r in rects]


# ---------------------------------------------------- Zotero artifact side ---

def existing_probe_items(headers):
    """All items tagged integritäts-check (for duplicate protection)."""
    q = urllib.parse.quote(TAG)
    items = zotero_get(f"/api/users/0/items?tag={q}&limit=100&format=json", headers)
    return items


def fingerprint_att(parent, label, quote):
    return (parent, label, norm(quote)[:80])


def fingerprint_note(parent, quote):
    return (parent, norm(quote)[:80])


def word_precise_rects(pdf_path, page_index, quote, fallback):
    """Word-precise highlight rects: bbox of the quote's words only."""
    try:
        doc = pymupdf.open(pdf_path)
        page = doc[page_index]
        H = page.rect.height
        import unicodedata as _u
        def nuk(s):
            s = _u.normalize("NFKC", s).replace("ß", "ss").replace("ﬁ", "fi").replace("ﬂ", "fl")
            return "".join(ch.lower() for ch in s if ch.isalnum())
        qw = [nuk(w) for w in quote.split()][:14]
        words = page.get_text("words")  # x0,y0,x1,y1,word,block,line,no
        out = []
        qi = 0
        for w in words:
            if qi >= len(qw):
                break
            if nuk(w[4]) == qw[qi] or nuk(w[4]).startswith(qw[qi]) or qw[qi].startswith(nuk(w[4])[:6]):
                out.append((w[0], w[1], w[2], w[3]))
                qi += 1
        doc.close()
        if qi >= max(4, len(qw) // 2):
            # merge per line (y-mid cluster), zotero coords
            lines = {}
            for x0, y0, x1, y1 in out:
                key = round((y0 + y1) / 2 / 5)
                if key not in lines:
                    lines[key] = [x0, y0, x1, y1]
                else:
                    L = lines[key]
                    L[0] = min(L[0], x0); L[1] = min(L[1], y0)
                    L[2] = max(L[2], x1); L[3] = max(L[3], y1)
            return [[round(x0, 2), round(H - y1, 2), round(x1, 2), round(H - y0, 2)]
                    for x0, y0, x1, y1 in sorted(lines.values(), key=lambda L: -L[1])]
    except Exception:
        pass
    return fallback


def make_annotation_payload(att_key, anchor):
    rects = fmt_rects(anchor["rects"])
    pos = json.dumps({"pageIndex": anchor["page_index"], "rects": rects},
                     separators=(",", ":"), ensure_ascii=False)
    first = rects[0]
    sort = "%05d|%06d|%05d" % (anchor["page_index"],
                               int(round(first[3] * 10)),
                               int(first[0]))
    return {
        "itemType": "annotation",
        "parentItem": att_key,
        "annotationType": "highlight",
        "annotationText": anchor["quote"],
        "annotationComment": "",
        "annotationColor": "#ffd400",
        "annotationPageLabel": str(anchor.get("N", anchor.get("pdf_label") or "")),
        "annotationSortIndex": sort,
        "annotationPosition": pos,
        "tags": [{"tag": TAG}],
        "relations": {},
    }


def csl_item(headers, item_key):
    arr = zotero_get(f"/api/users/0/items/{item_key}?format=csljson", headers)
    return arr[0]


def citation_display(csl, label):
    """(Author/Institution, Year, S. label) — APA-ish, German 'S.'."""
    authors = csl.get("author") or []
    if not authors:
        name = csl.get("publisher") or csl.get("container-title") or "o. A."
        a = name
    else:
        parts = []
        for au in authors[:2]:
            parts.append(au.get("family") or au.get("literal") or "")
        a = " and ".join(p for p in parts if p) if parts else "o. A."
        if len(authors) > 2:
            a = parts[0] + " et al."
    year = ""
    issued = csl.get("issued", {}).get("date-parts", [[None]])
    if issued and issued[0] and issued[0][0]:
        year = str(issued[0][0])
    if not label:
        return f"({a}, {year})" if year else f"({a})"
    return f"({a}, {year}, S. {label})" if year else f"({a}, S. {label})"


def enc(obj):
    return urllib.parse.quote(json.dumps(obj, separators=(",", ":"),
                                         ensure_ascii=True), safe="")


def short_title(title):
    t = re.sub(r"[–—-].*$", "", title)
    words = t.split()
    return " ".join(words[:4])


def make_note_payload(book_key, att_key, anchor, csl, blueprint_html):
    uri_book = URI_PREFIX + book_key
    uri_att = URI_PREFIX + att_key
    # v2 two-value design: N = print page (embedded PDF label, may be empty),
    # M = chunk exact page (paragraph_pages). Both visible side by side.
    label = str(anchor.get("N", ""))          # N — goes into the citation locator
    m_val = str(anchor.get("M", (anchor.get("chunk") or {}).get("page") or ""))
    rects = fmt_rects(anchor["rects"])
    pos = json.dumps({"pageIndex": anchor["page_index"], "rects": rects},
                     separators=(",", ":"), ensure_ascii=False)

    data_citation_items = enc([{"uris": [uri_book], "itemData": csl}])
    data_annotation = enc({
        "attachmentURI": uri_att,
        "pageLabel": label,
        "position": {"pageIndex": anchor["page_index"], "rects": rects},
        "citationItem": {"uris": [uri_book], "locator": label},
    })
    data_citation = enc({
        "citationItems": [{"uris": [uri_book], "locator": label}],
        "properties": {},
    })

    n_disp = label if label else "—"
    h1 = f"Integritätscheck {short_title(csl.get('title', ''))}: Anker {anchor['position'].upper()} — Zitat S. {n_disp} / Chunk S. {m_val}"
    einsatz = (f"Dreiecksprobe #195, Anker {anchor['position']}: Zitat-Seite {n_disp} ↔ Chunk-Seite {m_val}"
               + (" — MATCH" if (label and label == m_val) else " — ABWEICHUNG"))
    werte = (f"Zitat-Seite: {n_disp}" + ("" if label else " (kein eingebettetes Label)")
             + f" (= Locator in data-citation, Druckseite). "
             f"Chunk-Seite: {m_val} (= paragraph_pages-Exaktseite).")
    warum = ("Messung der Zitatkette über alle drei Beine: gelbe Annotation im "
             "Reader, paragraph_pages-Exaktseite des Chunks, eingebettetes "
             "PDF-Label. Kein Heilen, keine Re-Chunks — Befundlage für #195.")
    quote_span = f"„{anchor['quote']}“"
    cite_disp = citation_display(csl, label) if label else citation_display(csl, None)

    note = blueprint_html
    note = re.sub(r'data-citation-items="[^"]*"', f'data-citation-items="{data_citation_items}"', note, count=1)
    note = re.sub(r"<h1>.*?</h1>", lambda m: "<h1>" + _esc(h1) + "</h1>", note, count=1, flags=re.S)
    note = re.sub(r'(?s)<p><strong>Einsatz:</strong>.*?</p>', lambda m: "<p><strong>Einsatz:</strong> " + _esc(einsatz) + "</p>\n<p>" + _esc(werte) + "</p>", note, count=1)
    note = re.sub(r'(?s)<p><strong>Warum relevant</strong></p>\n?<p>.*?</p>',
                   lambda m: "<p><strong>Warum relevant</strong></p>\n<p>" + _esc(warum) + "</p>", note, count=1)
    note = re.sub(r'(?s)<span class="highlight" data-annotation="[^"]*">(.*?)</span>',
                   lambda m: f'<span class="highlight" data-annotation="{data_annotation}">' + _esc(quote_span) + "</span>", note, count=1)
    note = re.sub(r'(?s)<span class="citation" data-citation="[^"]*">.*?</span>',
                   lambda m: f'<span class="citation" data-citation="{data_citation}">(<span class="citation-item">' + _esc(cite_disp[1:-1]) + "</span>)</span>", note, count=1)
    if data_citation_items not in note or data_annotation not in note:
        raise RuntimeError("slot replacement failed — blueprint structure changed")
    return {
        "itemType": "note",
        "parentItem": book_key,
        "note": note,
        "tags": [{"tag": TAG}],
        "relations": {},
    }


def _esc(s):
    return (s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


# --------------------------------------------------------- chunk compare ---

def db_find_chunks(frag, doc_id):
    """Direct doc-scoped chunk lookup via psql (fast, exact-ish)."""
    import subprocess
    sql = ("select c.id::text, c.text, c.locator->>'physical_page_start', c.locator->>'physical_page_end' from processing_chunks c "
           "join processing_snapshots s on s.id=c.snapshot_id and s.active "
           "join zotero_documents d on d.id=s.document_id "
           "where d.id='" + doc_id + "' and replace(replace(c.text, chr(10), ' '), '-', '') ilike '%" 
           + frag.replace("'", "''").replace("-", "") + "%' limit 8")
    r = subprocess.run(["podman", "exec", "axiom-postgres", "psql", "-U", "axiom_user",
                        "-d", "axiom_db", "-t", "-A", "-F", "\x01", "-R", "\x02", "-c", sql],
                       capture_output=True, text=True, timeout=30)
    out = []
    for rec in r.stdout.split("\x02"):
        rec = rec.strip("\n")
        if "\x01" in rec:
            parts = rec.split("\x01")
            out.append({"chunk_id": parts[0], "text": parts[1],
                        "phys_start": parts[2] if len(parts) > 2 else None,
                        "phys_end": parts[3] if len(parts) > 3 else None})
    return out


def db_find_chunks_spacestripped(nuk_frag, doc_id):
    """Stage-5 lookup: compare space-stripped alnum-lowercase forms."""
    import subprocess
    safe = nuk_frag.replace("'", "''")
    sql = ("select c.id::text, c.text from processing_chunks c "
           "join processing_snapshots s on s.id=c.snapshot_id and s.active "
           "join zotero_documents d on d.id=s.document_id "
           "where d.id='" + doc_id + "' and "
           "regexp_replace(lower(regexp_replace(c.text, '\\s', '', 'g')), '[^a-zäöü0-9]', '', 'g') "
           "like '%" + safe + "%' limit 5")
    r = subprocess.run(["podman", "exec", "axiom-postgres", "psql", "-U", "axiom_user",
                        "-d", "axiom_db", "-t", "-A", "-F", "\x01", "-R", "\x02", "-c", sql],
                       capture_output=True, text=True, timeout=30)
    out = []
    for rec in r.stdout.split("\x02"):
        rec = rec.strip("\n")
        if "\x01" in rec:
            cid, text = rec.split("\x01", 1)
            out.append({"chunk_id": cid, "text": text})
    return out


def _window_in(nuk_frag, nuk_text):
    return bool(nuk_frag) and nuk_frag in (nuk_text or "")


def chunk_leg(quote, doc_id, page_index=None):
    """Locate the anchor in the ACTIVE generation. v2 escalation ladder:
    word windows exact -> shifted -> short; offset search across all
    windows; return-verification. No silent give-up."""
    import unicodedata
    def nuk(s):
        s = unicodedata.normalize("NFKC", s).replace("\u00df", "ss").replace("\ufb01", "fi").replace("\ufb02", "fl")
        return "".join(ch.lower() for ch in s if ch.isalnum())
    def sqlfrag(s):
        import unicodedata as _u
        s = _u.normalize("NFKC", s).replace("\u00df", "ss").replace("\ufb01", "fi").replace("\ufb02", "fl")
        return " ".join(s.split())
    def fragment_find(text, frag):
        """Match frag (dash-tolerant) in text; return RAW-text offset."""
        import re as _re
        def strip_dashes(s):
            return s.replace("-", "").replace("\xad", "")
        clean = strip_dashes(text)
        fwords = [strip_dashes(w) for w in frag.split()]
        pat = _re.escape(fwords[0])
        for w in fwords[1:]:
            pat += r"\W{0,6}" + _re.escape(w)
        m = _re.search(pat, clean, _re.I)
        if not m:
            return None
        cpos = m.start()
        ri = 0
        ci = 0
        while ci < cpos and ri < len(text):
            if text[ri] in "-\xad":
                ri += 1
                continue
            ri += 1
            ci += 1
        return ri

    words = norm(quote).split()
    windows = []
    if len(words) >= 6:
        windows += [words[2:6], words[5:9], words[6:10], words[-6:-2], words[3:7]]
    if len(words) >= 5:
        windows += [words[-4:], words[4:8][:4]]
    if len(words) >= 3:
        windows += [words[:3], words[1:4]]

    def core_verify(h):
        """How many of the quote's CORE words (len>3) are in the chunk?"""
        core = [nuk(w) for w in quote.split() if len(nuk(w)) > 3][:14]
        cn = nuk(h["text"])
        return sum(1 for w in core if w in cn), max(1, len(core))

    best_hits = []
    frag_plain = ""
    verified = []
    for frag_w in windows:
        frag = sqlfrag(" ".join(frag_w))
        if len(frag) < 12:
            continue
        hits = db_find_chunks(frag, doc_id)
        if not hits:
            continue
        if not best_hits:
            best_hits = hits
            frag_plain = " ".join(frag_w)
        # v3: a hit whose chunk contains the anchor's CORE words is the
        # real sentence site — running-head fragments only hit title copies
        for h in hits:
            kv, kt = core_verify(h)
            if kt >= 4 and kv >= max(4, int(kt * 0.7)):
                verified.append(h)
        if verified:
            break
    if not best_hits:
        # v2 ESCALATION STAGE 5: space-stripped substring for corrupted
        # text layers (e.g. \x02 word separators) + return-verification
        import unicodedata as _u2
        def nukq(s):
            s = _u2.normalize("NFKC", s).replace("\u00df", "ss").replace("\ufb01", "fi").replace("\ufb02", "fl")
            return "".join(ch.lower() for ch in s if ch.isalnum())
        for frag_w in windows:
            nf = nukq(" ".join(frag_w))
            if len(nf) < 14:
                continue
            hits = db_find_chunks_spacestripped(nf, doc_id)
            if hits:
                # return-verification: chunk must contain the anchor words
                # (nuk-space-stripped containment check)
                nq_full = nukq(quote)
                good = [h for h in hits if nukq(h["text"]) and _window_in(nf, nukq(h["text"]))]
                if good:
                    best_hits = good
                    frag_plain = " ".join(frag_w)
                    break

    if verified:
        pool = verified
        if len(pool) > 1 and page_index is not None:
            here = [h for h in pool
                    if (h.get("phys_start") or "").lstrip("-").isdigit()
                    and (h.get("phys_end") or "").lstrip("-").isdigit()
                    and int(h["phys_start"]) <= page_index <= int(h["phys_end"])]
            if here:
                pool = here
        best_hits = pool
    if best_hits:
        # v3: duplicate resolution — prefer the chunk whose PHYSICAL page
        # range contains the anchor's PDF page (duplicate texts live in
        # compilation reprints / TOC repetitions elsewhere)
        if len(best_hits) > 1 and page_index is not None:
            here = [h for h in best_hits
                    if (h.get("phys_start") or "").lstrip("-").isdigit()
                    and (h.get("phys_end") or "").lstrip("-").isdigit()
                    and int(h["phys_start"]) <= page_index <= int(h["phys_end"])]
            if here:
                best_hits = here
        h = max(best_hits, key=lambda x: len(x["text"]))
        at = None
        for frag_w in windows:
            at = fragment_find(h["text"], " ".join(frag_w))
            if at is not None:
                break
        if at is None:
            # stage-5 offset: nuk-space-stripped mapping (corrupted layers)
            def nuk_index(text):
                import unicodedata as _u
                idx = []
                out = []
                for i, ch in enumerate(text):
                    e = _u.normalize("NFKC", ch).replace("\u00df", "ss").replace("\ufb01", "fi").replace("\ufb02", "fl")
                    for c2 in e:
                        if c2.isalnum():
                            out.append(c2.lower())
                            idx.append(i)
                return "".join(out), idx
            ns, idxmap = nuk_index(h["text"])
            for frag_w in windows:
                nf = nuk(" ".join(frag_w))
                if len(nf) < 14:
                    continue
                p = ns.find(nf)
                if p >= 0:
                    at = idxmap[p]
                    break
        page = None
        if at is not None:
            page = rag_passage_page(h["chunk_id"], at)
            page = page.get("page") if page else None
        out = {"status": "ok", "chunk_id": h["chunk_id"], "at": at,
               "page": page, "locator_label": None, "page_source": None}
        if len(best_hits) > 1:
            out["dup_fragment_chunks"] = len(best_hits)
        if page is None:
            out["page_unresolved"] = True
            cn = nuk(h["text"])
            out["verify_words_in_chunk"] = sum(1 for w in quote.split()[:10] if nuk(w) in cn)
        return out
    # semantic-search fallback with ligature-tolerant comparison
    r = rag_search(quote, doc_id=None, top_n=8)
    hits = r.get("hits", [])
    matches = []
    nq = nuk(quote)
    for h in hits:
        if nq in nuk(h.get("text", "")):
            matches.append(h)
    chunk_ids = {h["chunk_id"] for h in matches}
    if not matches:
        r = rag_search(quote, doc_id=doc_id, top_n=8)
        for h in r.get("hits", []):
            if nq in nuk(h.get("text", "")):
                matches.append(h)
        chunk_ids = {h["chunk_id"] for h in matches}
    if not chunk_ids:
        return {"status": "no_chunk",
                "cause": "anchor text not contained in any chunk of the doc "
                         "(extraction gap: table cells stripped, OCR diff, "
                         "or chunk border); ladder tried %d word windows" % len(windows)}
    if len(chunk_ids) > 1:
        return {"status": "ambiguous", "chunk_ids": sorted(chunk_ids)}
    h = matches[0]
    if h["source"].get("doc_id") != doc_id:
        return {"status": "ambiguous_crossdoc", "chunk_id": h["chunk_id"],
                "doc": h["source"].get("doc_id")}
    raw = h["text"]
    at = norm_find(raw, nq)
    page = rag_passage_page(h["chunk_id"], at) if at is not None else None
    return {
        "status": "ok",
        "chunk_id": h["chunk_id"],
        "at": at,
        "page": page.get("page") if page else None,
        "locator_label": h["locator"].get("label"),
        "page_source": h["locator"].get("page_source"),
    }


def norm_find(raw, nq):
    """Find nq in raw text (whitespace-tolerant); return raw offset."""
    if nq in raw:
        return raw.find(nq)
    # build normalized index mapping
    idx = []
    out = []
    for i, ch in enumerate(raw):
        if ch.isspace():
            if out and out[-1] != " ":
                out.append(" ")
                idx.append(i)
        else:
            out.append(ch)
            idx.append(i)
    ns = "".join(out)
    p = ns.find(nq)
    if p < 0:
        return None
    return idx[p]


# ------------------------------------------------------------------ main ---

def load_results():
    if RESULTS.exists():
        return json.loads(RESULTS.read_text())
    return {}


def save_results(r):
    RESULTS.write_text(json.dumps(r, ensure_ascii=False, indent=1))


def probe(att_key, write=False):
    m = doc_map()
    if att_key not in m:
        return {"error": f"{att_key} not in document map"}
    info = m[att_key]
    pdf_path = STORAGE / att_key / info["filename"]
    if not pdf_path.exists():
        # storage files sometimes drop the + encoding
        cands = list((STORAGE / att_key).glob("*.pdf"))
        if not cands:
            return {"error": "pdf not found", "path": str(pdf_path)}
        pdf_path = cands[0]

    t0 = time.time()
    candmap, err = pick_anchor_candidates(pdf_path)
    if err:
        return {"attachment": att_key, "title": info["title"], "error": err}

    headers = zotero_headers()
    existing = existing_probe_items(headers) if write else []
    ex_att = {fingerprint_att(i["data"].get("parentItem"),
                              i["data"].get("annotationPageLabel"),
                              i["data"].get("annotationText", ""))
              for i in existing if i["data"]["itemType"] == "annotation"}

    out = {"attachment": att_key, "title": info["title"], "item_key": info["item_key"],
           "doc_id": info["doc_id"], "anchors": []}
    used_pages = set()
    verdict = "MATCH"
    for pos_name in ("front", "middle", "back"):
        chosen = None
        tried = 0
        misses = []
        for page_exclusive in (True, False):
            # pass 1: each anchor on its own page; pass 2 (sparse docs):
            # page reuse allowed when the position has no other candidates
            for cand in candmap.get(pos_name, [])[:25]:
                if page_exclusive and cand["page_index"] in used_pages:
                    continue
                tried += 1
                leg = chunk_leg(cand["quote"], info["doc_id"], page_index=cand["page_index"])
                if leg["status"] == "ok" and leg.get("page") is not None:
                    chosen = dict(cand)
                    chosen["position"] = pos_name
                    chosen["chunk"] = leg
                    chosen["tried"] = tried
                    used_pages.add(cand["page_index"])
                    break
                misses.append((leg.get("status"), leg.get("cause")))
            if chosen is not None:
                break
        if chosen is None:
            # v2: SKIP abolished — unlocatable = BLOCKER with cause
            from collections import Counter as _C
            causes = dict(_C(m[0] for m in misses))
            c0 = candmap.get(pos_name, [])[:1]
            entry = {"position": pos_name, "verdict": "BLOCKER",
                     "cause": f"no corpus-locatable anchor after {tried} candidates; "
                              f"leg statuses: {causes}",
                     "candidates": len(candmap.get(pos_name, []))}
            if c0:
                entry.update({"page_index": c0[0]["page_index"],
                              "pdf_label": c0[0]["pdf_label"],
                              "quote": c0[0]["quote"]})
            out["anchors"].append(entry)
            verdict = "BLOCKER"
            continue
        n_druck = str(chosen["pdf_label"] or "")
        m_chunk = str(chosen["chunk"].get("page"))
        if n_druck and n_druck == m_chunk:
            chosen["N"] = n_druck
            chosen["M"] = m_chunk
            chosen["verdict"] = "MATCH"
        else:
            chosen["N"] = n_druck
            chosen["M"] = m_chunk
            reason = "kein eingebettetes Label" if n_druck == "" else "Label weicht ab"
            chosen["verdict"] = f"ABWEICHUNG({reason}: N={n_druck or '—'}, M={m_chunk})"
            verdict = "ABWEICHUNG" if verdict == "MATCH" else verdict
        out["anchors"].append(chosen)

    if write:
        csl = csl_item(headers, info["item_key"])
        blueprint = zotero_get(f"/api/users/0/items/{BLUEPRINT_KEY}?format=json",
                               headers)["data"]["note"]
        for a in out["anchors"]:
            if "error" in a or a.get("verdict") == "BLOCKER":
                continue
            if fingerprint_att(att_key, a["pdf_label"], a["quote"]) in ex_att:
                # sparse docs: the position shares the only corpus-locatable
                # sentence with another anchor — note with explicit reference,
                # no duplicate highlight
                a["annotation"] = {"status": "duplicate_skipped (shared measurement "
                                      "site — see annotation of the other anchor)"}
                note_payload = make_note_payload(info["item_key"], att_key, a, csl, blueprint)
                note_payload["note"] = note_payload["note"].replace(
                    "Dreiecksprobe #195, Anker " + a["position"],
                    "Dreiecksprobe #195, Anker " + a["position"] +
                    " (teilt die Messstelle mit einem anderen Anker — einziger "
                    "korpus-lokalisierbarer Satz)", 1)
                nresp = zotero_post("/api/users/0/items", [note_payload], headers)
                nkey = nresp.get("successful", {}).get("0", {}).get("key")
                a["note"] = {"status": "created" if nkey else "failed",
                             "key": nkey, "resp": nresp if not nkey else None}
                continue
            a["rects"] = word_precise_rects(
                STORAGE / att_key / next((STORAGE / att_key).glob("*.pdf")).name
                if False else pdf_path, a["page_index"], a["quote"], a["rects"])
            payload = make_annotation_payload(att_key, a)
            resp = zotero_post("/api/users/0/items", [payload], headers)
            key = resp.get("successful", {}).get("0", {}).get("key")
            a["annotation"] = {"status": "created" if key else "failed",
                               "key": key, "resp": resp if not key else None}
            if key:
                ex_att.add(fingerprint_att(att_key, a["pdf_label"], a["quote"]))
            note_payload = make_note_payload(info["item_key"], att_key, a, csl, blueprint)
            nresp = zotero_post("/api/users/0/items", [note_payload], headers)
            nkey = nresp.get("successful", {}).get("0", {}).get("key")
            a["note"] = {"status": "created" if nkey else "failed",
                         "key": nkey, "resp": nresp if not nkey else None}
    out["verdict"] = verdict
    out["took_s"] = round(time.time() - t0, 1)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--dry", nargs="*", default=[])
    ap.add_argument("--write", nargs="*", default=[])
    ap.add_argument("--report")
    args = ap.parse_args()

    if args.list:
        m = doc_map()
        for k, v in sorted(m.items(), key=lambda kv: kv[1]["title"].lower()):
            print(k, "|", v["title"][:70])
        return

    results = load_results()
    for key in args.dry + args.write:
        r = probe(key, write=key in args.write)
        results[key] = r
        save_results(results)
        print(json.dumps(r, ensure_ascii=False, indent=1))

    if args.report:
        rows = []
        for k, r in results.items():
            rows.append((k, r.get("title", "?")[:50], r.get("verdict", r.get("error", "?")),
                         [a.get("verdict", "?") for a in r.get("anchors", [])]))
        print(f"{'ATT':9} {'VERDICT':10} ANCHORS  TITLE")
        for k, t, v, av in sorted(rows, key=lambda x: x[1]):
            print(f"{k:9} {v:10} {av}  {t}")


if __name__ == "__main__":
    main()
