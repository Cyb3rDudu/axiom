"""#220 Stage 1 — EPUB page-anchor parser (four dialects) + CFI annotation.

Turns publisher print-page anchors into a uniform map so epub_cfi locators
can carry citable print pages (page_source = epub_pagelist). Self-build by
owner decision (#220 decision comment): no permissive standalone parser
exists; readers solve this internally.

Dialects (real shapes from the #220 inventory comment):
  class="page"   Jossé/dtv:  <a class="page" id="page_N">N</a> (inline)
  id="page_N"    Bieger/Springer: <a id="page_N"/>[N] (often top-level)
  epub:type      EPUB3/ProQuest: <span epub:type="pagebreak" title="N"/>
  page-map.xml   Adobe: <page-entry page-number="N" href="doc#frag"/>

Never guess: anchors are only trusted when their page numbers form a
monotone (non-decreasing) sequence in spine order — otherwise the map is
flagged and the runner refuses enrichment (no silent upgrade).
"""
from __future__ import annotations

import itertools
import logging
import posixpath
import re
import zipfile
from html.parser import HTMLParser
from pathlib import Path
from typing import Any

from axiom_ng_runner.epub_cfi import _parse_opf_spine

logger = logging.getLogger(__name__)

_VOID_TAGS = frozenset({"img", "br", "hr", "input", "meta", "link", "area",
                        "base", "col", "embed", "source", "track", "wbr"})

_ID_PAGE = re.compile(r"^page[_-]?(\d{1,4})$", re.IGNORECASE)
_NUM = re.compile(r"(\d{1,4})")


def _attr_num(attrs: dict[str, str | None], *names: str) -> int | None:
    for n in names:
        v = attrs.get(n)
        if v:
            m = _NUM.search(v)
            if m:
                return int(m.group(1))
    return None


class _AnchorScanner(HTMLParser):
    """Walks one XHTML doc, counting top-level body children EXACTLY like
    ``epub_cfi._CFICollector`` (EVERY element at depth 0 increments the
    index, void tags included — comments/CDATA are invisible to
    html.parser, matching the foliate-js element/text-only child-counting
    rule). Records the (element index, page) position of every page
    anchor."""

    def __init__(self, frag_pages: dict[str, int]) -> None:
        super().__init__(convert_charrefs=True)
        self._frag_pages = frag_pages  # Adobe page-map: id-frag -> page
        self._depth = 0
        self._body_child_idx = 0
        self._in_body = False
        self._stack: list[str] = []
        # pending anchor waiting for its number from text content
        self._pending: list[dict[str, Any]] = []
        self.anchors: list[dict[str, Any]] = []  # {elem, page}

    def _emit(self, elem: int, page: int) -> None:
        self.anchors.append({"elem": elem, "page": page})

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag = tag.lower()
        if tag == "body":
            self._in_body = True
            return
        if not self._in_body:
            return
        if tag in _VOID_TAGS:
            # Void tags never open/close/flush a pending anchor, but a
            # top-level one is still an element child of <body> and consumes
            # an element step — exact parity with _CFICollector.
            if self._depth == 0:
                self._body_child_idx += 1
            return
        a = {k.lower(): v for k, v in attrs}
        if self._depth == 0:
            self._body_child_idx += 1
        elem = self._body_child_idx
        self._stack.append(tag)
        self._depth += 1

        page = self._detect(tag, a)
        if page is not None:
            self._emit(elem, page)
        elif self._is_candidate(tag, a):
            # anchor without a number in its attrs — collect the text INSIDE
            # it to resolve the number (never from unrelated later text)
            self._pending.append({"depth": self._depth, "elem": elem,
                                  "text": ""})

    def _is_candidate(self, tag: str, a: dict[str, str | None]) -> bool:
        etype = (a.get("epub:type") or a.get("type") or "").split()
        classes = (a.get("class") or "").split()
        aid = a.get("id") or ""
        return (
            "pagebreak" in etype
            or "page" in classes
            or (bool(aid) and bool(_ID_PAGE.match(aid)))
            or aid in self._frag_pages
        )

    def _detect(self, tag: str, a: dict[str, str | None]) -> int | None:
        """Anchor dialects; page number precedence: title > id > (text)."""
        etype = (a.get("epub:type") or a.get("type") or "").split()
        classes = (a.get("class") or "").split()
        aid = a.get("id") or ""

        if "pagebreak" in etype:
            return _attr_num(a, "title", "id")
        if "page" in classes:
            return _attr_num(a, "id", "title")
        if aid:
            m = _ID_PAGE.match(aid)
            if m:
                return int(m.group(1))
            if aid in self._frag_pages:  # Adobe page-map fragment target
                return self._frag_pages[aid]
        return None

    def handle_data(self, data: str) -> None:
        # Only text INSIDE the pending anchor's element may resolve its
        # number — anything after its end tag belongs to other content.
        if self._pending and self._depth >= self._pending[-1]["depth"]:
            self._pending[-1]["text"] += data

    def _pop_pending(self) -> None:
        """Resolve the innermost pending anchor from its OWN element text
        or drop it — an anchor that closes without a resolvable number is
        never kept for later (no digit scavenging from following text)."""
        p = self._pending.pop()
        m = _NUM.search(p["text"])
        if m:
            self._emit(p["elem"], int(m.group(1)))

    def _flush_pending(self) -> None:
        """Resolve pending anchors still open at document end."""
        while self._pending:
            self._pop_pending()

    def handle_endtag(self, tag: str) -> None:
        tag = tag.lower()
        if tag == "body":
            self._flush_pending()
            self._in_body = False
            self._depth = 0
            self._stack = []
            return
        if not self._in_body or tag in _VOID_TAGS:
            return
        # lenient stack pop: XHTML is usually well-formed; implied end tags
        # just pop the nearest match (or the top) — anchor detection only
        # needs the depth, not perfect nesting.
        if tag in self._stack:
            while self._stack and self._stack.pop() != tag:
                self._depth = max(0, self._depth - 1)
        self._depth = max(0, self._depth - 1)
        # Pendings whose element just closed (directly or via an implied
        # end tag): resolve from their own text or drop them here.
        while self._pending and self._pending[-1]["depth"] > self._depth:
            self._pop_pending()

    def close(self) -> None:
        super().close()
        self._flush_pending()


