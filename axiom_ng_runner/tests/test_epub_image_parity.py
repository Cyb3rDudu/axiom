"""#274 — EPUB/PDF image pipeline parity.

Corpus evidence (2026-09-15): every EPUB in the library had 0 image-marker
chunks while image artifacts existed. Root cause, reproduced with pandoc
3.7: publisher EPUBs wrap figures in <figure>/<figcaption> and pandoc's
GFM writer keeps those blocks as RAW HTML — the chunker only recognizes
``![alt](path)``, so every figure-wrapped image vanished from the text
flow. The worker now inlines HTML images as markdown markers (#274) and
rewrites every reference to its stable ``image_<N>.<ext>`` name.

This suite is the dev deliverable of #274:
- worker units (inlining, ref rewrite, duplicate-basename safety)
- an end-to-end worker run on a fixture EPUB (pandoc-gated)
- the PARITY GATE: an equivalent image-bearing fixture through the
  EPUB leg (real pandoc worker output) and the PDF leg (marker-style
  markdown, the shape pdf_worker emits — pinned by the existing suites)
  must yield EQUIVALENT linkage through the shared runner seam
  (chunker → image_refs normalization → artifacts → figure captions).

Run: .venv/bin/python -m pytest tests/test_epub_image_parity.py
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

import pytest
from axiom_ng_runner.compute_core.chunker import Chunker
from axiom_ng_runner.compute_core.epub_worker.__main__ import (
    _inline_html_images,
    _rewrite_image_refs,
    _save_extracted_images,
)
from axiom_ng_runner.runner import (
    _collect_image_artifacts,
    _drop_link_refs,
    _extract_figure_captions,
)

_PANDOC = shutil.which("pandoc")

_CONTAINER = """<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
 <rootfiles><rootfile full-path="OEBPS/content.opf"
  media-type="application/oebps-package+xml"/></rootfiles>
