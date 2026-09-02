"""#234 — chapter-1 interior folio coverage for wrapped-chapter EPUBs.

The Databricks shape: each publisher chapter doc wraps ALL content in one
top-level element. Both scanners then assign everything the same
(spine, elem) position, and the pre-fix annotate_cfi_entries collapsed
the whole chapter anchor run onto its LAST page (chapter 1 → page 14;
interior pages 2–13 unreachable; the book's most-cited section carried no
folio granularity at all).

These tests pin, at the REAL pipeline-function level
(_enrich_epub_cfi_locators + annotate_cfi_entries + interior_page):
  - the chapter-wide entry's page is the chapter START page (1),
  - interior chunks interpolate real print pages from the anchor run,
  - front-matter chunks (before the first anchor) keep page_source=none,
  - the #226 counter-direction: a NON-MONOTONE anchor map keeps refusing —
    chunks carry no pages at all, interior or otherwise
    (mutation probe: re-enabling any interior enrichment for refused maps
    turns the refusal test red).

Run: .venv/bin/python -m pytest tests/test_early_folio_gap.py
"""
from __future__ import annotations

import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core import epub_pagelist
from axiom_ng_runner.compute_core import page_trust as pt
from axiom_ng_runner.epub_cfi import build_cfi_map
from axiom_ng_runner.runner import _enrich_epub_cfi_locators

_PARA_A = ("Chapter one opens with the definition of the lakehouse "
           "architecture and its role in modern data platforms. " * 6)
_PARA_B = ("The second section discusses unified governance and the "
           "shared security model across the entire platform. " * 6)
_PARA_C = ("A third section covers streaming tables and real-time "
           "analytics workloads on the lakehouse. " * 6)


def _pagebreak(n: int) -> str:
    return (f'<span xmlns:epub="http://www.idpf.org/2007/ops" '
            f'epub:type="pagebreak" role="doc-pagebreak" id="PB{n}" '
            f'aria-label="{n}" title="{n}"/>')


def _chapter_doc(pages: list[int]) -> str:
    """Databricks shape: the WHOLE chapter in ONE top-level wrapper div."""
    body = f"<h1>Chapter One</h1>{_pagebreak(pages[0])}<p>{_PARA_A}</p>"
    for pg, para in zip(pages[1:], (_PARA_B, _PARA_C)):
        body += f"{_pagebreak(pg)}<p>{para}</p>"
    return (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml">'
        f"<head><title>c</title></head><body><div>{body}</div></body></html>"
    )


_FRONT_DOC = (
    '<?xml version="1.0" encoding="utf-8"?>'
    '<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml">'
    "<head><title>f</title></head><body><div>"
    "<p>This is unpaginated front matter with no print anchors at all "
    "and enough text to form a real matching entry for the probe.</p>"
    "</div></body></html>"
)


def _write_epub(path: Path, pages: list[int]) -> None:
    opf = (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<package xmlns="http://www.idpf.org/2007/opf" version="3.0" '
        'unique-identifier="uid">'
        '<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">'
        '<dc:identifier id="uid">test</dc:identifier>'
        "<dc:title>t</dc:title></metadata>"
        "<manifest>"
        '<item id="front" href="front.xhtml" media-type="application/xhtml+xml"/>'
        '<item id="chap" href="chap.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest>"
        '<spine><itemref idref="front"/><itemref idref="chap"/></spine>'
        "</package>"
    )
    container = (
        '<?xml version="1.0" encoding="utf-8"?>'
        '<container version="1.0" '
        'xmlns="urn:oasis:names:tc:opendocument:xmlns:container">'
        '<rootfiles><rootfile full-path="OEBPS/content.opf" '
        'media-type="application/oebps-package+xml"/></rootfiles></container>'
    )
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", container)
        z.writestr("OEBPS/content.opf", opf)
        z.writestr("OEBPS/front.xhtml", _FRONT_DOC)
        z.writestr("OEBPS/chap.xhtml", _chapter_doc(pages))


def _annotate(path: Path, pages: list[int], monotone: bool):
    entries = build_cfi_map(str(path))
    pm = epub_pagelist.parse_page_map(str(path))
    assert pm["monotone"] is monotone, pm
    if monotone:
        epub_pagelist.annotate_cfi_entries(entries, pm["anchors"])
        for e in entries:
            if e.get("page") is not None:
                e["page_trust"] = pt.PRINT_VERIFIED if (
                    pages == sorted(pages)) else None
    return entries, pm


