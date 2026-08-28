"""#222 — twins_sweep derive+inject contract suite.

Synthetic EPUB (umlaut text on purpose): anchor two folios against the
token stream, inject a working copy, and assert the three invariants the
sweep's round-trip verification depends on:
  1. page-map.xml is an actual archive member (was silently dropped while
     the OPF kept referencing it), hrefs resolve to the marker ids;
  2. inline pagebreak spans land before their anchor token, inside the
     same text node, and re-harvest as epub3_pagebreak markers;
  3. the source EPUB is byte-identical afterwards — originals are never
     written.
No network, no Zotero, no library file is ever touched.
"""
from __future__ import annotations

import importlib.util
import sys
import zipfile
from pathlib import Path

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "twins_sweep.py"

_spec = importlib.util.spec_from_file_location("twins_sweep", SCRIPT)
assert _spec is not None and _spec.loader is not None
ts = importlib.util.module_from_spec(_spec)
sys.modules["twins_sweep"] = ts
_spec.loader.exec_module(ts)


def build_epub(path: Path) -> None:
    """One chapter, German text with umlauts before both anchor tokens
    (byte-vs-char offset hazard for the raw-offset splicing)."""
    html = (
        '<?xml version="1.0" encoding="utf-8"?>\n'
        '<html xmlns="http://www.w3.org/1999/xhtml">'
        "<head><title>t</title></head><body>"
        "<p>Erklärung über Nachhaltigkeit und Verantwortung. Größe "
        "Unternehmen führen.</p>"
        "<p>Zweiter Absatz mit Kommunikation und Beteiligung.</p>"
        "</body></html>"
    ).encode()
    opf = (
        b'<?xml version="1.0"?>\n'
        b'<package xmlns="http://www.idpf.org/2007/opf" version="3.0" '
        b'unique-identifier="u"><metadata xmlns:dc="http://purl.org/dc/'
        b'elements/1.1/"><dc:title>Synth</dc:title><dc:identifier id="u">'
        b"x</dc:identifier></metadata><manifest><item id=\"c1\" "
        b'href="text/c1.xhtml" media-type="application/xhtml+xml"/>'
        b"</manifest><spine><itemref idref=\"c1\"/></spine></package>"
    )
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip",
                   compress_type=zipfile.ZIP_STORED)
        z.writestr("content.opf", opf)
        z.writestr("text/c1.xhtml", html)


def test_inject_writes_page_map_and_spares_original(tmp_path: Path) -> None:
    src = tmp_path / "src.epub"
    build_epub(src)
    src_bytes = src.read_bytes()

    stream, files, _opf = ts.epub_token_stream(src)
    idx_grosse = next(i for i, t in enumerate(stream)
                      if t[0].startswith("gross"))
    idx_komm = next(i for i, t in enumerate(stream) if t[0].startswith("kommun"))
    dst = tmp_path / "dst.injected.epub"
    res = ts.inject_pagelist(src, dst, [{"folio": 7, "stream_idx": idx_grosse},
                                        {"folio": 8, "stream_idx": idx_komm}])

    # 3. original never written
    assert src.read_bytes() == src_bytes
    assert src.exists() and dst.exists()

    with zipfile.ZipFile(dst) as z:
        names = z.namelist()
        # 1. page-map is a member and its hrefs resolve to the marker ids
        assert res["page_map"] in names, "page-map.xml missing from zip"
        pm = z.read(res["page_map"]).decode()
        assert '<page name="7" href="text/c1.xhtml#PB7"/>' in pm
        assert '<page name="8" href="text/c1.xhtml#PB8"/>' in pm
        opf = z.read("content.opf").decode()
        assert 'name="page-map"' in opf and 'id="pagemap"' in opf
        # #226: explicit provenance meta — the runner reads this to stamp
        # derived_from_sibling (shape detection is impossible: injected
        # anchors mimic the native publisher format byte-for-byte).
        assert '<meta name="axiom-page-source" content="derived_from_sibling"/>' in opf
        # 2. spans before their anchor token, inside the same text node
        xhtml = z.read("text/c1.xhtml").decode()
        assert xhtml.index("PB7") < xhtml.index("Größe") < xhtml.index("</p>")
        assert xhtml.index("PB8") < xhtml.index("Kommunikation")

    markers, meta = ts.harvest_epub(dst, set(ts.norm_tokens(ts.BOILERPLATE)))
    assert [m["marker_page"] for m in markers] == [7, 8]
    assert meta["dialects"] == ["epub3_pagebreak"]