def _read_page_map_xml(epub: zipfile.ZipFile) -> list[tuple[str, int]]:
    """Adobe page-map entries: [(href (with optional #frag), page)]."""
    for name in epub.namelist():
        if not name.endswith("page-map.xml"):
            continue
        raw = epub.read(name).decode("utf-8", errors="replace")
        base = str(Path(name).parent)
        if base in ("", "."):
            base = ""
        out: list[tuple[str, int]] = []
        for m in re.finditer(r"<page-entry\b[^>]*>", raw):
            tag = m.group(0)
            pm = re.search(r'(?:page-number|name)="(\d{1,4})"', tag)
            hm = re.search(r'href="([^"]+)"', tag)
            if pm and hm:
                href = hm.group(1)
                if base and not href.startswith(("http", "/")):
                    href = f"{base}/{href}"
                out.append((href, int(pm.group(1))))
        return out
    return []


def parse_page_map(epub_path: str) -> dict[str, Any]:
    """Scan an EPUB for print-page anchors (all four dialects).

    Returns {"anchors": [{spine, elem, page, cfi}], "count", "monotone",
    "dialects"}. Anchor CFIs use the same element-step scheme as
    build_cfi_map: epubcfi(/6/{(spine+1)*2}!/4/{elem*2}). ``monotone`` is
    False when page numbers drop anywhere in spine order — the caller must
    refuse enrichment then (never guess)."""
    try:
        epub = zipfile.ZipFile(epub_path)
    except (zipfile.BadZipFile, FileNotFoundError):
        return {"anchors": [], "count": 0, "monotone": False, "dialects": []}

    spine_hrefs = _parse_opf_spine(epub)
    # Adobe page-map: href (relative to page-map location) -> page; plus
    # per-doc frag targets resolved against element ids by the scanner.
    pmap = _read_page_map_xml(epub)
    frag_pages_by_doc: dict[str, dict[str, int]] = {}
    doc_page: dict[str, list[int]] = {}  # doc start pages from page-map hrefs
    for href, page in pmap:
        if "#" in href:
            doc, frag = href.split("#", 1)
            frag_pages_by_doc.setdefault(doc, {})[frag] = page
        else:
            doc_page.setdefault(href, []).append(page)

    anchors: list[dict[str, Any]] = []
    dialects: set[str] = set()
    raw_names = epub.namelist()
    for spine_idx, href in enumerate(spine_hrefs):
        raw = None
        for c in (href, href.replace("\\", "/")):
            for name in raw_names:
                if name.endswith(c) or c.endswith(name):
                    try:
                        raw = epub.read(name).decode("utf-8", errors="replace")
                        break
                    except KeyError:
                        continue
            if raw is not None:
                break
        if raw is None:
            continue
        if re.search(r'epub:type="[^"]*pagebreak', raw):
            dialects.add("epub_type_pagebreak")
        if re.search(r'class="[^"]*\bpage\b', raw):
            dialects.add("class_page")
        if re.search(r'id="page[_-]?\d+"', raw):
            dialects.add("id_page_n")
        scanner = _AnchorScanner(frag_pages_by_doc.get(href, {}))
        scanner.feed(raw)
        scanner.close()
        spine_step = (spine_idx + 1) * 2
        for a in scanner.anchors:
            anchors.append({
                "spine": spine_idx,
                "elem": a["elem"],
                "page": a["page"],
                "cfi": f"epubcfi(/6/{spine_step}!/4/{a['elem'] * 2})",
            })
        # page-map doc-level entries: page starts at the FIRST element of
        # the target doc (elem 0 → before every block).
        for page in doc_page.get(href, []):
            anchors.append({
                "spine": spine_idx, "elem": 0, "page": page,
                "cfi": f"epubcfi(/6/{spine_step}!/4)",
            })
    if pmap:
        dialects.add("page_map_xml")
    epub.close()

    anchors.sort(key=lambda a: (a["spine"], a["elem"]))
    pages = [a["page"] for a in anchors]
    monotone = all(b >= a for a, b in itertools.pairwise(pages))
    return {"anchors": anchors, "count": len(anchors), "monotone": monotone,
            "dialects": sorted(dialects)}