def _chunks():
    # production chunks always carry non-empty metadata (token_count,
    # section_titles) — the enrich path's `meta or {}` guard would swap an
    # EMPTY dict for a fresh one and silently drop the enrichment
    md = lambda: {"token_count": 8, "section_titles": []}
    return [
        {"text": "Unpaginated front matter with no print anchors",
         "metadata": md()},
        {"text": _PARA_A, "metadata": md()},
        {"text": _PARA_B, "metadata": md()},
        {"text": _PARA_C, "metadata": md()},
    ]


def test_wrapped_chapter_interior_folios(tmp_path):
    """Chapter 1 (pages 1-3): entry page = 1; interior chunks interpolate
    2 and 3 from the anchor run instead of collapsing onto the last page."""
    path = tmp_path / "book.epub"
    _write_epub(path, [1, 2, 3])
    entries, _ = _annotate(path, [1, 2, 3], monotone=True)

    chap = next(e for e in entries if e.get("spine") == 1)
    assert chap["page"] == 1, "chapter entry must open at its START page"
    assert [p for _, p in chap["anchor_offsets"]] == [1, 2, 3]

    chunks = _chunks()
    _enrich_epub_cfi_locators(chunks, entries)
    front, a, b, c = (ch["metadata"] for ch in chunks)
    # front matter: before the first anchor -> no page at all
    assert "page_start" not in front and "page_trust" not in front
    # chapter interior: real interpolated print pages
    assert a["page_start"] == 1 and a["page_trust"] == "print_verified"
    assert b["page_start"] == 2, "interior chunk must interpolate page 2"
    assert c["page_start"] == 3, "interior chunk must interpolate page 3"
    assert b["page_end"] >= 2


def test_non_monotone_map_stays_refused(tmp_path):
    """#226 counter-direction: pages 1, 5, 2 — the map refuses, annotate is
    never run (runner discipline), no chunk carries ANY page even though a
    wrapped chapter would offer an interpolation run."""
    path = tmp_path / "book.epub"
    _write_epub(path, [1, 5, 2])
    entries = build_cfi_map(str(path))
    pm = epub_pagelist.parse_page_map(str(path))
    assert pm["monotone"] is False
    # the runner only annotates on monotone+sanity+TOC-accepted maps; a
    # refused map leaves entries unannotated
    assert not any(e.get("page") is not None for e in entries)
    chunks = _chunks()
    _enrich_epub_cfi_locators(chunks, entries)
    for ch in chunks:
        m = ch["metadata"]
        assert "page_start" not in m and "page_trust" not in m, (
            "refused map must leave every chunk page-less (#226)")
        assert m.get("locator_type") == "epub_cfi"  # CFI still assigned


def test_interior_page_probe_semantics(tmp_path):
    """interior_page: probe before the first anchor -> None (caller keeps
    the chapter-start page); probe resolution is positional, not 'last'."""
    path = tmp_path / "book.epub"
    _write_epub(path, [1, 2, 3])
    entries, _ = _annotate(path, [1, 2, 3], monotone=True)
    chap = next(e for e in entries if e.get("spine") == 1)
    assert epub_pagelist.interior_page(chap, _PARA_B) == 2
    assert epub_pagelist.interior_page(chap, _PARA_C, tail=True) == 3
    # a probe that does not resolve returns None (never a guess)
    assert epub_pagelist.interior_page(chap, "x" * 60) is None


def test_cross_boundary_chunk_matches(tmp_path):
    """#234 layer 2: a chunk spanning a heading/paragraph boundary carries
    the boundary as whitespace (markdown \\n\\n), while the pre-fix entry
    text abutted nested blocks with NO separator — the 40-char probe died
    at every block boundary and interior chunks fell to the carry-forward
    fallback (no folio). The separator space fixes the probe."""
    path = tmp_path / "book.epub"
    _write_epub(path, [1, 2, 3])
    entries, _ = _annotate(path, [1, 2, 3], monotone=True)
    chunks = [{"text": "Chapter One\n\n" + _PARA_A,
               "metadata": {"token_count": 8, "section_titles": []}},
              {"text": _PARA_A[-120:] + "\n\n" + _PARA_B[:160],
               "metadata": {"token_count": 8, "section_titles": []}}]
    _enrich_epub_cfi_locators(chunks, entries)
    assert chunks[0]["metadata"]["page_trust"] == "print_verified"
    assert chunks[0]["metadata"]["page_start"] == 1
    # the second chunk CROSSES the A|B block boundary (anchor 2 sits
    # between them) — it must still match the chapter entry; it opens
    # inside A (page 1) and its tail interpolates into B (page 2)
    assert chunks[1]["metadata"].get("page_trust") == "print_verified"
    assert chunks[1]["metadata"]["page_start"] == 1
    assert chunks[1]["metadata"]["page_end"] == 2
