"""EPUB CFI (Canonical Fragment Identifier) extraction for contract §11.

Parses an EPUB ZIP to build a mapping from text content to EPUB-CFI locators.
This enables citation-accurate chunk locators for EPUBs (§11: "EPUB may use
locator.type = epub_cfi"). Without CFI, EPUB chunks would carry fabricated
page labels (§11 violation — "MUST NOT fabricate").

Builds spec-conformant CFI paths: ``epubcfi(/6/{spine_step}!/4/{elem_step})``
where:
  - /6 = the package document wrapper step
  - {spine_step} = (spine_index + 1) × 2 — even index for the spine itemref
  - ! separates the package-document step from the content-document step
  - /4 = step into the body element
  - {elem_step} = (element_child_index + 1) × 2 — even index for elements

Per the EPUB-CFI spec (https://idpf.org/epub/linking/cfi/): even indices
address element nodes, odd indices address character data. All emitted
steps use even (element) indices.
"""

from __future__ import annotations

import re
import zipfile
from html.parser import HTMLParser
from pathlib import Path
from typing import Any

# Void elements in HTML that never have a closing tag (HTML5 spec).
_VOID_TAGS = frozenset({"img", "br", "hr", "input", "meta", "link", "area",
                        "base", "col", "embed", "source", "track", "wbr"})


class _CFICollector(HTMLParser):
    """Walk XHTML and collect text↔CFI-element mappings.

    For each block-level element (<p>, <h1>-<h6>, <div>, <li>, etc.),
    records the element's CFI step and its text content (stripped).

    Handles non-XHTML HTML constructs that html.parser does not resolve:
    - Void elements (<img>, <br>) without self-closing slash are tracked
      without incrementing depth.
    - Implied end tags (an unclosed <p> followed by a new <p>) close the
      previous entry automatically (C2 fix).
    """

    BLOCK_TAGS = frozenset({"p", "h1", "h2", "h3", "h4", "h5", "h6", "div",
                            "li", "blockquote", "pre", "td", "th", "section",
                            "article", "header", "footer", "figure"})

    def __init__(self, chapter_cfi_base: str) -> None:
        # chapter_cfi_base has NO closing paren: "epubcfi(/6/4!"
        super().__init__(convert_charrefs=True)
        self._base = chapter_cfi_base
        self._depth = 0
        self._body_child_idx = 0  # element count within body (0-based)
        self._in_body = False
        self._current_tag = ""
        self._current_text = ""
        self._current_cfi = ""
        self._current_elem = 0  # #220: body-child index for page-anchor maps
        # #234: raw text-char offset stream — parity with
        # compute_core.epub_pagelist._AnchorScanner (both count handle_data
        # chars in document order, unconditionally incl. <head> text) so
        # anchor and entry positions compare in ONE stream. Synthetic
        # separators (void placeholders, block-boundary spaces) also step
        # the counter: anchor offsets and entry text lengths then live in
        # the same space.
        self._total_chars = 0
        self._cur_start = 0
        self.entries: list[dict[str, Any]] = []

    def _flush_entry(self) -> None:
        """Emit the current block entry if it has text."""
        if self._current_tag:
            text = self._current_text.strip()
            if text:
                self.entries.append({
                    "cfi": self._current_cfi,
                    "text": text,
                    "tag": self._current_tag,
                    # #220: position keys shared with compute_core.
                    # epub_pagelist (body-child element index) —
                    # annotate_cfi_entries joins on these.
                    "elem": self._current_elem,
                    # #234: raw char offset where this entry's text begins
                    # (same stream as _AnchorScanner's anchor chars).
                    "start": self._cur_start,
                })
        self._current_tag = ""
        self._current_text = ""
        self._current_cfi = ""

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        tag = tag.lower()
        if tag == "body":
            self._in_body = True
            return
        if not self._in_body:
            return

        # C2 fix: void elements never have end tags — don't affect depth.
        if tag in _VOID_TAGS:
            # #220 foliate-js mirror: a top-level void element is still an
            # element child of <body> (nodeType 1) and consumes an element
            # step — skipping it would shift every following sibling.
            if self._depth == 0:
                self._body_child_idx += 1
            elif self._depth >= 1:
                self._current_text += " "  # placeholder for void content
                self._total_chars += 1  # #234: one stream with anchor chars
            return

        if self._depth == 0:
            # Top-level child of <body>
            self._body_child_idx += 1
            self._current_elem = self._body_child_idx
            if tag in self.BLOCK_TAGS:
                # C2 fix: implied end tag — a new block at depth 0 closes
                # any previous unclosed block.
                if self._current_tag:
                    self._flush_entry()
                self._depth = 1
                self._current_tag = tag
                self._current_text = ""
                self._cur_start = self._total_chars  # #234
                # C1 fix: even element index (1-based element count × 2).
                elem_step = self._body_child_idx * 2
                self._current_cfi = f"{self._base}!/4/{elem_step})"
        elif self._depth == 1 and tag in self.BLOCK_TAGS and tag == self._current_tag:
            # C2 fix: implied end tag at depth 1 — <p>a<p>b is legal HTML:
            # the first <p> is implicitly closed by the second. Flush the
            # current entry and start a new sibling.
            self._flush_entry()
            self._body_child_idx += 1
            self._current_elem = self._body_child_idx
            self._current_tag = tag
            self._current_text = ""
            self._cur_start = self._total_chars  # #234
            elem_step = self._body_child_idx * 2
            self._current_cfi = f"{self._base}!/4/{elem_step})"
            # depth stays at 1
        else:
            if tag in self.BLOCK_TAGS and self._depth >= 1:
                # #234: nested block boundary — the flattened entry text
                # abuts blocks with NO separator ("…Lakehouse?Let's dig…"),
                # but the markdown-derived chunk text carries the boundary
                # as whitespace. A single separator space (mirroring the
                # void-content placeholder above) makes cross-boundary
                # probes match; _normalize_text collapses it away on both
                # sides when no boundary is involved.
                self._current_text += " "
                self._total_chars += 1  # #234: one stream with anchor chars
            self._depth += 1

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        """Self-closing tags like <img/> — treat as void (no depth change)."""
        self.handle_starttag(tag, attrs)
        if tag.lower() not in _VOID_TAGS and self._in_body and self._depth == 1:
            # Self-closing block tag (<div/>) — close immediately
            self._depth = 0
            self._flush_entry()

    def handle_endtag(self, tag: str) -> None:
        tag = tag.lower()
        if tag == "body":
            # Flush any remaining entry
            self._flush_entry()
            self._in_body = False
            return
        if not self._in_body:
            return
        if tag in _VOID_TAGS:
            return  # void elements have no meaningful end tag
        if self._depth == 1 and tag == self._current_tag:
            self._flush_entry()
            self._depth = 0
        elif self._depth > 1:
            self._depth -= 1
        elif self._depth == 1 and tag != self._current_tag:
            # Mismatched end tag — treat as closing the current block (C2).
            self._flush_entry()
            self._depth = 0

    def handle_data(self, data: str) -> None:
        self._total_chars += len(data)  # #234: unconditional offset stream
        if self._depth >= 1:
            self._current_text += data