</container>
"""

# 1x1 PNG — valid, tiny, deterministic.
_PNG = bytes.fromhex(
    "89504e470d0a1a0a0000000d4948445200000001000000010802000000907753de"
    "0000000c4944415408d76360f8cf000000ffff030005fe74d966a4000000004945"
    "4e44ae426082"
)


def _opf(items: str, spine: str) -> str:
    return (
        '<?xml version="1.0"?>\n'
        '<package xmlns="http://www.idpf.org/2007/opf" version="3.0" '
        'unique-identifier="u">\n'
        ' <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">'
        "<dc:title>Parity</dc:title>"
        '<dc:identifier id="u">urn:uuid:parity</dc:identifier></metadata>\n'
        f" <manifest>{items}</manifest>\n <spine>{spine}</spine>\n</package>"
    )


def build_epub(
    path: Path, chapters: dict[str, str], images: dict[str, bytes]
) -> Path:
    """Write a minimal valid EPUB3: chapters map {href: xhtml-body-html},
    images map {zip-path: bytes} (also declared in the OPF manifest)."""
    items = "".join(
        f'<item id="c{i}" href="{href}" media-type="application/xhtml+xml"/>'
        for i, href in enumerate(chapters)
    )
    spine = "".join(
        f'<itemref idref="c{i}"/>' for i in range(len(chapters))
    )
    media = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
        ".svg": "image/svg+xml",
    }
    img_items = "".join(
        f'<item id="m{i}" href="{p}" media-type="{media[Path(p).suffix]}"/>'
        for i, p in enumerate(images)
    )
    with zipfile.ZipFile(path, "w") as z:
        z.writestr(zipfile.ZipInfo("mimetype"), "application/epub+zip")
        z.writestr("META-INF/container.xml", _CONTAINER)
        z.writestr("OEBPS/content.opf", _opf(items + img_items, spine))
        for href, body in chapters.items():
            z.writestr(
                f"OEBPS/{href}",
                '<?xml version="1.0" encoding="utf-8"?>\n'
                '<html xmlns="http://www.w3.org/1999/xhtml">'
                f"<body>{body}</body></html>",
            )
        for img_path, data in images.items():
            z.writestr(f"OEBPS/{img_path}", data)
    return path


# The shared corpus forms: publisher <figure> and lecture2epub-style bare
# <img> inside a div (pandoc converts the latter itself; both must yield
# markers after the worker pass).
_PUBLISHER_BODY = (
    "<h1>Kapitel 1</h1>"
    "<p>Erster Absatz vor der Abbildung mit genug Text zum Chunken.</p>"
    '<figure><img src="../images/fig1.png" alt="Abb. 1: Umsatz"/>'
    "<figcaption>Abb. 1: Umsatzentwicklung im Zeitverlauf</figcaption></figure>"
    "<p>Zweiter Absatz nach der Abbildung mit weiterem Fliesstext.</p>"
)
_LECTURE_BODY = (
    "<h1>Kapitel 1</h1>"
    "<p>Erster Absatz vor der Abbildung mit genug Text zum Chunken.</p>"
    '<div class="img_container"><img src="../images/fig1.png" alt="Abb. 1: Umsatz"/></div>'
    "<p>Abb. 1: Umsatzentwicklung im Zeitverlauf</p>"
    "<p>Zweiter Absatz nach der Abbildung mit weiterem Fliesstext.</p>"
)

# ponytail: SVG-wrapped figures (<svg><image …>) are not inlined —
# corpus census found 1 inline SVG (a titlepage cover) across 175 books;
# revisit only if a corpus book actually ships SVG body figures.


# ── worker units ─────────────────────────────────────────────────────────


class TestInlineHtmlImages:
    def test_figure_becomes_marker_plus_caption_line(self):
        out = _inline_html_images(
            '<figure><img src="media/images/f1.png" alt="Abb. 1"/>'
            "<figcaption>Abb. 1: <em>Umsatz</em></figcaption></figure>"
        )
        assert "![Abb. 1](media/images/f1.png)" in out
        assert "Abb. 1: Umsatz" in out          # caption as plain text
        assert "<figure" not in out and "<img" not in out

    def test_bare_img_becomes_marker(self):
        out = _inline_html_images('<p>x <img src="a/b.png" alt="B"> y</p>')
        assert "![B](a/b.png)" in out and "<img" not in out

    def test_remote_and_data_uri_imgs_dropped(self):
        out = _inline_html_images(
            '<img src="https://x.example/a.png" alt="r"/>'
            '<img src="data:image/png;base64,AAAA" alt="d"/>'
        )
        assert "![" not in out and "<img" not in out

    def test_markdown_images_untouched(self):
        md = "![alt](media/f.png) text"
        assert _inline_html_images(md) == md

    def test_remote_img_figure_keeps_caption_drops_html(self):
        """W4: remote-only figure — caption survives as text, no raw HTML."""
        out = _inline_html_images(
            '<figure><img src="https://x.example/a.png" alt="r"/>'
            "<figcaption>Abb. 9: nur online</figcaption></figure>"
        )
        assert "Abb. 9: nur online" in out
        assert "<figure" not in out and "<img" not in out

    def test_empty_remote_figure_collapses(self):
        """Remote img, no caption: nothing usable — the wrapper must not
        leak raw <figure></figure> into chunk text (review #274 NIT)."""
        out = _inline_html_images(
            '<figure><img src="https://x.example/a.png" alt="r"/></figure>'
        )
        assert "<figure" not in out and "<img" not in out and out.strip() == ""

    def test_figure_residual_text_preserved(self):
        """BLOCKER pin (review #274, Sonko exemplar): text inside a figure
        that is neither <img> nor <figcaption> — Springer-style
        <div class="TextObject"><p>…</p></div> image descriptions — must
        survive as chunk text, not be discarded by the figure replace."""
        out = _inline_html_images(
            '<figure><img src="media/images/f1.png" alt="Venn"/>'
            '<div class="TextObject"><p>A chart presents a Venn diagram.'
            "</p></div></figure>"
        )
        assert "![Venn](media/images/f1.png)" in out
        assert "A chart presents a Venn diagram." in out
        assert "<figure" not in out and "<div" not in out

    def test_caption_entities_decoded(self):
        """NIT pin: &amp; and friends decode to real characters."""
        out = _inline_html_images(
            '<figure><img src="f.png"/>'
            "<figcaption>R&amp;D Ausgaben</figcaption></figure>"
        )
        assert "R&D Ausgaben" in out and "&amp;" not in out

    def test_fenced_code_blocks_untouched(self):
        """MAJOR pin (review #274 P7): a literal HTML sample inside a
        fenced code block must NOT gain a phantom image marker — the
        marker's ref has no artifact and would terminally fail the
        snapshot persist gate."""
        md = (
            "Text davor.\n\n```html\n"
            '<figure><img src="../images/fig1.png" alt="sample"/></figure>\n'
            "```\n\nText danach."
        )
        out = _inline_html_images(md)
        assert out == md
        # and the ref rewrite skips fences too
        ref_index = {"../images/fig1.png": "image_0.png"}
        assert _rewrite_image_refs(out, ref_index) == md
        # sanity: the SAME html outside a fence IS inlined
        plain = '<figure><img src="../images/fig1.png" alt="sample"/></figure>'
        assert "![sample](../images/fig1.png)" in _inline_html_images(plain)

    def test_data_src_attribute_not_matched(self):
        """706ffe boundary pin: data-src/data-alt are different attributes
        — no marker, no src confusion."""
        out = _inline_html_images('<img data-src="nope.png" data-alt="x"/>')
        assert out == ""

    def test_markdown_title_ref_path_only(self):
        """MINOR pin (review #274 P2): pandoc emits ![alt](path "title")
        for <img title> — the rewrite must touch the path, keep the title.
        A title swallowed into the ref would miss the lookup and die at
        the persist gate."""
        out = _rewrite_image_refs(
            '![alt](media/images/fig1.png "Titeltext")',
            {"media/images/fig1.png": "image_0.png"},
        )
        assert out == '![alt](image_0.png "Titeltext")'
        assert "<figure" not in out and "<img" not in out

    def test_figure_without_caption_and_multi_image_figure(self):
        out = _inline_html_images("<figure><img src='f.png'/></figure>")
        assert "![](f.png)" in out
        out = _inline_html_images(
            "<figure><img src='a.png'/><img src='b.png'/>"
            "<figcaption>Teil a und b</figcaption></figure>"
        )
        assert "![](a.png)" in out and "![](b.png)" in out
        assert "Teil a und b" in out


class TestSaveAndRewrite:
    def test_nested_and_duplicate_basenames_resolve_by_full_path(self, tmp_path):
        media = tmp_path / "media"
        (media / "ch1").mkdir(parents=True)
        (media / "ch2").mkdir(parents=True)
        (media / "ch1" / "fig.png").write_bytes(_PNG)
        (media / "ch2" / "fig.png").write_bytes(_PNG)
        (media / "unique.jpg").write_bytes(_PNG)

        mapping, ref_index = _save_extracted_images(media, tmp_path / "out")
        # mapping keys are the saved names (runner basename lookup contract)
        assert sorted(mapping) == ["image_0.png", "image_1.png", "image_2.jpg"]
        # full-path resolution: each dir-local ref maps to its own image
        assert ref_index["ch1/fig.png"] == "image_0.png"
        assert ref_index["ch2/fig.png"] == "image_1.png"

        md = f"![a](ch2/fig.png) und ![b]({media}/ch2/fig.png) und ![c](unique.jpg)"
        out = _rewrite_image_refs(md, ref_index)
        assert "![a](image_1.png)" in out   # relative form resolved exactly
        # the unresolved-absolute form is what pandoc writes with
        # --extract-media=<abs dir> (macOS symlinked temp dirs make
        # resolve()-derived keys miss — W2)
        assert "![b](image_1.png)" in out
        # basename fallback is first-wins: an unindexed ch2-shaped ref must
        # never resolve to the ch1 duplicate
        assert "![a](image_0.png)" not in out
        assert "![c](image_2.jpg)" in out

    def test_unresolvable_ref_left_untouched(self, tmp_path):
        media = tmp_path / "media"
        media.mkdir()
        _, ref_index = _save_extracted_images(media, tmp_path / "out")
        assert _rewrite_image_refs("![x](nope.png)", ref_index) == "![x](nope.png)"


# ── end-to-end worker + runner seam (the EPUB leg) ───────────────────────


def _run_worker(epub: Path, out_md: Path, out_img: Path) -> dict:
    proc = subprocess.run(
        [sys.executable, "-m", "axiom_ng_runner.compute_core.epub_worker",
         str(epub), str(out_md), str(out_img)],
        capture_output=True, text=True, check=False,
        cwd=str(Path(__file__).resolve().parents[2]),
    )
    assert proc.returncode == 0, proc.stderr[-500:]
    return json.loads(proc.stdout.strip().splitlines()[-1])


def _linkage(markdown: str, images_dir: Path, image_mapping: dict):
    """The shared runner seam: chunker → refs → artifacts → fig captions.
    Mirrors _real_pipeline's post-conversion steps exactly (the parity
    both formats must satisfy)."""
    chunks = Chunker(max_chunk_tokens=1200).chunk(
        markdown, doc_metadata={"doc_id": "parity"}
    )
    artifacts, orig_to_ref = _collect_image_artifacts(
        images_dir, image_mapping, images_dir.parent
    )
    for c in chunks:
        meta = c["metadata"]
        raw = _drop_link_refs(meta.get("image_refs", []))
        norm = []
        for r in raw:
            orig = str(r.get("path", "")) if isinstance(r, dict) else str(r)
            norm.append(orig_to_ref.get(Path(orig).name, orig))
        meta["image_refs"] = norm
    _extract_figure_captions(
        chunks, {ref: orig for orig, ref in orig_to_ref.items()}
    )
    return chunks, artifacts


@pytest.mark.skipif(_PANDOC is None, reason="pandoc not on PATH")
class TestEpubWorkerEndToEnd:
    @pytest.mark.parametrize("body", [_PUBLISHER_BODY, _LECTURE_BODY],
                             ids=["publisher-figure", "lecture2epub-img"])
    def test_markers_refs_and_captions(self, tmp_path, body):
        epub = build_epub(
            tmp_path / "book.epub",
            {"text/c1.xhtml": body},
            {"images/fig1.png": _PNG},
        )
        out_md = tmp_path / "md" / "markdown.md"
        res = _run_worker(epub, out_md, tmp_path / "images")
        md = out_md.read_text(encoding="utf-8")

        # marker interleaved at the text position, stable saved name
        assert "![Abb. 1: Umsatz](image_0.png)" in md
        # caption rides as document text (PDF parity)
        assert "Abb. 1: Umsatzentwicklung im Zeitverlauf" in md
        assert res["image_mapping"] == {"image_0.png": "image_0.png"}

        chunks, artifacts = _linkage(md, tmp_path / "images", res["image_mapping"])
        assert [a["kind"] for a in artifacts] == ["extracted_image"]
        refd = [c for c in chunks if c["metadata"]["image_refs"]]
        assert len(refd) == 1
        assert refd[0]["metadata"]["image_refs"] == ["image-0000"]
        assert refd[0]["metadata"]["figure_captions"] == {
            "image-0000": "Abb. 1: Umsatzentwicklung im Zeitverlauf"
        }


# ── the parity gate (#274 DoD) ───────────────────────────────────────

# The PDF leg is a PROXY, not the pdf_worker path: marker-style markdown in
# the post-normalization shape the runner seam sees (pdf_worker's own
# marker/output mapping is pinned by the existing marker suites; running
# real Marker/GPU here is out of scope for a dev gate). The gate proves the
# SHARED seam produces equivalent linkage for equivalent input semantics.────

# The PDF leg: marker-style markdown — the post-normalization shape
# (marker output after the pdf_worker mapping, as the runner seam sees
# it): page markers + ![alt](saved_name) + caption as text; pinned by
# the existing marker suites. Same document semantics as the EPUB
# fixture.
_PDF_LEG_MD = """{0}------------------------------------------------

# Kapitel 1

Erster Absatz vor der Abbildung mit genug Text zum Chunken.

![Abb. 1: Umsatz](image_0.png)

Abb. 1: Umsatzentwicklung im Zeitverlauf

Zweiter Absatz nach der Abbildung mit weiterem Fliesstext.
"""


@pytest.mark.skipif(_PANDOC is None, reason="pandoc not on PATH")
def test_parity_gate_pdf_vs_epub_linkage(tmp_path):
    """Equivalent image-bearing fixture through BOTH paths must yield
    equivalent linkage: same image_refs sequence, same figure-caption
    pairing, same artifact declaration. Same semantics, not same bytes."""
    epub = build_epub(
        tmp_path / "book.epub",
        {"text/c1.xhtml": _PUBLISHER_BODY},
        {"images/fig1.png": _PNG},
    )
    epub_md_path = tmp_path / "md" / "markdown.md"
    res = _run_worker(epub, epub_md_path, tmp_path / "epub_images")
    epub_chunks, epub_arts = _linkage(
        epub_md_path.read_text(encoding="utf-8"),
        tmp_path / "epub_images", res["image_mapping"],
    )

    pdf_dir = tmp_path / "pdf_images"
    pdf_dir.mkdir()
    (pdf_dir / "image_0.png").write_bytes(_PNG)
    pdf_chunks, pdf_arts = _linkage(
        _PDF_LEG_MD, pdf_dir, {"image_0.png": "image_0.png"},
    )

    def _shape(chunks):
        return (
            [c["metadata"]["image_refs"] for c in chunks],
            {k: v for c in chunks
             for k, v in (c["metadata"].get("figure_captions") or {}).items()},
        )

    assert _shape(epub_chunks) == _shape(pdf_chunks)
    assert epub_arts == pdf_arts


# ── machine-caption composition (#230 × EPUB refs — the DoD evidence) ────


def test_machine_captions_reach_epub_chunks(tmp_path, monkeypatch):
    """MINOR pin (review #274): the #230 caption stage composes with the
    NEW EPUB image refs — chunk image_captions and the artifact's
    machine_caption attribute appear when an EPUB chunk references an
    extracted image. Reuses the #230 fake-captioner harness."""
    pytest.importorskip("torch")
    from test_image_captions import FakeCaptioner, _run

    epub = build_epub(
        tmp_path / "book.epub",
        {"text/c1.xhtml": _PUBLISHER_BODY},
        {"images/fig1.png": _PNG},
    )
    out_md = tmp_path / "md" / "markdown.md"
    res = _run_worker(epub, out_md, tmp_path / "images")
    chunks, artifacts = _linkage(
        out_md.read_text(encoding="utf-8"),
        tmp_path / "images", res["image_mapping"],
    )

    ran, sc = _run(artifacts, chunks, tmp_path, FakeCaptioner())
    assert ran is True and sc["image_captions"] is True
    refd = next(c for c in chunks if c["metadata"].get("image_refs"))
    assert refd["metadata"]["image_captions"] == {
        "image-0000": artifacts[0]["attributes"]["machine_caption"],
    }
    assert artifacts[0]["attributes"]["caption_model"] == "fake-vision-1"


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__]))
