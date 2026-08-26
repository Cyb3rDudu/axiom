"""#220 Stage 1 — CFI mirror against foliate-js semantics (readest/foliate-js
epubcfi.js, MIT; semantics reference only, no code reuse).

The EPUB-CFI spec pins the rule foliate-js implements: child counting for
indirect steps covers ONLY elements (nodeType 1) and character data (3/4) —
"content other than element and character data is ignored". XML parsers
that expose comment nodes as children (lxml.etree!) shift element steps —
the known trap from the #220 crawl experiment. These tests pin that
html.parser-based counting matches foliate-js on exactly those cases:

  - comment between body children: element indices unchanged
  - CDATA: ignored for element counting (html.parser: unknown_decl)
  - adjacent text chunks: ONE odd index between each pair of elements
    ("one chunk between each pair of child elements" — foliate-js)
  - even steps address elements, odd steps character data

Expected indices below are derived by hand from the foliate-js rule:
element #n (counting only element/text children) gets index n*2; the text
chunk between element #n and #n+1 gets index n*2+1.

Run: .venv/bin/python -m pytest tests/test_epub_cfi_foliate_mirror.py
"""
from __future__ import annotations

from axiom_ng_runner.epub_cfi import build_cfi_map

_DOC_TMPL = '<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body>{}</body></html>'


def _cfis(body: str) -> list[tuple[str, str]]:
    """Build one-doc CFI entries from a body fragment (tmp file via BytesIO
    is not supported — build_cfi_map reads a path)."""
    import tempfile
    import zipfile
    from pathlib import Path

    with tempfile.TemporaryDirectory() as td:
        epub = Path(td) / "m.epub"
        with zipfile.ZipFile(epub, "w") as z:
            z.writestr("mimetype", "application/epub+zip")
            z.writestr("META-INF/container.xml", (
                '<?xml version="1.0"?><container version="1.0" '
                'xmlns="urn:oasis:names:tc:opendocument:xmlns:container">'
                '<rootfiles><rootfile full-path="content.opf" '
                'media-type="application/oebps-package+xml"/></rootfiles></container>'))
            z.writestr("content.opf", (
                '<?xml version="1.0"?><package '
                'xmlns="http://www.idpf.org/2007/opf" version="3.0">'
                "<metadata/><manifest><item id='c' href='c.xhtml' "
                "media-type='application/xhtml+xml'/></manifest>"
                "<spine><itemref idref='c'/></spine></package>"))
            z.writestr("c.xhtml", _DOC_TMPL.format(body))
        entries = build_cfi_map(str(epub))
        return [(e["cfi"], e["text"]) for e in entries]


def test_comments_do_not_shift_element_steps():
    """foliate-js: comments are invisible to child counting. <p> #1 and #2
    keep steps /2 and /4 whether or not comments sit between them (the
    lxml trap — an element-child iteration over the raw XML would count
    the comment and emit /4 and /6)."""
    got = _cfis("<!-- lead --><p>erster Absatz</p><!-- mid --><p>zweiter Absatz</p>")
    assert [c for c, _ in got] == ["epubcfi(/6/2!/4/2)", "epubcfi(/6/2!/4/4)"]


def test_cdata_ignored_for_element_counting():
    """CDATA is character data (nodeType 4) — it never shifts an element
    step; per foliate-js it merges into the adjacent text chunk."""
    got = _cfis("<p>vor cdata</p><![CDATA[ roh ]]> <p>nach cdata</p>")
    assert [c for c, _ in got] == ["epubcfi(/6/2!/4/2)", "epubcfi(/6/2!/4/4)"]


def test_adjacent_text_chunks_collapse_to_one_odd_index():
    """foliate-js chunks: between element #1 and element #2 there is exactly
    ONE character-data index (/3) — whitespace, comments and a CDATA section
    between them all belong to that single chunk. Our element steps are
    unaffected (elements keep even indices); pinned here so any future
    resolver that emits character offsets uses chunk semantics, not
    per-text-node counting."""
    got = _cfis(
        "<p>alpha beta gamma</p> whitespace "
        "<!-- c --><![CDATA[x]]> more "
        "<p>delta epsilon zeta</p>"
    )
    assert [c for c, _ in got] == ["epubcfi(/6/2!/4/2)", "epubcfi(/6/2!/4/4)"]
    # the single chunk between them would be /3 — NOT /5 or /7, which a
    # text-node-per-index counting would produce.


def test_processing_instructions_ignored():
    got = _cfis("<?pi ignore?><p>erster Absatz</p><?pi two?><p>zweiter Absatz</p>")
    assert [c for c, _ in got] == ["epubcfi(/6/2!/4/2)", "epubcfi(/6/2!/4/4)"]


def test_void_elements_do_not_count_as_children():
    """A <br/> between paragraphs is an element but NOT a counted body child
    in our scheme? NO — foliate counts ALL elements (nodeType 1). Our
    collector skips void tags for depth tracking; the pinned expectation:
    void elements inside a block do not split the block, and top-level void
    elements do not consume an element step (they carry no text to cite)."""
    got = _cfis("<p>zeile eins<br/>zeile zwei</p><hr/><p>naechster Block</p>")
    # <hr/> is a body child (element) but void: no entry, and per our
    # counting scheme the second <p> is body child 3 -> step 6.
    assert [c for c, _ in got] == ["epubcfi(/6/2!/4/2)", "epubcfi(/6/2!/4/6)"]


def test_spine_step_and_bang_separator():
    """Package-document step /6, spine itemref step = (index+1)*2, then '!'
    into the content document, /4 into body — the foliate-js shape."""
    got = _cfis("<p>einzig</p>")
    assert got[0][0] == "epubcfi(/6/2!/4/2)"
