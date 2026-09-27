"""#222 — pure-function checks for four_point_pilot.py (no pymupdf needed:
`import fitz` lives inside map_pdf_folios, never at module level).

Covers the folio/anchor machinery the twins sweep depends on:
  - _roman incl. 'm' (KeyError 'm' crashed the first sweep run)
  - _norm_path for manifest hrefs escaping with ../ (Jossé EPUB)
  - _anchor_number dialect table (id_page_n yes, non-numeric ids no,
    class_page needs digit text)
  - _assign_folios roman-seed guard (band word 'M' must not seed a roman
    sequence) and the arabic re-seed after >=10 starved candidate pages
"""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from xml.etree import ElementTree as ET

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "four_point_pilot.py"

_spec = importlib.util.spec_from_file_location("four_point_pilot", SCRIPT)
assert _spec is not None and _spec.loader is not None
fpp = importlib.util.module_from_spec(_spec)
sys.modules["four_point_pilot"] = fpp
_spec.loader.exec_module(fpp)


def _el(tag: str, text: str | None = None, **attrs) -> ET.Element:
    el = ET.Element(tag)
    for k, v in attrs.items():
        el.set(fpp.EPUB_TYPE if k == "epub_type" else k, v)
    if text is not None:
        el.text = text
    return el


def test_roman_incl_m() -> None:
    assert fpp._roman("i") == 1
    assert fpp._roman("xiv") == 14
    assert fpp._roman("m") == 1000  # crashed the first sweep run
    assert fpp._folio_token("M") == -1000
    assert fpp._folio_token("30") == 30
    assert fpp._folio_token("Größe") is None


def test_norm_path_escapes() -> None:
    assert fpp._norm_path("OEBPS", "../x.xhtml") == "x.xhtml"
    assert fpp._norm_path("OEBPS", "text/a.xhtml") == "OEBPS/text/a.xhtml"
    assert fpp._norm_path("", "a.xhtml") == "a.xhtml"


def test_anchor_number_dialects() -> None:
    a = _el("a", id="page_12")
    assert fpp._anchor_number(a) == (12, "id_page_n")
    # non-numeric / non-page ids must not match
    assert fpp._anchor_number(_el("span", id="pagebreak-note")) is None
    assert fpp._anchor_number(_el("span", id="page-title")) is None
    # class="page" + digit text
    c = _el("span", text="12", **{"class": "page"})
    assert fpp._anchor_number(c) == (12, "class_page")
    # epub:type pagebreak with aria-label
    s = _el("span", id="PB17", **{"epub_type": "pagebreak",
                                  "aria-label": "17"})
    assert fpp._anchor_number(s) == (17, "epub3_pagebreak")


def _page(n: int, cands, empty: bool = False) -> dict:
    return {"physical": n, "cands": cands, "tokens": [], "empty": empty}


def test_assign_folios_roman_seed_guard() -> None:
    # a lone band word 'M' (-1000) must not seed a roman sequence;
    # the next page's real roman folio v (-5) seeds it instead
    pages = fpp._assign_folios([
        _page(1, [(-1000, 0)]),      # junk 'M'
        _page(2, [(-5, 0)]),         # real roman seed
        _page(3, [(-6, 0)]),         # continues vi
        _page(4, []),                # unnumbered
    ])
    assert pages[0]["folio"] is None and pages[0]["class"] == "unnumbered"
    assert pages[1]["folio"] == -5 and pages[1]["class"] == "roman"
    assert pages[2]["folio"] == -6
    assert pages[3]["class"] == "unnumbered"


def test_assign_folios_arabic_reseed() -> None:
    raw = [_page(1, [(1, 0)]), _page(2, [(2, 0)]), _page(3, [(3, 0)])]
    # 10 starved pages whose candidates are far off-sequence ...
    for k in range(10):
        raw.append(_page(4 + k, [(400 + k, 0)]))
    # ... then a plausible new-sequence candidate must re-seed
    raw.append(_page(14, [(500, 0)]))
    raw.append(_page(15, [(501, 0)]))
    pages = fpp._assign_folios(raw)
    assert [p["folio"] for p in pages[:3]] == [1, 2, 3]
    assert all(p["folio"] is None for p in pages[3:13])  # starved, unnumbered
    assert pages[13]["folio"] == 500 and pages[13]["class"] == "arabic"
    assert pages[14]["folio"] == 501
