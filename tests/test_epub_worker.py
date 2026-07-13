"""Tests for the EPUB import path (worker + converter predicates).

The conversion itself is exercised by spawning ``ai_researcher.epub_worker``
as a subprocess — the exact way the doc-processor invokes it — so the test
needs only the stdlib + the ``pandoc`` binary (no SQLAlchemy / pgvector /
GPU deps). Predicate tests import ``DocumentConverter`` lazily and skip if
that module's heavy import chain is unavailable in the current env.
"""
import json
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

import pytest

AXIOM_BACKEND = Path(__file__).resolve().parent.parent / "axiom_backend"
# So `from ai_researcher...` resolves when pytest runs from the repo root.
sys.path.insert(0, str(AXIOM_BACKEND))

PANDOC = shutil.which("pandoc")
needs_pandoc = pytest.mark.skipif(PANDOC is None, reason="pandoc binary not installed")

# A minimal 1×1 PNG so the image-extraction path has something to copy.
_PNG_BYTES = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000001000000010802000000907753de"
    "0000000c4944415408d76360f8cf000000ffff030005fe74d966a4000000004945"
    "4e44ae426082"
)


def _build_epub(path: Path) -> None:
    """Write a minimal valid EPUB3 with two chapters and one referenced image."""
    with zipfile.ZipFile(path, "w") as z:
        # mimetype must be the first entry and stored uncompressed.
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
        z.writestr(
            "OEBPS/ch1.xhtml",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Ch1</title></head>\n'
            # Stylistic <span> wrapper — pandoc leaks it into GFM; the worker
            # must strip it so headings come out clean.
            "<body><h1><span class=\"bold\">Chapter One</span></h1>"
            "<p>The quick brown fox.</p>\n"
            '<p><img src="cover.png" alt="cover"/></p></body></html>\n',
        )
        z.writestr(
            "OEBPS/ch2.xhtml",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Ch2</title></head>\n'
            "<body><h1>Chapter Two</h1><p>Jumps over the lazy dog.</p></body></html>\n",
        )
        z.writestr(
            "OEBPS/nav.xhtml",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">\n'
            '<body><nav epub:type="toc"><ol><li><a href="ch1.xhtml">Ch1</a></li>'
            "<li><a href=\"ch2.xhtml\">Ch2</a></li></ol></nav></body></html>\n",
        )
        z.writestr(
            "OEBPS/content.opf",
            '<?xml version="1.0" encoding="utf-8"?>\n'
            '<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">\n'
            '  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">\n'
            "    <dc:title>Smoke Book</dc:title>"
            '<dc:identifier id="bookid">urn:uuid:smoke</dc:identifier>\n'
            '    <dc:language>en</dc:language></metadata>\n'
            "  <manifest>\n"
            '    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>\n'
            '    <item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>\n'
            '    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>\n'
            '    <item id="cover" href="cover.png" media-type="image/png"/>\n'
            "  </manifest>\n"
            "  <spine><itemref idref=\"ch1\"/><itemref idref=\"ch2\"/></spine></package>\n",
        )


@pytest.fixture
def sample_epub(tmp_path: Path) -> Path:
    p = tmp_path / "sample.epub"
    _build_epub(p)
    return p


def _run_worker(epub: Path, out_md: Path, out_img: Path) -> subprocess.CompletedProcess:
    """Invoke epub_worker the same way the doc-processor does."""
    return subprocess.run(
        [sys.executable, "-m", "ai_researcher.epub_worker", str(epub), str(out_md), str(out_img)],
        cwd=str(AXIOM_BACKEND),
        capture_output=True,
        text=True,
    )


def _last_json(lines: str) -> dict:
    for line in reversed(lines.splitlines()):
        line = line.strip()
        if line.startswith("{") and line.endswith("}"):
            return json.loads(line)
    raise AssertionError(f"no JSON line in: {lines!r}")


@needs_pandoc
def test_worker_converts_epub_to_markdown(sample_epub: Path, tmp_path: Path) -> None:
    out_md = tmp_path / "out.md"
    out_img = tmp_path / "images" / "doc1"

    proc = _run_worker(sample_epub, out_md, out_img)
    assert proc.returncode == 0, f"worker failed: {proc.stderr}"

    result = _last_json(proc.stdout)
    assert result["ok"] is True
    assert result["markdown_path"] == str(out_md)

    markdown = out_md.read_text(encoding="utf-8")
    assert "Chapter One" in markdown
    assert "Chapter Two" in markdown
    # Stylistic span/div wrappers must be stripped (clean heading, no raw HTML).
    assert "# Chapter One" in markdown
    assert "<span" not in markdown
    assert "<div" not in markdown

    # The referenced cover image should be extracted + renamed image_N.ext.
    assert result["image_mapping"], "expected cover.png to be extracted"
    saved_name = result["image_mapping"]["cover.png"]
    assert saved_name.startswith("image_")
    assert (out_img / saved_name).exists()


@needs_pandoc
def test_worker_corrupt_epub_fails(tmp_path: Path) -> None:
    broken = tmp_path / "broken.epub"
    broken.write_text("this is not a zip file")
    proc = _run_worker(broken, tmp_path / "out.md", tmp_path / "images" / "doc1")
    assert proc.returncode != 0
    err = _last_json(proc.stderr)
    assert err["ok"] is False
    assert "error" in err


# --- Converter predicate tests ---------------------------------------------
# Import lazily: ai_researcher.core_rag pulls SQLAlchemy/pgvector at package
# load, which may be absent in a stripped env. Skip if so.

@pytest.fixture(scope="module")
def converter():
    try:
        from ai_researcher.core_rag.document_converter import DocumentConverter
    except Exception as exc:  # pragma: no cover - env-dependent
        pytest.skip(f"DocumentConverter import unavailable: {exc}")
    return DocumentConverter()


def test_is_epub_predicate(converter) -> None:
    assert converter.is_epub_file("book.epub")
    assert converter.is_epub_file("BOOK.EPUB")
    assert not converter.is_epub_file("book.pdf")
    assert not converter.is_epub_file("book.mobi")


def test_is_supported_format_includes_epub(converter) -> None:
    assert converter.is_supported_format("novel.epub")
    assert converter.is_supported_format("paper.pdf")
    assert not converter.is_supported_format("book.mobi")
