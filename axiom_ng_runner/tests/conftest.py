"""Pytest support for the axiom_ng_runner black-box contract suite.

Makes ``axiom_ng_runner`` and the existing ``ai_researcher`` cores importable,
then stands up an isolated work root + source root per session.
"""

from __future__ import annotations

import sys
import zipfile
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AXIOM_BACKEND = REPO_ROOT / "axiom_backend"

for _p in (str(REPO_ROOT), str(AXIOM_BACKEND)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

# A 1x1 PNG for EPUB image content.
_PNG_BYTES = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000001000000010802000000907753de"
    "0000000c4944415408d76360f8cf000000ffff030005fe74d966a4000000004945"
    "4e44ae426082"
)


def build_epub(path: Path, chapters: int = 2) -> Path:
    """Write a minimal valid EPUB3 fixture with real HTML chapters."""
    with zipfile.ZipFile(path, "w") as z:
        z.writestr(
            zipfile.ZipInfo("mimetype"),
            "application/epub+zip",
            compress_type=zipfile.ZIP_STORED,
        )
        z.writestr(
            "META-INF/container.xml",
            '<?xml version="1.0"?>\n'
            '<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">\n'
            '  <rootfiles><rootfile full-path="OEBPS/content.opf" '
            'media-type="application/oebps-package+xml"/></rootfiles>\n'
            "</container>\n",
        )
        z.writestr("OEBPS/cover.png", _PNG_BYTES)
        for i in range(1, chapters + 1):
            z.writestr(
                f"OEBPS/ch{i}.xhtml",
                '<?xml version="1.0" encoding="utf-8"?>\n'
                '<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Ch'
                f"{i}</title></head><body>"
                f"<h1>Chapter {i}</h1><p>The quick brown fox jumps. Chapter-number-{i}."
                " A NamedEntity and AnotherThing appear.</p></body></html>\n",
            )
        z.writestr(
            "OEBPS/nav.xhtml",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">\n'
            '<body><nav epub:type="toc"><ol>'
            + "".join(
                f'<li><a href="ch{i}.xhtml">Ch{i}</a></li>'
                for i in range(1, chapters + 1)
            )
            + "</ol></nav></body></html>\n",
        )
        z.writestr(
            "OEBPS/content.opf",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">\n'
            '  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">'
            "<dc:title>Smoke Book</dc:title>"
            '<dc:identifier id="bookid">urn:uuid:smoke</dc:identifier></metadata>\n'
            '<manifest><item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" '
            'properties="nav"/>'
            + "".join(
                f'<item id="ch{i}" href="ch{i}.xhtml" media-type="application/xhtml+xml"/>'
                for i in range(1, chapters + 1)
            )
            + "</manifest><spine>"
            + "".join(f'<itemref idref="ch{i}"/>' for i in range(1, chapters + 1))
            + "</spine></package>\n",
        )
    return path


def build_pdf(path: Path, pages: int = 3) -> Path:
    """Write a tiny deterministic PDF with numbered pages and a heading."""
    import pymupdf

    doc = pymupdf.open()
    for i in range(pages):
        page = doc.new_page()
        page.insert_text((72, 90), f"Chapter One\nPage detail {i}", fontsize=12)
        page.insert_text(
            (72, 120), f"Entity PDFThing{i} and AnotherEntity{i}.", fontsize=11
        )
    doc.save(str(path))
    doc.close()
    return path


@pytest.fixture(scope="session")
def fixture_dirs(tmp_path_factory):
    root = tmp_path_factory.mktemp("axiom_runner_fixtures")
    sources = root / "sources"
    work = root / "work"
    sources.mkdir(parents=True, exist_ok=True)
    work.mkdir(parents=True, exist_ok=True)

    pdf = build_pdf(sources / "smoke.pdf", pages=3)
    epub = build_epub(sources / "smoke.epub", chapters=2)
    return {"sources": sources, "work": work, "pdf": pdf, "epub": epub}


@pytest.fixture(scope="session")
def processed_fixture_dirs(tmp_path_factory):
    """A pooled dir the whole suite's source files live in; cleaned in teardown."""
    root = tmp_path_factory.mktemp("axiom_runner_processed")
    return root


@pytest.fixture(scope="session", autouse=True)
def _avoids_cross_pollution():
    yield


def requires_pymupdf():
    try:
        import pymupdf  # noqa: F401

        return True
    except ImportError:
        return False
