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
