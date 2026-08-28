"""#223 — printed-TOC verification of EPUB page anchors.

Book-internal proof that markers are print folios: the printed TOC's page
numbers joined against the chapter-start anchors. Verdicts verified /
divergent / no_toc feed the page_source split print_verified vs
print_unverified; divergent (reader-pagination markers — the ProQuest
drift class) must be DETECTED, never trusted.

Synthetic fixtures reproduce the real shapes: a frontmatter TOC doc with
dot-leader entries + trailing page numbers (href-carrying and plain), and
a divergent book whose markers drift +6 per the crawl evidence.

Run: .venv/bin/python -m pytest tests/test_epub_toc_verification.py
"""
from __future__ import annotations

import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_pagelist import (
    _TOCScanner,
    parse_page_map,
    sanity_check,
    verify_print_folios,
)

_OPF = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="u">
 <metadata/><manifest>
  <item id="toc" href="toc.xhtml" media-type="application/xhtml+xml"/>
  <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
  <item id="c2" href="c2.xhtml" media-type="application/xhtml+xml"/>
  <item id="c3" href="c3.xhtml" media-type="application/xhtml+xml"/>
  <item id="c4" href="c4.xhtml" media-type="application/xhtml+xml"/>
 </manifest>
 <spine>
  <itemref idref="toc"/><itemref idref="c1"/><itemref idref="c2"/>
  <itemref idref="c3"/><itemref idref="c4"/>
 </spine>