def annotate_cfi_entries(
    cfi_entries: list[dict[str, Any]],
    anchors: list[dict[str, Any]],
) -> int:
    """Carry anchor pages onto cfi_entries (in place). A page applies from
    its anchor's (spine, elem) position forward until the next anchor —
    across spine docs too (print pages run through the whole book).
    Returns the number of annotated entries."""
    if not anchors:
        return 0
    queue = sorted(anchors, key=lambda a: (a["spine"], a["elem"]))
    # Seed ONLY from an anchor at the very first position (spine 0, elem 0):
    # a page that starts later must not leak backwards into earlier spine
    # docs (cover/TOC) — those entries carry no page at all.
    page: int | None = None
    qi = 0
    if (queue[0]["spine"], queue[0]["elem"]) == (0, 0):
        page = queue[0]["page"]
        qi = 1
    n = 0
    for e in cfi_entries:  # entries are in spine order (build_cfi_map)
        pos = (e.get("spine", 0), e.get("elem", 0))
        while qi < len(queue) and (queue[qi]["spine"], queue[qi]["elem"]) <= pos:
            page = queue[qi]["page"]
            qi += 1
        if page is not None:
            e["page"] = page
            n += 1
    return n


# ---------------------------------------------------------------------------
# #223 — printed-TOC verification: book-internal print-folio proof
# ---------------------------------------------------------------------------

_TOC_LINE = re.compile(r"^(.{3,200}?)[\s.\u00b7\u2022_\-]*(\d{1,4})$")


def _norm_title(t: str) -> str:
    """Fuzzy TOC-title key: lowercase, digits/dots/space stripped — survives
    '1.1 Grundlagen' vs 'Grundlagen' and dot-leader remnants."""
    return re.sub(r"[\d\s.\u00b7:;\-]+", "", t.lower())