def _parse_opf_spine(epub: zipfile.ZipFile) -> list[str]:
    """Return the ordered list of XHTML file paths from the OPF spine."""
    try:
        container = epub.read("META-INF/container.xml").decode("utf-8", errors="replace")
    except KeyError:
        return []
    opf_match = re.search(r'full-path="([^"]+)"', container)
    if not opf_match:
        return []
    opf_path = opf_match.group(1)
    try:
        opf = epub.read(opf_path).decode("utf-8", errors="replace")
    except KeyError:
        return []
    opf_dir = str(Path(opf_path).parent)

    id_to_href: dict[str, str] = {}
    for m in re.finditer(r'<item[^>]*\bid="([^"]+)"[^>]*\bhref="([^"]+)"', opf):
        id_to_href[m.group(1)] = m.group(2)
    for m in re.finditer(r'<item[^>]*\bhref="([^"]+)"[^>]*\bid="([^"]+)"', opf):
        id_to_href[m.group(2)] = m.group(1)

    spine_ids: list[str] = []
    for m in re.finditer(r'<itemref[^>]*\bidref="([^"]+)"', opf):
        spine_ids.append(m.group(1))

    hrefs: list[str] = []
    for sid in spine_ids:
        href = id_to_href.get(sid, "")
        if href:
            full = str(Path(opf_dir) / href) if opf_dir else href
            hrefs.append(full)
    return hrefs


