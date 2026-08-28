"""#226 F1/F2 hardening — marker canonicalization, tail-trim, marker merge.

Pins the worker's dialect canonicalization (W1: class token semantics),
the pagelist's restart-tail trim (W2 precondition) and the runner's
marker-bounds merge gate (W2: never mix numbering spaces).

Run: .venv/bin/python -m pytest tests/test_epub_marker_canonicalization.py
"""
from __future__ import annotations

import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_pagelist import parse_page_map
from axiom_ng_runner.compute_core.epub_worker.__main__ import (
    _canonicalize_page_anchors,
    _marker_canonicalized_copy,
)
from axiom_ng_runner.runner import _merge_marker_pages

_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""

_OPF = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="u">
 <metadata/><manifest>
  <item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="c1"/></spine>
</package>
"""


def _doc(body: str) -> str:
    return ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml" '
            'xmlns:epub="http://www.idpf.org/2007/ops"><body>' + body +
            "</body></html>")


def _write_epub(path: Path, docs: dict[str, str]) -> None:
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", _OPF)
        for name, body in docs.items():
            z.writestr(name, body)


# --- (a) all canonicalization shapes -------------------------------------


def test_dialect_epub_type_pagebreak():
    """Native Apress + #222-injected: epub:type="pagebreak", number in id."""
    out = _canonicalize_page_anchors(
        '<p><span id="PB176" epub:type="pagebreak" role="doc-pagebreak"></span>'
        "Text der Seite</p>"
    )
    assert '<span id="axiom_page_176"></span>' in out
    assert "PB176" not in out


def test_dialect_class_page_token_semantics():
    """Jossé class="page": matched; class="page-num" NOT (W1)."""
    out = _canonicalize_page_anchors(
        '<p><a class="page-num" id="x">45</a>Text</p>'
    )
    assert out == '<p><a class="page-num" id="x">45</a>Text</p>'
    out = _canonicalize_page_anchors(
        '<p><a class="first page" id="page_45">45</a>Text</p>'
    )
    assert '<span id="axiom_page_45"></span>' in out


def test_dialect_id_page_self_closing_and_paired():
    """Bieger id="page_N": self-closing and paired-empty, [N] echo dropped."""
    out = _canonicalize_page_anchors('<a id="page_12"/>[12]<p>Text</p>')
    assert '<span id="axiom_page_12"></span><p>Text</p>' == out
    out = _canonicalize_page_anchors('<a id="page_13"></a><p>Text</p>')
    assert '<span id="axiom_page_13"></span><p>Text</p>' == out


def test_dialect_single_quoted_attributes():
    """Single-quoted attrs canonicalize identically (cheap parity)."""
    out = _canonicalize_page_anchors(
        "<p><a class='page' id='page_7'>7</a>Text</p>"
        "<a id='page_8'/>[8]"
    )
    assert '<span id="axiom_page_7"></span>' in out
    assert '<span id="axiom_page_8"></span>' in out


# --- (b) roman frontmatter untouched -------------------------------------


def test_roman_frontmatter_anchors_not_canonicalized():
    """title="ii" carries no digit — the anchor stays as-is."""
    raw = '<span id="pgii" epub:type="pagebreak" title="ii"></span>'
    assert _canonicalize_page_anchors(raw) == raw


# --- (c) copy fast-path ---------------------------------------------------


def test_marker_canonicalized_copy_fast_path(tmp_path):
    """No anchors anywhere -> the original path is returned unchanged."""
    epub = tmp_path / "plain.epub"
    _write_epub(epub, {"c1.xhtml": _doc("<p>keine Anker</p>")})
    (tmp_path / "media").mkdir()
    out = _marker_canonicalized_copy(epub, tmp_path / "media")
    assert out == epub  # same object: fast path, no rewritten copy


def test_marker_canonicalized_copy_rewrites(tmp_path):
    """Anchors present -> a rewritten copy comes back."""
    epub = tmp_path / "anchored.epub"
    _write_epub(
        epub, {"c1.xhtml": _doc('<p><a class="page" id="page_3">3</a>Text</p>')}
    )
    (tmp_path / "media").mkdir()
    out = _marker_canonicalized_copy(epub, tmp_path / "media")
    assert out != epub
    with zipfile.ZipFile(out) as z:
        assert 'id="axiom_page_3"' in z.read("c1.xhtml").decode()