class _TOCScanner(HTMLParser):
    """Collects printed-TOC entries from one doc: per block (p/li/div) the
    flattened text and the first <a href> inside. An entry is a line ending
    in an arabic number (dot-leader forms included).

    Buffer discipline (review W2): the buffer clears when a block CLOSES
    (after emitting), so closing an outer wrapper (<div><ol><li>…</li></ol>
    </div>) re-emits nothing; an inner block OPEN does not wipe the outer
    buffer (leading wrapper text merges into the first entry)."""

    _BLOCK = frozenset({"p", "li", "div"})

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self._block = 0
        self._href: str | None = None
        self.entries: list[dict[str, Any]] = []  # {title, page, href}
        self._buf: list[str] = []
        self._buf_href: str | None = None

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag = tag.lower()
        if tag in self._BLOCK:
            self._block += 1  # inner opens never wipe the outer buffer
        elif tag == "a" and self._block and self._href is None:
            a = {k.lower(): v for k, v in attrs}
            self._href = a.get("href")

    def handle_endtag(self, tag: str) -> None:
        tag = tag.lower()
        if tag in self._BLOCK and self._block:
            self._block -= 1
            text = re.sub(r"\s+", " ", "".join(self._buf)).strip()
            if text:
                m = _TOC_LINE.match(text)
                if m and _norm_title(m.group(1)):
                    self.entries.append({
                        "title": m.group(1).strip(),
                        "page": int(m.group(2)),
                        "href": self._buf_href,
                    })
            self._href = None
            self._buf = []  # W2: cleared at close — an outer close emits nothing
            self._buf_href = None
        elif tag == "a":
            self._href = None

    def handle_data(self, data: str) -> None:
        if self._block:
            self._buf.append(data)
            if self._buf_href is None:
                self._buf_href = self._href


def _find_nav(epub: zipfile.ZipFile, opf_path: str) -> dict[str, str]:
    """EPUB3 nav doc (item properties contains 'nav') → {norm_title: href}."""
    try:
        opf = epub.read(opf_path).decode("utf-8", "replace")
    except KeyError:
        return {}
    href = None
    for m in re.finditer(r"<item\b[^>]*>", opf):
        tag = m.group(0)
        if re.search(r'properties="[^"]*\bnav\b', tag):
            hm = re.search(r'href="([^"]+)"', tag)
            if hm:
                href = hm.group(1)
                break
    if not href:
        names = epub.namelist()
        # prefer the real nav doc over a toc.xhtml (name-order independent)
        for suffix in ("nav.xhtml", "nav.html", "toc.ncx", "toc.xhtml", "toc.html"):
            hit = next((n for n in names if n.lower().endswith(suffix)), None)
            if hit:
                href = hit
                break
    if not href:
        return {}
    for name in epub.namelist():
        if name == href or name.endswith("/" + href):
            try:
                raw = epub.read(name).decode("utf-8", "replace")
            except KeyError:
                continue
            nav: dict[str, str] = {}
            for m in re.finditer(r"<a\b[^>]*href=\"([^\"]+)\"[^>]*>(.*?)</a>",
                                 raw, re.IGNORECASE | re.DOTALL):
                title = re.sub(r"<[^>]+>", "", m.group(2))
                key = _norm_title(title)
                if key:
                    nav[key] = m.group(1)
            return nav
    return {}


def _resolve_spine(href: str, spine_hrefs: list[str]) -> int | None:
    """Spine index for a (possibly relative) href — matched by suffix."""
    tail = href.split("#")[0]
    for i, s in enumerate(spine_hrefs):
        if s == tail or s.endswith("/" + tail) or tail.endswith(s):
            return i
    return None


