"""#233 — locator_backfill alignment engine tests.

Covers (all with synthetic, deterministic EPUB+PDF fixtures built on the fly;
no D'heur corpus needed, <= pytest with pymupdf on the light CI runner):

  - PDF path: physical_page_start -> derived print page via four-point
    anchors; a known physical page maps to the expected print page (the
    "reference page" behaviour, D'heur p.176 analog): a chunk whose physical
    page holds the known reference content lands exactly on that print page
    (±0 in the controlled fixture).
  - refusal, never guess: a chunk with no physical_page_start keeps `none`;
    a chunk beyond the anchored range stays refused.
  - already-folio chunks (page_source=folio_verified) are left untouched
    (no downgrade).
  - whole-backfill refusal: a NON-monotone candidate page map refuses the
    entire backfill (aligned=False).
  - EPUB (CFI) path: a monotone candidate carries print pages onto chunks
    whose text matches the candidate's CFI entries.
  - idempotency by construction: re-running the engine yields an identical
    plan.
"""
from __future__ import annotations

import zipfile
from pathlib import Path

import pytest
from axiom_ng_runner.compute_core.locator_backfill import (
    align_epub_chunks,
    align_pdf_chunks,
    backfill_chunks,
)
from conftest import requires_pymupdf

# ---------------------------------------------------------------------------
# synthetic fixture: EPUB with one chapter of "page sections", PDF whose
# physical page p holds page-section p's text (print page == physical page).
# ---------------------------------------------------------------------------

def _doc(body: str) -> str:
    return ('<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml" '
            'xmlns:epub="http://www.idpf.org/2007/ops"><body>' + body +
            "</body></html>")


def _section_tokens(n: int) -> str:
    """~130 distinct tokens unique to page-section n, so a 900-char harvest
    window (~120 tokens) after the marker stays entirely inside section n's
    text and the containing PDF page matches with high overlap."""
    return " ".join(f"folg{n}w{m}" for m in range(130))


def _mk_epub(path: Path, npages: int = 10, poison: int | None = None) -> Path:
    """A monotone (or poisoned) injected-style EPUB: one spine doc whose body
    is page-marker + section-text per page. ``poison`` inserts a drop so the
    map is non-monotone (page K then page K-3). Pagebreaks carry an ``id`` so
    four_point_pilot.harvest_epub locates each window position (the #222
    injector writes ``id="PBn"`` too)."""
    blocks = []
    for i in range(1, npages + 1):
        page = i if poison is None or i != poison else i - 3
        # same pagebreak shape the #222 injector writes (inline xmlns:epub,
        # role=doc-pagebreak, aria-label, id) so harvest_epub numbers AND
        # positions it correctly
        blocks.append(
            f'<span xmlns:epub="http://www.idpf.org/2007/ops" '
            f'epub:type="pagebreak" role="doc-pagebreak" id="PB{i}" '
            f'title="{page}" aria-label="{page}"/>'
            f'<p>{_section_tokens(i)}</p>')
    opf = ('<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" '
           'version="3.0" unique-identifier="bi"><metadata '
           'xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>T</dc:title>'
           '<dc:identifier id="bi">u</dc:identifier></metadata>'
           '<manifest><item id="c1" href="c1.xhtml" '
           'media-type="application/xhtml+xml"/></manifest>'
           '<spine><itemref idref="c1"/></spine></package>')
    with zipfile.ZipFile(path, "w") as z:
        z.writestr("mimetype", "application/epub+zip")
        z.writestr("META-INF/container.xml",
                   ('<?xml version="1.0"?><container version="1.0" '
                    'xmlns="urn:oasis:names:tc:opendocument:xmlns:container">'
                    '<rootfiles><rootfile full-path="c1.opf" '
                    'media-type="application/oebps-package+xml"/></rootfiles></container>'))
        z.writestr("c1.opf", opf)
        z.writestr("c1.xhtml", _doc("".join(blocks)))
    return path