# --- (d) restart tail trim ------------------------------------------------


def _bieger_doc(pages: list[int]) -> str:
    return "".join(f'<a id="page_{p}"/>[{p}]<p>Text {p}</p>' for p in pages)


def test_restart_tail_trimmed(tmp_path):
    """19 monotone anchors + restarted 20th -> prefix kept, tail counted."""
    epub = tmp_path / "tail.epub"
    _write_epub(epub, {"c1.xhtml": _doc(_bieger_doc(list(range(1, 20)) + [5]))})
    m = parse_page_map(str(epub))
    assert m["count"] == 19
    assert m["monotone"] is True
    assert m["restart_tail_trimmed"] == 1


def test_restart_trim_boundary_exactly_90_percent(tmp_path):
    """#226 review pin: the trim rule is >= (prefix LENGTH), so a map that
    is exactly 90% monotone (18 of 20) trims — cut vs cut+1 arithmetic."""
    epub = tmp_path / "boundary.epub"
    parts = []
    for i in range(1, 19):  # 18 ascending
        parts.append(f'<p><a class="page" id="page_{i}">{i}</a>Text {i} lang genug</p>')
    for p_ in (5, 6):  # restarted appendix tail (a DROP below 18)
        parts.append(f'<p><a class="page" id="page_{p_}">{p_}</a>Anhang {p_}</p>')
    _write_epub(epub, {"c1.xhtml": _doc("".join(parts))})
    m = parse_page_map(str(epub))
    assert m["monotone"] is True
    assert m["count"] == 18
    assert m["restart_tail_trimmed"] == 2

def test_early_drop_stays_non_monotone(tmp_path):
    """Drop after 3 of 20 anchors (15% prefix) -> refused, nothing trimmed."""
    epub = tmp_path / "early.epub"
    pages = [1, 2, 3, 1] + list(range(4, 20))  # drop at anchor 4
    _write_epub(epub, {"c1.xhtml": _doc(_bieger_doc(pages))})
    m = parse_page_map(str(epub))
    assert m["count"] == 20
    assert m["monotone"] is False
    assert m["restart_tail_trimmed"] == 0


# --- W2: marker-bounds merge gate -----------------------------------------


def test_merge_marker_pages_applies_without_trim():
    """Untrimmed map: marker bounds ride along and sharpen the envelope."""
    meta = {"page_start": 186, "page_end": 190,
            "paragraph_pages": [["0", "175"], ["1603", "176"]]}
    _merge_marker_pages(meta, None)
    assert meta["page_start"] == 175
    assert meta["page_end"] == 176
    assert meta["epub_paragraph_pages"] == [["0", "175"], ["1603", "176"]]


def test_merge_marker_pages_within_trimmed_max():
    """Trimmed map, labels inside the trusted range -> override applies."""
    meta = {"page_start": 400, "page_end": 405,
            "paragraph_pages": [["0", "402"], ["900", "404"]]}
    _merge_marker_pages(meta, 448)
    assert meta["page_start"] == 402
    assert meta["epub_paragraph_pages"][0] == ["0", "402"]


def test_merge_marker_pages_drops_restarted_labels():
    """Trimmed map, labels beyond the trusted max -> cfi envelope kept,
    no paragraph_pages (never mix numbering spaces under one stamp)."""
    meta = {"page_start": 440, "page_end": 448,
            "paragraph_pages": [["0", "447"], ["900", "450"]]}
    _merge_marker_pages(meta, 448)
    assert meta["page_start"] == 440
    assert meta["page_end"] == 448
    assert "epub_paragraph_pages" not in meta


def test_merge_marker_pages_noop_without_prereqs():
    """No trusted envelope or no marker bounds -> nothing happens."""
    meta = {"paragraph_pages": [["0", "5"]]}
    _merge_marker_pages(meta, None)
    assert "page_start" not in meta and "epub_paragraph_pages" not in meta
    meta = {"page_start": 5, "page_end": 5}
    _merge_marker_pages(meta, None)
    assert meta["page_start"] == 5 and "epub_paragraph_pages" not in meta