def verify_print_folios(
    epub_path: str, anchors: list[dict[str, Any]]
) -> dict[str, Any]:
    """#223: cross-check the EPUB's own printed TOC against the page anchors
    of the target docs — book-internal proof that markers are print folios.

    Join semantics (review C1): a TOC entry page P targeting spine doc S
    matches when ANY anchor in S carries page P (subsection entries hit
    intra-chapter markers exactly); otherwise the offset is the chapter-
    start drift (first anchor of S minus P) — constant chapter drift stays
    detectable as `divergent`.

    Verdicts: ``verified`` (>= 3 joins, >= 80% exact marker==TOC matches),
    ``divergent`` (systematic offset or mismatch — likely reader
    pagination; ``offset`` reports the dominant drift), ``no_toc`` (no
    printable TOC found or < 3 joins — unverifiable, stays honest)."""
    empty = {"verdict": "no_toc", "joins": 0, "matched": 0, "offset": None}
    try:
        epub = zipfile.ZipFile(epub_path)
    except (zipfile.BadZipFile, FileNotFoundError):
        return empty
    spine_hrefs = _parse_opf_spine(epub)
    if not spine_hrefs or not anchors:
        epub.close()
        return empty

    first_anchor_spine = min(a["spine"] for a in anchors)
    # candidate printed TOC: a spine doc BEFORE the first anchor doc with
    # >= 4 number-terminated block lines (chapter docs rarely qualify).
    toc_entries: list[dict[str, Any]] = []
    toc_dir = ""
    raw_names = epub.namelist()
    opf_path = next((n for n in raw_names if n.lower().endswith(".opf")), "")
    nav = _find_nav(epub, opf_path)
    for idx, href in enumerate(spine_hrefs):
        if idx >= first_anchor_spine:
            break
        raw = None
        for name in raw_names:
            if name == href or name.endswith("/" + href) or href.endswith(name):
                try:
                    raw = epub.read(name).decode("utf-8", "replace")
                    toc_dir = posixpath.dirname(name)
                    break
                except KeyError:
                    continue
        if raw is None:
            continue
        scanner = _TOCScanner()
        scanner.feed(raw)
        # >= 4 number-terminated block lines = a printed TOC; the join
        # gate (>= 3 joins) does the real deciding.
        if len(scanner.entries) >= 4:
            toc_entries = scanner.entries
            break
    epub.close()
    if not toc_entries:
        return empty

    # join (review C1): each printed-TOC entry joins against the FULL
    # anchor set of its target doc — subsection pages hit intra-chapter
    # markers exactly; only unmatched entries contribute chapter drift.
    anchors_in_spine: dict[int, list[dict[str, Any]]] = {}
    for a in sorted(anchors, key=lambda x: (x["spine"], x["elem"])):
        anchors_in_spine.setdefault(a["spine"], []).append(a)

    offsets: list[int] = []
    joins = 0
    matched = 0
    for e in toc_entries:
        href = e.get("href")
        if not href:
            href = nav.get(_norm_title(e["title"]))
        if not href:
            continue
        if toc_dir and not href.startswith(("http", "/")):
            href = posixpath.normpath(posixpath.join(toc_dir, href))
        sp = _resolve_spine(href, spine_hrefs)
        if sp is None:
            continue
        doc_anchors = anchors_in_spine.get(sp)
        if not doc_anchors:
            continue
        joins += 1
        if any(a["page"] == e["page"] for a in doc_anchors):
            offsets.append(0)
            matched += 1
        else:
            offsets.append(doc_anchors[0]["page"] - e["page"])
    if joins < 3:
        return {"verdict": "no_toc", "joins": joins, "matched": matched,
                "offset": None}

    if joins and matched >= joins * 0.8:
        return {"verdict": "verified", "joins": joins, "matched": matched,
                "offset": 0}
    # dominant non-zero offset = systematic drift (reader pagination)
    counts: dict[int, int] = {}
    for o in offsets:
        counts[o] = counts.get(o, 0) + 1
    dom_off, dom_n = max(counts.items(), key=lambda kv: kv[1])
    return {"verdict": "divergent", "joins": joins, "matched": matched,
            "offset": dom_off if dom_n >= joins * 0.8 else None}


def sanity_check(anchors: list[dict[str, Any]]) -> dict[str, Any]:
    """#223 sanity guards regardless of TOC: plausible numbering start
    (arabic body starts small; roman frontmatter would not appear as arabic
    anchors anyway) and folio count/page range plausibility (monotonicity
    is gated separately by the runner before this runs).
    Returns {ok, reasons} — a False ok must refuse enrichment (never guess)."""
    reasons: list[str] = []
    if len(anchors) < 5:
        reasons.append(f"too few anchors ({len(anchors)})")
    pages = [a["page"] for a in anchors]
    if pages and pages[0] > 60:
        reasons.append(f"implausible numbering start ({pages[0]})")
    # sparse chapter-anchored maps are legitimate: a 350-page book with
    # 11 chapter anchors passes, a 10-anchor map spanning 5000 pages still
    # refuses (review W4; absolute cap stays at 5000). Ratio 40 covers the
    # review's examples (350/11 ≈ 32 pages per chapter anchor) while the
    # junk class (5000/10 = 500) stays far above it.
    if pages and (max(pages) > 5000 or max(pages) > len(pages) * 40):
        reasons.append(f"implausible page range (max {max(pages)}, {len(pages)} anchors)")
    return {"ok": not reasons, "reasons": reasons}