def _mk_pdf(path: Path, npages: int = 10, front: int = 0) -> Path:
    """Physical pages whose text is exactly the page-section text. ``front``
    prepends unnumbered pages so physical_page_start p's print page is p - front.
    Uses insert_textbox (wrapping) so all ~130 section tokens survive the PDF
    round-trip (a bare insert_text clips to the first line)."""
    # pi-lens-ignore: reportMissingImports
    import pymupdf

    rect = pymupdf.Rect(50, 50, 550, 700)
    doc = pymupdf.open()
    for i in range(front):
        doc.new_page().insert_textbox(rect, f"frontmatter {i}", fontsize=9)
    for i in range(1, npages + 1):
        doc.new_page().insert_textbox(rect, _section_tokens(i), fontsize=9)
    doc.save(str(path))
    doc.close()
    return path


def _folioless_chunks(n: int, front: int = 0) -> list[dict]:
    """Active-snapshot chunks with page_source=none. Physical pages are
    0-BASED (the stored convention: section i lives on physical i-1) —
    matching what the real pipeline writes (contract §11)."""
    return [
        {"id": f"chunk-{i}", "text": f"body text {_section_tokens(i)}",
         "locator": {"type": "page_span", "page_source": "none",
                     "physical_page_start": i - 1 + front,
                     "physical_page_end": i - 1 + front}}
        for i in range(1, n + 1)
    ]


# ---------------------------------------------------------------------------
# PDF path
# ---------------------------------------------------------------------------

@pytest.fixture(scope="module")
def pdf_pairs(tmp_path_factory):
    if not requires_pymupdf():
        pytest.skip("pymupdf not available")
    root = tmp_path_factory.mktemp("locator_backfill")
    epub = _mk_epub(root / "book.epub", npages=10)
    pdf = _mk_pdf(root / "book.pdf", npages=10, front=0)
    return epub, pdf


def test_pdf_path_enriches_reference_page(pdf_pairs):
    """The four-point PDF path maps physical page -> derived print page. The
    reference page (physical 7 = print 7) gets an enriched, confident seat."""
    epub, pdf = pdf_pairs
    chunks = _folioless_chunks(10)
    res = align_pdf_chunks(str(epub), str(pdf), chunks)
    s = res.summary()
    assert s["aligned"] is True
    assert s["anchor_count"] >= 4
    assert s["pages_enriched"] >= 1
    # physical page 7 -> print page 7 (identity map in this fixture)
    hit = next(c for c in res.chunk_results if c.to_dict()["page_start"] == 7)
    d = hit.to_dict()
    assert d["enrich"] is True
    assert d["page_start"] == 7
    assert d["source"] == "derived_from_sibling"
    assert d["refused"] is False


def test_pdf_path_refuses_nophysical(chunk_pdf_pair):
    epub, pdf, chunks = chunk_pdf_pair
    chunks[0]["locator"].pop("physical_page_start", None)
    res = align_pdf_chunks(str(epub), str(pdf), chunks)
    d = res.chunk_results[0].to_dict()
    assert d["refused"] is True
    assert "no physical" in d["reason"]


def test_pdf_path_leaves_already_folio_chunks_untouched(pdf_pairs):
    """A chunk already carrying folio_verified must not be downgraded /
    re-stamped."""
    epub, pdf = pdf_pairs
    chunks = _folioless_chunks(3)
    chunks[1]["locator"]["page_source"] = "folio_verified"
    chunks[1]["locator"]["page_label_start"] = "99"
    res = align_pdf_chunks(str(epub), str(pdf), chunks)
    d = res.chunk_results[1].to_dict()
    assert d["enrich"] is False          # not a backfill target
    assert d["refused"] is False         # and not a refusal — just untouched
    # its existing physical range is reported through unchanged; the mocked
    # folio label survives untouched
    assert d["page_start"] == 1          # 0-based physical_page_start echoed
    assert chunks[1]["locator"]["page_label_start"] == "99"
    # the other two (folio-less) still enrich
    assert res.chunk_results[0].to_dict()["enrich"] is True
    assert res.chunk_results[2].to_dict()["enrich"] is True