</package>
"""
_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""


def _doc(body: str) -> str:
    return ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml" '
            "><body>" + body + "</body></html>")


def _chapter(page: int) -> str:
    return (f'<p><a class="page" id="page_{page}">{page}</a>Kapitelanfang auf '
            f"Druckseite {page}, gefolgt von ausreichend Text</p>"
            "<p>zweiter Absatz desselben Kapitels</p>")


def _toc_doc(entries: list[tuple[str, int, str | None]], drift: int = 0) -> str:
    parts = []
    for title, page, href in entries:
        link_open = f'<a href="{href}">' if href else ""
        link_close = "</a>" if href else ""
        parts.append(f"<p>{link_open}{title}{link_close} . . . {page + drift}</p>")
    return _doc("".join(parts))


def _write(tmp_path: Path, name: str, toc: str, chapters: list[str]) -> Path:
    epub = tmp_path / name
    with zipfile.ZipFile(epub, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", _OPF)
        z.writestr("toc.xhtml", toc)
        for i, body in enumerate(chapters, start=1):
            z.writestr(f"c{i}.xhtml", _doc(body))
    return epub


_CHAPTER_PAGES = [7, 45, 88, 130]
_CHAPTERS = [_chapter(p) for p in _CHAPTER_PAGES]
_TOC = [
    ("1 Grundlagen der Kostenrechnung", 7, "c1.xhtml"),
    ("2 Kostenartenrechnung im Detail", 45, "c2.xhtml"),
    ("3 Kostenstellen und Kostenträger", 88, "c3.xhtml"),
    ("4 Deckungsbeitragsrechnung", 130, "c4.xhtml"),
]


def test_verified_toc_matches_markers(tmp_path):
    """Printed TOC page numbers == chapter-start anchors → verified."""
    epub = _write(tmp_path, "verified.epub", _toc_doc(_TOC), _CHAPTERS)
    m = parse_page_map(str(epub))
    assert m["monotone"]
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "verified"
    assert v["joins"] == 4 and v["matched"] == 4 and v["offset"] == 0


def test_divergent_reader_pagination_detected(tmp_path):
    """The ProQuest class: TOC says 7/45/88/130, markers drift +6 — reader
    pagination must be DETECTED (divergent, offset reported), not trusted."""
    epub = _write(tmp_path, "divergent.epub", _toc_doc(_TOC), [
        _chapter(p + 6) for p in _CHAPTER_PAGES
    ])
    m = parse_page_map(str(epub))
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "divergent"
    assert v["offset"] == 6
    assert v["matched"] == 0


def test_no_toc_without_frontmatter_toc(tmp_path):
    """No printable TOC (nav-only book, Heaton class) → no_toc, honest."""
    chapters = [_chapter(p) for p in _CHAPTER_PAGES]
    empty_toc = _doc("<p>Vorwort ohne Inhaltsverzeichnis</p>")
    epub = _write(tmp_path, "notoc.epub", empty_toc, chapters)
    m = parse_page_map(str(epub))
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "no_toc"
    assert v["joins"] == 0


def test_plain_toc_without_links_joins_via_titles(tmp_path):
    """Printed TOC without hrefs still joins when the EPUB3 nav doc carries
    the same titles."""
    epub = _write(
        tmp_path, "plainlinks.epub",
        _toc_doc([(t, p, None) for t, p, _ in _TOC]), _CHAPTERS)
    # add a nav doc referenced nowhere in the spine — _find_nav scans names
    with zipfile.ZipFile(epub, "a") as z:
        z.writestr("nav.xhtml", _doc(
            "<nav epub:type='toc'><ol>" + "".join(
                f'<li><a href="{h}">{t}</a></li>' for t, _, h in _TOC
            ) + "</ol></nav>"))
    m = parse_page_map(str(epub))
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "verified"
    assert v["joins"] >= 3


def test_sanity_guards():
    ok = sanity_check([{"page": p} for p in range(1, 40)])
    assert ok["ok"], ok
    # too few anchors
    assert not sanity_check([{"page": p} for p in (1, 2)])["ok"]
    # implausible numbering start (arabic body starting at 500)
    assert not sanity_check([{"page": p} for p in range(500, 540)])["ok"]
    # implausible range: 10 anchors spanning 5000 pages
    assert not sanity_check(
        [{"page": p * 500} for p in range(1, 11)])["ok"]


def _dense_pagebreak_doc(pages: list[int]) -> str:
    """Chapter doc with dense epub:type="pagebreak" anchors (Databricks
    shape): one anchor per print page listed."""
    parts = []
    for p in pages:
        parts.append(
            f'<p><span epub:type="pagebreak" title="{p}"/>Abschnittstext auf '
            f"Druckseite {p} mit ausreichend Inhalt zum Zitieren</p>")
    return _doc("".join(parts))


def test_hierarchical_toc_joins_intra_chapter_anchors(tmp_path):
    """Review C1: a hierarchical printed TOC (chapters + subsections) must
    join each entry against the target doc's FULL anchor set — subsection
    pages hit intra-chapter markers exactly → verdict verified. The old
    first-anchor-only join mis-verdicted this fixture as divergent."""
    chapter_pages = {1: [7, 12, 18], 2: [45, 51], 3: [88, 95], 4: [130, 137]}
    toc = [
        ("1 Grundlagen", 7, "c1.xhtml"),
        ("1.1 Begriffe", 12, "c1.xhtml"),
        ("1.2 Systematik", 18, "c1.xhtml"),
        ("2 Verfahren", 45, "c2.xhtml"),
        ("2.1 Schritte", 51, "c2.xhtml"),
        ("3 Praxis", 88, "c3.xhtml"),
        ("3.1 Fälle", 95, "c3.xhtml"),
        ("4 Ausblick", 130, "c4.xhtml"),
        ("4.1 Fazit", 137, "c4.xhtml"),
    ]
    epub = _write(
        tmp_path, "hierarchical.epub", _toc_doc(toc),
        [_dense_pagebreak_doc(chapter_pages[i]) for i in range(1, 5)])
    m = parse_page_map(str(epub))
    assert m["monotone"]
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "verified", v
    assert v["joins"] == 9 and v["matched"] == 9


def test_hierarchical_toc_with_chapter_drift_still_divergent(tmp_path):
    """C1 counterpart: chapter-start drift (reader pagination) must stay
    detectable — no anchor in the target doc matches, so every entry
    offsets by the chapter-start drift (+6) → divergent, offset reported."""
    chapter_pages = {1: [13, 19, 25], 2: [51, 57, 63], 3: [94, 100, 106],
                     4: [136, 142, 148]}
    toc = [
        ("1 Grundlagen", 7, "c1.xhtml"),
        ("2 Verfahren", 45, "c2.xhtml"),
        ("3 Praxis", 88, "c3.xhtml"),
        ("4 Ausblick", 130, "c4.xhtml"),
    ]
    epub = _write(
        tmp_path, "drift.epub", _toc_doc(toc),
        [_dense_pagebreak_doc(chapter_pages[i]) for i in range(1, 5)])
    m = parse_page_map(str(epub))
    v = verify_print_folios(str(epub), m["anchors"])
    assert v["verdict"] == "divergent", v
    assert v["matched"] == 0
    assert v["offset"] == 6


def test_wrapped_toc_yields_exact_entries():
    """Review W2: a TOC wrapped in <div><ol><li>...</li></ol></div> must
    yield exactly N distinct entries — closing the wrapper div must not
    re-emit the last li's content (the old scanner duplicated it)."""
    raw = _doc(
        "<div><ol>"
        "<li><a href='c1.xhtml'>1 Erstes Kapitel</a> . . . 5</li>"
        "<li><a href='c2.xhtml'>2 Zweites Kapitel</a> . . . 9</li>"
        "<li><a href='c3.xhtml'>3 Drittes Kapitel</a> . . . 14</li>"
        "</ol></div>")
    scanner = _TOCScanner()
    scanner.feed(raw)
    assert [e["page"] for e in scanner.entries] == [5, 9, 14]
    assert [e["href"] for e in scanner.entries] == [
        "c1.xhtml", "c2.xhtml", "c3.xhtml"]


def test_sanity_sparse_chapter_anchored_map_passes():
    """Review W4: a 350-page book with 11 chapter anchors is plausible
    (old ratio 8 false-refused it); a 10-anchor map spanning 5000 pages
    still refuses."""
    sparse = [1, 33, 64, 98, 122, 155, 187, 212, 249, 301, 350]
    assert sanity_check([{"page": p} for p in sparse])["ok"]
    assert not sanity_check(
        [{"page": p * 500} for p in range(1, 11)])["ok"]
