"""#220 Stage 2 — mechanical EPUB repair toolbelt.

normalize_entry_paths is the W9/Z3 promotion (its original suite stays in
test_z3_epub_normalization.py and must keep passing through the re-export);
repair_spine and remove_entry_corpses are the new structural ops;
apply_repairs proves a repair via the preflight analyzer (red→green).

Run: .venv/bin/python -m pytest tests/test_epub_repair.py
"""
from __future__ import annotations

import zipfile
from pathlib import Path

from axiom_ng_runner.compute_core.epub_health import analyze_epub
from axiom_ng_runner.compute_core.epub_repair import (
    apply_repairs,
    normalize_entry_paths,
    remove_entry_corpses,
    repair_spine,
)

_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""

_CHAPTER = ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml">'
            "<body><p>" + "Kapiteltext mit ausreichend Inhalt zum Extrahieren. " * 30 +
            "</p></body></html>")


def _epub(path: Path, opf: str, extra: dict[str, str] | None = None) -> None:
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("content.opf", opf)
        z.writestr("c1.xhtml", _CHAPTER)
        z.writestr("c2.xhtml", _CHAPTER)
        for name, body in (extra or {}).items():
            z.writestr(name, body)


def test_repair_spine_synthesizes_from_manifest(tmp_path):
    """The preflight 'OPF/Spine fehlt' class: no <spine> → synthesize."""
    src = tmp_path / "nospine.epub"
    _epub(src, (
        '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
        'version="3.0"><metadata/><manifest>'
        '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
        '<item id="c2" href="c2.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest></package>"))
    assert analyze_epub(str(src))["opf_spine"] is False
    out = repair_spine(src, tmp_path)
    assert out != src
    assert analyze_epub(str(out))["opf_spine"] is True
    with zipfile.ZipFile(out) as z:
        opf = z.read("content.opf").decode()
    assert '<itemref idref="c1"/>' in opf and '<itemref idref="c2"/>' in opf


def test_repair_spine_fast_path_when_present(tmp_path):
    src = tmp_path / "ok.epub"
    _epub(src, (
        '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
        'version="3.0"><metadata/><manifest>'
        '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest><spine><itemref idref=\"c1\"/></spine></package>"))
    assert repair_spine(src, tmp_path) == src


def test_remove_entry_corpses(tmp_path):
    src = tmp_path / "corpses.epub"
    _epub(src, (
        '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
        'version="3.0"><metadata/><manifest>'
        '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest><spine><itemref idref=\"c1\"/></spine></package>"),
        extra={"junk/trailer.xhtml": "<html/>", "ads/leftover.xhtml": "<html/>"})
    out = remove_entry_corpses(src, tmp_path)
    assert out != src
    with zipfile.ZipFile(out) as z:
        names = set(z.namelist())
    assert "junk/trailer.xhtml" not in names
    assert "ads/leftover.xhtml" not in names
    assert {"mimetype", "META-INF/container.xml", "content.opf",
            "c1.xhtml"} <= names  # referenced entries survive


def test_remove_entry_corpses_fast_path_clean(tmp_path):
    src = tmp_path / "clean.epub"
    _epub(src, (
        '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
        'version="3.0"><metadata/><manifest>'
        '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
        '<item id="c2" href="c2.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest><spine><itemref idref=\"c1\"/></spine></package>"))
    assert remove_entry_corpses(src, tmp_path) == src


def test_normalize_promotion_matches_w9_behavior(tmp_path):
    """The promoted function IS the Z3 tool (same contract, new home)."""
    src = tmp_path / "bad.epub"
    with zipfile.ZipFile(src, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml",
                   '<container><rootfiles><rootfile full-path="OEBPS/content.opf"/>'
                   "</rootfiles></container>")
        z.writestr("OEBPS/content.opf",
                   '<manifest><item href="../Pkg/Text/Cover.xhtml" id="c"/></manifest>')
        z.writestr("Pkg/Text/Cover.xhtml", "<html><body>Cover</body></html>")
    out = normalize_entry_paths(src, tmp_path)
    assert out != src
    with zipfile.ZipFile(out) as z:
        opf = z.read("axiom_content.opf").decode()
        assert "../" not in opf
        assert "Pkg/Text/Cover.xhtml" in opf


def test_apply_repairs_red_to_green(tmp_path):
    """Broken spine + corpses: chained repair + preflight proof."""
    src = tmp_path / "broken.epub"
    _epub(src, (
        '<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
        'version="3.0"><metadata/><manifest>'
        '<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>'
        "</manifest></package>"),
        extra={"junk/trailer.xhtml": "<html/>"})
    assert analyze_epub(str(src))["opf_spine"] is False
    r = apply_repairs(src, tmp_path)
    assert "repair_spine" in r["applied"] and "remove_entry_corpses" in r["applied"]
    assert r["preflight"]["opf_spine"] is True
    assert r["preflight"]["verdacht"].startswith("🟢")