def test_pdf_path_refuses_non_monotone_candidate(tmp_path):
    """A poisoned (non-monotone) candidate page map refuses the WHOLE
    backfill before any chunk is processed (#226 discipline)."""
    epub = _mk_epub(tmp_path / "bad.epub", npages=10, poison=8)
    pdf = _mk_pdf(tmp_path / "bad.pdf", npages=10)
    res = align_pdf_chunks(str(epub), str(pdf), _folioless_chunks(3))
    assert res.aligned is False
    assert res.chunk_results == []
    assert res.refused_reason


@pytest.fixture(scope="module")
def chunk_pdf_pair(pdf_pairs):
    epub, pdf = pdf_pairs
    return epub, pdf, _folioless_chunks(10)


def test_confidence_refusal_for_out_of_range(pdf_pairs):
    """A chunk whose physical page is far beyond the anchored range refuses
    (never guess): physical page 1000 is outside physical 1..10."""
    epub, pdf = pdf_pairs
    chunks = _folioless_chunks(10)
    chunks[5]["locator"]["physical_page_start"] = 1000
    chunks[5]["locator"]["physical_page_end"] = 1000
    res = align_pdf_chunks(str(epub), str(pdf), chunks)
    d = res.chunk_results[5].to_dict()
    # either refused by confidence or by "outside anchored range" — never enriched
    assert d["refused"] is True


def test_pdf_path_refuses_any_target_below_min_conf(pdf_pairs):
    epub, pdf = pdf_pairs
    chunks = [{"id": "z", "text": "zzzz zzzz zzzz",   # no overlap with any page
               "locator": {"type": "page_span", "page_source": "none",
                           "physical_page_start": 5, "physical_page_end": 5}}]
    res = align_pdf_chunks(str(epub), str(pdf), chunks)
    d = res.chunk_results[0].to_dict()
    assert d["refused"] is True  # text-overlap below the floor


def test_idempotent_by_construction(pdf_pairs):
    epub, pdf = pdf_pairs
    chunks = _folioless_chunks(10)
    r1 = align_pdf_chunks(str(epub), str(pdf), chunks)
    r2 = align_pdf_chunks(str(epub), str(pdf), chunks)
    assert [c.to_dict() for c in r1.chunk_results] == \
           [c.to_dict() for c in r2.chunk_results]


# ---------------------------------------------------------------------------
# EPUB (CFI) path
# ---------------------------------------------------------------------------

def test_epub_path_enriches_from_candidate_map(tmp_path):
    """The candidate's monotone page map carries print pages onto chunks whose
    text matches its CFI entries."""
    epub = _mk_epub(tmp_path / "book.epub", npages=4)
    chunks = [
        {"id": "e1", "text": f"matching text {_section_tokens(2)}",
         "locator": {"type": "epub_cfi", "page_source": "none",
                     "cfi_start": "epubcfi(/6/2!/4/4)"}},
    ]
    res = align_epub_chunks(str(epub), chunks)
    assert res.aligned is True
    assert res.pages_enriched == 1
    d = res.chunk_results[0].to_dict()
    assert d["enrich"] is True
    assert d["page_start"] == 2
    assert d["source"] == "derived_from_sibling"


def test_epub_path_refuses_non_monotone_candidate(tmp_path):
    epub = _mk_epub(tmp_path / "bad.epub", npages=6, poison=5)
    res = align_epub_chunks(str(epub), [
        {"id": "e1", "text": "x", "locator": {"page_source": "none"}}])
    assert res.aligned is False
    assert res.refused_reason


# ---------------------------------------------------------------------------
# dispatcher & help text
# ---------------------------------------------------------------------------

def test_backfill_chunks_dispatches_pdf_and_epub(pdf_pairs, tmp_path):
    epub, pdf = pdf_pairs
    chunks = _folioless_chunks(3)
    r = backfill_chunks(str(epub), "pdf", str(pdf), chunks)
    assert r.aligned is True
    # no pdf_path -> honest refusal, not a panic
    r2 = backfill_chunks(str(epub), "pdf", None, chunks)
    assert r2.aligned is False
