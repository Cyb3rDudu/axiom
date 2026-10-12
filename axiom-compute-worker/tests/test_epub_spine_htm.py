"""#378 — EPUB spine detection must not guess by extension.

Wiley EPUBs name their content documents *.htm (three letters); the old
.xhtml/.html-only filter produced spine_docs=[] → text_layer=False and
flagged healthy books as broken/DRM. Fix: derive spine docs from the
OPF manifest+spine idrefs, with a .htm-aware extension fallback.

Run: .venv/bin/python -m pytest tests/test_epub_spine_htm.py
"""
from __future__ import annotations

import io
import zipfile

from axiom_compute_worker.compute_core.epub_health import analyze_epub

_TEXT = ("<html><body><p>"
         + "Price Action Trends Kapitel mit gut extrahierbarem Text. " * 20
         + "</p></body></html>")

_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="OEBPS/content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""


def _epub_file(tmp_path, opf: str, docs: dict[str, str]):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("OEBPS/content.opf", opf)
        for name, body in docs.items():
            z.writestr(name, body)
    path = tmp_path / "book.epub"
    path.write_bytes(buf.getvalue())
    return str(path)


def _opf(href: str) -> str:
    return f"""<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
 <metadata/><manifest>
  <item id="c1" href="{href}" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="c1"/></spine>
</package>
"""


def test_htm_spine_doc_has_text_layer(tmp_path):
    """Live-Beweis-Äquivalent (Wiley .htm, #378): healthy → text_layer=True."""
    path = _epub_file(tmp_path, _opf("c1.htm"), {"OEBPS/c1.htm": _TEXT})
    result = analyze_epub(path)
    assert result["text_layer"] is True
    label = result["label_befund"]
    assert "kein extrahierbarer Text" not in label
    assert result["finding"] != "🔴 defekt/DRM (epub)"


def test_manifest_derived_spine_survives_odd_extension(tmp_path):
    """Robustheit: OPF-spine-declared doc with no known extension passes."""
    path = _epub_file(tmp_path, _opf("c1.wiley"), {"OEBPS/c1.wiley": _TEXT})
    assert analyze_epub(path)["text_layer"] is True


def test_textless_epub_stays_red(tmp_path):
    """Negativkontrolle: echt textloses EPUB bleibt defekt."""
    empty = "<html><body><p>kurz</p></body></html>"
    path = _epub_file(tmp_path, _opf("c1.htm"), {"OEBPS/c1.htm": empty})
    result = analyze_epub(path)
    assert result["text_layer"] is False
    assert "kein extrahierbarer Text" in result["label_befund"]


def test_fallback_extension_filter_includes_htm(tmp_path):
    """Fallback-Pfad (OPF ohne spine idrefs): .htm zählt als Spine-Dokument."""
    opf = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
 <metadata/><manifest/><spine/>
</package>
"""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("OEBPS/content.opf", opf)
        z.writestr("OEBPS/c1.htm", _TEXT)
    path = tmp_path / "no_spine_refs.epub"
    path.write_bytes(buf.getvalue())
    assert analyze_epub(str(path))["text_layer"] is True


def test_manifest_href_parent_segments_resolves(tmp_path):
    """Regression (Review-Blocker, FFMTJA3S-Klasse): hrefs mit ../-Segmenten
    müssen auf den echten Zip-Eintrag normalisiert werden — kein KeyError."""
    opf = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
 <metadata/><manifest>
  <item id="c1" href="../Text/c1.xhtml" media-type="application/xhtml+xml"/>
  <item id="c2" href="../Text/c2.htm" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
</package>
"""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("OEBPS/content.opf", opf)
        z.writestr("Text/c1.xhtml", _TEXT)
        z.writestr("Text/c2.htm", _TEXT)
    path = tmp_path / "parent_segments.epub"
    path.write_bytes(buf.getvalue())
    result = analyze_epub(str(path))
    assert result["text_layer"] is True
    assert result["spine_docs"] == 2


def test_percent_encoded_hash_in_filename_resolves(tmp_path):
    """RFC 3986: Fragment-Trennung vor Percent-Decoding — ein literales #
    im Dateinamen (als %23 kodiert) muss nicht still aus der Spine fallen
    (Review-MINOR: False-Red ggü. origin/main)."""
    opf = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
 <metadata/><manifest>
  <item id="c1" href="Kapitel%20%231.htm" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="c1"/></spine>
</package>
"""
    path = _epub_file(tmp_path, opf, {"OEBPS/Kapitel #1.htm": _TEXT})
    result = analyze_epub(path)
    assert result["text_layer"] is True
    assert result["spine_docs"] == 1


def test_unresolvable_href_is_skipped_not_fatal(tmp_path):
    """href zeigt auf nicht vorhandenen Eintrag + Fragment-Suffix: Eintrag
    wird übersprungen, analysierbare Docs zählen trotzdem, kein Crash."""
    opf = """<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
 <metadata/><manifest>
  <item id="ghost" href="../Missing/ghost.xhtml" media-type="application/xhtml+xml"/>
  <item id="c1" href="c1.htm#c1anchor" media-type="application/xhtml+xml"/>
 </manifest>
 <spine><itemref idref="ghost"/><itemref idref="c1"/></spine>
</package>
"""
    path = _epub_file(tmp_path, opf, {"OEBPS/c1.htm": _TEXT})
    result = analyze_epub(path)
    assert result["text_layer"] is True
    assert result["spine_docs"] == 1