def build_cfi_map(epub_path: str) -> list[dict[str, Any]]:
    """Build a list of {cfi, text, tag} entries from an EPUB.

    Each entry maps a text block to its spec-conformant EPUB-CFI element
    step. The caller matches markdown paragraphs to these entries to assign
    cfi_start/cfi_end locators to chunks.
    """
    try:
        epub = zipfile.ZipFile(epub_path)
    except (zipfile.BadZipFile, FileNotFoundError):
        return []

    spine_hrefs = _parse_opf_spine(epub)
    if not spine_hrefs:
        spine_hrefs = sorted(n for n in epub.namelist()
                             if n.endswith((".html", ".xhtml"))
                             and not n.startswith("META-INF/"))

    all_entries: list[dict[str, Any]] = []
    for spine_idx, href in enumerate(spine_hrefs):
        candidates = [href, href.replace("\\", "/")]
        for name in epub.namelist():
            if name.endswith(href) or href.endswith(name):
                candidates.append(name)
        raw = None
        for c in candidates:
            try:
                raw = epub.read(c).decode("utf-8", errors="replace")
                break
            except KeyError:
                continue
        if raw is None:
            continue

        # C1 fix: even spine step + ! separator, NO closing paren (the
        # _CFICollector appends !/4/{elem}) to complete the CFI.
        spine_step = (spine_idx + 1) * 2
        cfi_base = f"epubcfi(/6/{spine_step}"
        collector = _CFICollector(cfi_base)
        collector.feed(raw)
        # Flush any remaining entry at end of feed.
        collector._flush_entry()
        for e in collector.entries:
            e["spine"] = spine_idx  # #220: position key for page anchors
        all_entries.extend(collector.entries)

    epub.close()
    return all_entries


def _normalize_text(text: str) -> str:
    """Strip markdown/HTML markup and collapse whitespace for matching."""
    clean = re.sub(r"<[^>]+>", " ", text)  # strip HTML tags
    clean = re.sub(r"[#*`>|_-]", " ", clean)  # strip markdown chars
    clean = re.sub(r"\s+", " ", clean).strip()
    return clean


def match_text_to_cfi(text: str, cfi_entries: list[dict[str, Any]]) -> tuple[str, str]:
    """Find the best CFI start/end for a chunk's text.

    Matching: bidirectional substring match with a minimum entry length
    (C3 fix: short entries like "1" cannot poison matches).

    Fallback: carry-forward — an unmatched chunk inherits the previous
    chunk's last matched position (chapter-scoped, not book-start).
    The first chunk with no match gets the first entry's CFI.
    """
    if not cfi_entries:
        return "", ""

    clean = _normalize_text(text)
    min_entry_len = 12  # C3 fix: ignore ultra-short entries

    # Find the first CFI entry whose text matches the chunk
    cfi_start = ""
    cfi_start_idx = -1
    for i, entry in enumerate(cfi_entries):
        entry_text = _normalize_text(entry["text"])
        if len(entry_text) < min_entry_len:
            continue
        # Bidirectional: entry appears in chunk, or chunk start matches entry
        if (entry_text[:40] in clean or
                (len(clean) >= 20 and clean[:40] in entry_text)):
            cfi_start = entry["cfi"]
            cfi_start_idx = i
            break

    # Find the last matching entry — bounded to a forward window after the
    # start (a chunk is at most a few dozen blocks; without the bound the
    # reverse scan matches generic/boilerplate text far across the book and
    # produces absurd page spans).
    cfi_end = ""
    if cfi_start_idx >= 0:
        for i in range(min(len(cfi_entries), cfi_start_idx + 60) - 1,
                       cfi_start_idx - 1, -1):
            entry_text = _normalize_text(cfi_entries[i]["text"])
            if len(entry_text) < min_entry_len:
                continue
            if (entry_text[-40:] in clean or
                    (len(clean) >= 20 and clean[-40:] in entry_text)):
                cfi_end = cfi_entries[i]["cfi"]
                break

    # Fallback: no text match → carry-forward (first entry as anchor).
    if not cfi_start:
        cfi_start = cfi_entries[0]["cfi"]
    if not cfi_end:
        cfi_end = cfi_start

    return cfi_start, cfi_end
