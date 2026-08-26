"""#220 Stage 1 — four-dialect EPUB page-anchor parser (synthetic fixtures).

Each fixture reproduces a REAL publisher shape from the #220 inventory
comment (Jossé class="page" 228 / Bieger id="page_N" 197 / Databricks
epub:type 406 / Adobe page-map.xml), shrunk to test size. The dialect
GRAMMAR is what is pinned here; the anchor counts of the real books are
regression targets for the #221 pilot, not for synthetic files.

Run: .venv/bin/python -m pytest tests/test_epub_pagelist.py
"""
from __future__ import annotations

import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_pagelist import (
    annotate_cfi_entries,
    parse_page_map,
)
from axiom_ng_runner.epub_cfi import build_cfi_map

_OPF = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="u">
 <metadata/><manifest>
  <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
  <item id="c2" href="c2.xhtml" media-type="application/xhtml+xml"/>
  <item id="map" href="page-map.xml" media-type="application/oebps-page-map+xml"/>
 </manifest>
 <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
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
            'xmlns:epub="http://www.idpf.org/2007/ops"><body>' + body +
            "</body></html>")


def _write_epub(path: Path, docs: dict[str, str], with_map: str | None = None,
                opf: str = _OPF) -> None:
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", opf)
        for name, body in docs.items():
            z.writestr(name, body)
        if with_map:
            z.writestr("page-map.xml", with_map)


def test_dialect_class_page_josse(tmp_path):
    """Jossé/dtv shape: inline <a class="page" id="page_N">N</a>."""
    epub = tmp_path / "josse.epub"
    body = "".join(
        f'<p><a class="page" id="page_{i}">{i}</a>Kostenrechnung Absatz {i} '
        f"mit hinreichend langem Text</p>"
        for i in range(1, 8)
    )
    _write_epub(epub, {"c1.xhtml": _doc(body), "c2.xhtml": _doc("<p>ohne Anker</p>")})
    m = parse_page_map(str(epub))
    assert m["count"] == 7
    assert m["monotone"] is True
    assert "class_page" in m["dialects"]
    assert [a["page"] for a in m["anchors"]] == list(range(1, 8))


def test_dialect_id_page_n_bieger(tmp_path):
    """Bieger/Springer shape: top-level <a id="page_N"/>[N] followers."""
    epub = tmp_path / "bieger.epub"
    parts = []
    for i in range(1, 7):
        parts.append(f'<a id="page_{i}"/>[{i}]')
        parts.append(f"<p>Management Studies Textblock Nummer {i}, lang genug</p>")
    _write_epub(epub, {"c1.xhtml": _doc("".join(parts))})
    m = parse_page_map(str(epub))
    assert m["count"] == 6
    assert "id_page_n" in m["dialects"]
    assert [a["page"] for a in m["anchors"]] == list(range(1, 7))
    # top-level anchors are their own element: elem 1,3,5,7,9,11
    assert [a["elem"] for a in m["anchors"]] == [1, 3, 5, 7, 9, 11]


def test_dialect_epub_type_pagebreak_databricks(tmp_path):
    """Databricks/ProQuest shape: <span epub:type="pagebreak" title="N"/>."""
    epub = tmp_path / "databricks.epub"
    body = "".join(
        f'<p><span epub:type="pagebreak" title="{i}"/>Lakehouse Kapiteltext '
        f"Nummer {i} der Plattform-Dokumentation</p>"
        for i in range(1, 9)
    )
    _write_epub(epub, {"c1.xhtml": _doc(body), "c2.xhtml": _doc("<p>Ende</p>")})
    m = parse_page_map(str(epub))
    assert m["count"] == 8
    assert "epub_type_pagebreak" in m["dialects"]
    assert m["monotone"] is True


def test_dialect_adobe_page_map(tmp_path):
    """Adobe page-map.xml: doc-start hrefs + fragment targets via element id."""
    epub = tmp_path / "adobe.epub"
    pmap = """<?xml version="1.0"?>
<page-map xmlns="http://www.idpf.org/2007/ops">
 <page-entry page-number="5" href="c1.xhtml"/>
 <page-entry page-number="33" href="c1.xhtml#p33"/>
 <page-entry page-number="60" href="c2.xhtml"/>
</page-map>
"""
    c1 = ('<p id="p33">Text ab Druckseite dreiunddreissig, ausreichend lang '
          "fuer den Test</p>")
    _write_epub(epub, {"c1.xhtml": _doc("<p>Kapitel eins Text</p>" + c1),
                       "c2.xhtml": _doc("<p>Kapitel zwei Text</p>")},
                with_map=pmap)
    m = parse_page_map(str(epub))
    assert m["count"] == 3
    assert "page_map_xml" in m["dialects"]
    assert [a["page"] for a in m["anchors"]] == [5, 33, 60]
    # fragment anchor sits on its element (elem 2), doc starts at elem 0
    assert [a["elem"] for a in m["anchors"]] == [0, 2, 0]
    assert [a["spine"] for a in m["anchors"]] == [0, 0, 1]


def test_no_anchors_returns_empty(tmp_path):
    epub = tmp_path / "bare.epub"
    _write_epub(epub, {"c1.xhtml": _doc("<p>Heaton: kein Anker im Buch</p>")})
    m = parse_page_map(str(epub))
    assert m["anchors"] == [] and m["count"] == 0


def test_non_monotone_map_flagged(tmp_path):
    """Never guess: a page-number DROP poisons the whole map."""
    epub = tmp_path / "broken.epub"
    body = "".join(
        f'<p><a class="page" id="page_{p}">{p}</a>Textblock {p} lang genug</p>'
        for p in (1, 2, 9, 4, 5)
    )
    _write_epub(epub, {"c1.xhtml": _doc(body)})
    m = parse_page_map(str(epub))
    assert m["count"] == 5
    assert m["monotone"] is False  # 9 -> 4 drop


def test_annotation_carries_pages_forward(tmp_path):
    """Pages apply from their anchor position forward, across spine docs."""
    epub = tmp_path / "annotate.epub"
    c1 = ('<p><a class="page" id="page_10">10</a>erster Text auf Seite zehn, '
          "genug Zeichen</p><p>zweiter Text noch auf Seite zehn</p>")
    c2 = "<p>kapitel zwei laeuft auf derselben Druckseite weiter</p>"
    _write_epub(epub, {"c1.xhtml": _doc(c1), "c2.xhtml": _doc(c2)})
    m = parse_page_map(str(epub))
    entries = build_cfi_map(str(epub))
    n = annotate_cfi_entries(entries, m["anchors"])
    assert n == len(entries) == 3
    assert {e["page"] for e in entries} == {10}
    assert entries[0]["cfi"] == "epubcfi(/6/2!/4/2)"
    assert entries[2]["spine"] == 1


def test_annotation_without_map_leaves_entries_bare(tmp_path):
    epub = tmp_path / "bare2.epub"
    _write_epub(epub, {"c1.xhtml": _doc("<p>kein Anker, kein Page</p>")})
    m = parse_page_map(str(epub))
    entries = build_cfi_map(str(epub))
    assert annotate_cfi_entries(entries, m["anchors"]) == 0
    assert "page" not in entries[0]


def test_annotation_no_backward_leak_into_earlier_spine_docs(tmp_path):
    """C1 regression: a page-map entry on the SECOND spine doc must not
    leak its page into the entries of the first doc (cover/TOC)."""
    epub = tmp_path / "leak.epub"
    pmap = ('<?xml version="1.0"?>\n<page-map '
            'xmlns="http://www.idpf.org/2007/ops">\n'
            '<page-entry page-number="10" href="c2.xhtml"/>\n'
            "</page-map>\n")
    _write_epub(epub, {"c1.xhtml": _doc("<p>Titelblatt ohne Anker</p>"),
                       "c2.xhtml": _doc("<p>Kapiteltext auf Seite zehn</p>")},
                with_map=pmap)
    m = parse_page_map(str(epub))
    assert m["anchors"] == [{"spine": 1, "elem": 0, "page": 10,
                             "cfi": "epubcfi(/6/4!/4)"}]
    entries = build_cfi_map(str(epub))
    n = annotate_cfi_entries(entries, m["anchors"])
    assert n == 1
    assert "page" not in entries[0]  # spine doc 0: no page (review C1)
    assert entries[1]["page"] == 10


def test_scanner_counts_top_level_void_elements(tmp_path):
    """W2 regression: top-level void elements consume an element step
    (parity with _CFICollector/foliate-js), and a page starting AFTER a
    block never annotates that earlier block."""
    epub = tmp_path / "void.epub"
    body = ("<p>Alpha-Text</p><hr/><p>Beta-Text</p><hr/>"
            '<p><a class="page" id="page_10">10</a>Gamma-Text</p>')
    _write_epub(epub, {"c1.xhtml": _doc(body)})
    m = parse_page_map(str(epub))
    assert [(a["elem"], a["page"]) for a in m["anchors"]] == [(5, 10)]
    entries = build_cfi_map(str(epub))
    annotate_cfi_entries(entries, m["anchors"])
    assert "page" not in entries[0]  # Alpha (elem 1)
    assert "page" not in entries[1]  # Beta (elem 3) — before the anchor
    assert entries[2]["page"] == 10  # Gamma (elem 5)


def test_pending_anchor_never_scavenges_later_text(tmp_path):
    """W3 regression: a numberless class="page" anchor that closes without
    a digit is DROPPED — following text with digits must not resolve it."""
    epub = tmp_path / "scavenge.epub"
    body = ('<p><span class="page"></span>vf</p>'
            "<p>Section 7 und weiter mit Text</p>")
    _write_epub(epub, {"c1.xhtml": _doc(body)})
    m = parse_page_map(str(epub))
    assert m["anchors"] == [] and m["count"] == 0
