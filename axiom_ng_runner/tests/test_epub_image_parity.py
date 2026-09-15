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
    _img_attr,
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

    def test_fenced_and_indented_code_untouched(self):
        """MAJOR pin (review round 2, P7): a literal HTML sample inside a
        fenced block (any body length) OR a pandoc <pre><code> block
        (4-space indented) must NOT gain a phantom image marker — the
        marker's ref has no artifact and would terminally fail the
        snapshot persist gate."""
        fence_md = (
            "Text davor.\n\n```html\n"
            '<figure><img src="../images/fig1.png" alt="sample"/></figure>\n'
            "second line\nthird line\n"
            "```\n\nText danach."
        )
        assert _inline_html_images(fence_md) == fence_md
        ref_index = {"../images/fig1.png": "image_0.png"}
        assert _rewrite_image_refs(fence_md, ref_index) == fence_md
        indented_md = (
            "Ein Code-Beispiel:\n\n"
            "    <figure><img src=\"../images/fig1.png\"/></figure>\n"
            "    more code\n\nText danach."
        )
        assert _inline_html_images(indented_md) == indented_md
        assert _rewrite_image_refs(indented_md, ref_index) == indented_md
        # sanity: the SAME html outside code IS inlined (a 2-space list
        # continuation is NOT code — pandoc's list content column)
        list_md = "- Punkt\n  <figure><img src=\"../images/fig1.png\"/></figure>"
        assert "![" in _inline_html_images(list_md)

    def test_bracket_carrying_path_rewritten(self):
        """Round-4 pin: paths with ')' inside (fig(1).png) resolve via
        candidate scanning — the first-candidate form stops too early."""
        out = _rewrite_image_refs(
            "![A](/tmp/epub_media_x/fig(1).png)",
            {"/tmp/epub_media_x/fig(1).png": "image_0.png"},
        )
        assert out == "![A](image_0.png)"

    def test_unbalanced_fences_stay_fast(self):
        """Perf pin (round 4): 20k unterminated fence-open lines must not
        rescan to EOF per candidate (measured 15.9 s before the tempered
        fence pattern)."""
        import time

        md = "```html\n" * 20_000 + "x" * 1000
        t0 = time.monotonic()
        _inline_html_images(md)
        assert time.monotonic() - t0 < 10.0

    def test_code_span_closes_on_dedent(self):
        """ROUND-5 BLOCKER pin: an open code span must CLOSE at the first
        non-blank line indented <4 — round-4 let it run to EOF, eating
        every later ref (corpus: one book's CC badge went unresolved →
        terminal persist failure)."""
        md = ("Text\n\n    code line\n\nNormaler Absatz.\n\n"
              "![img](f.png)")
        out = _rewrite_image_refs(md, {"f.png": "image_0.png"})
        assert "![img](image_0.png)" in out
        # a document ENDING in an indented code block keeps it protected
        # to EOF (the only span shape that legitimately reaches the end)
        md_end = "Text\n\n    tail <img src='t.png'/>"
        assert _inline_html_images(md_end) == md_end

    def test_marker_only_line_opens_list_not_code(self):
        """ROUND-5 MAJOR pin: pandoc loose lists emit marker-only lines
        like '    1.  ' at 4-space indent with an empty stack — a list,
        not code (round-4 started a code span there and swallowed the
        following figure)."""
        md = "    1.  \n\n<figure><img src='f.png' alt='L'/></figure>"
        out = _inline_html_images(md)
        assert "![L](f.png)" in out
        # the isolated rule: a figure at the marker's CONTENT column (8)
        # is list content and must be inlined — under the round-4
        # misclassification (span opens at the marker line) it would sit
        # inside the code span and stay raw
        md2 = "    1.  \n\n        <figure><img src='g.png' alt='M'/></figure>"
        assert "![M](g.png)" in _inline_html_images(md2)

    def test_thematic_break_is_not_a_list_marker(self):
        """ROUND-5 NIT pin: '* * *' / '---' are thematic breaks — they
        must not open list context (an indented code block after one
        stays code)."""
        md = "* * *\n\n    <figure><img src='f.png'/></figure>"
        assert _inline_html_images(md) == md

    def test_marker_inside_code_stays_code(self):
        """A marker-looking line INSIDE an open code span is code, not a
        list opener: the span is opened by a NON-marker line ('    code
        start'), then a marker-with-content line carrying rewritable
        HTML stays inside it — verified to fail under a steal-mutation
        (marker detection running before/inside the code span would
        inline the <img> and produce a phantom ref)."""
        md = "    code start\n    1. <img src='p.png'/>\n    more\n\nText."
        assert _inline_html_images(md) == md

    def test_blank_inside_code_keeps_span_open(self):
        """A blank line inside an indented code block keeps the span
        open (GFM): the <figure> after the blank is still sample text,
        not inlineable HTML."""
        md = "    code one\n\n    <figure><img src='f.png'/></figure>"
        assert _inline_html_images(md) == md

    def test_unbalanced_figure_opens_stay_fast(self):
        """Perf pin: many unmatched <figure> opens + one closing must not
        go quadratic (measured 35 s at 20k opens before the content
        guard). Generous wall-clock bound to stay CI-stable."""
        import time

        md = "<figure><img src='a.png'>" * 20_000 + "x</figure>" + "y" * 1000
        t0 = time.monotonic()
        _inline_html_images(md)
        assert time.monotonic() - t0 < 10.0

    def test_data_src_attribute_not_matched(self):
        """706ffe boundary pin: data-src/data-alt are different attributes
        — no marker, no src confusion."""
        out = _inline_html_images('<img data-src="nope.png" data-alt="x"/>')
        assert out == ""

    def test_markdown_title_ref_path_only(self):
        """Round-3 pin: pandoc emits ![alt](path "title") for <img title>.
        The rewrite takes the path and DROPS the title — the PDF path
        emits no titles, and a title left in the ref would miss the
        runner's basename lookup and terminally fail the persist gate."""
        out = _rewrite_image_refs(
            '![alt](media/images/fig1.png "Titeltext")',
            {"media/images/fig1.png": "image_0.png"},
        )
        assert out == "![alt](image_0.png)"

    def test_link_wrapped_badge_rewritten_link_intact(self):
        """BLOCKER pin (review round 2, corpus: 10 books / 202 refs):
        Springer license badges are [![alt](path)](url) — the path must
        rewrite and the link wrapper must survive; the old \\S+ form
        swallowed ')](url' into the ref and left the temp path in place
        (terminal CHUNK_IMAGE_REF_UNRESOLVED)."""
        md = (
            "Lizenz: [![CC BY-NC-ND](media/images/cc.png)]"
            "(https://creativecommons.org/licenses/by-nc-nd/4.0/) Ende"
        )
        out = _rewrite_image_refs(md, {"media/images/cc.png": "image_0.png"})
        assert (
            "Lizenz: [![CC BY-NC-ND](image_0.png)]"
            "(https://creativecommons.org/licenses/by-nc-nd/4.0/) Ende" == out
        )
        # with a title inside the badge image, the title is dropped too
        out = _rewrite_image_refs(
            "[![CC](media/images/cc.png \"badge\")](https://x.example)",
            {"media/images/cc.png": "image_0.png"},
        )
        assert out == "[![CC](image_0.png)](https://x.example)"

    def test_space_carrying_path_rewritten(self):
        """Round-3 pin: paths with spaces resolve (non-greedy stops at
        ')', not at whitespace)."""
        out = _rewrite_image_refs(
            "![A](/tmp/epub_media_x/my fig.png)",
            {"/tmp/epub_media_x/my fig.png": "image_0.png"},
        )
        assert out == "![A](image_0.png)"

    def test_quoted_gt_in_attributes_keeps_image(self):
        """MAJOR pin (review round 2, Databricks Fig15): alt texts contain
        '>' ("path: All catalogs > unitygo > …") — quote-aware tag matching
        must find the src instead of truncating the tag at the inner >
        (which silently dropped the image while the artifact existed)."""
        tag = '<img src="media/images/f15.jpg" alt="path: All catalogs > unitygo > tools"/>'
        assert _img_attr(tag, "src") == "media/images/f15.jpg"
        out = _inline_html_images(
            '<figure>' + tag + "<figcaption>Abb. 15</figcaption></figure>"
        )
        assert "![path: All catalogs > unitygo > tools](media/images/f15.jpg)" in out
        assert "Abb. 15" in out

    def test_escaped_img_in_residual_makes_no_phantom_marker(self):
        """MINOR pin: &lt;img …&gt; escaped inside a figure description must
        not materialize into a phantom marker via the unescape."""
        out = _inline_html_images(
            '<figure><img src="media/images/f1.png"/>'
            '<div class="TextObject"><p>Beispiel: &lt;img src="nope.png"&gt;</p></div>'
            "</figure>"
        )
        assert "nope.png" not in out
        assert out.count("![") == 1
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

    def test_nested_list_figure_inlined_top_code_protected(self, tmp_path):
        """ROUND-4 MAJOR pin: pandoc indents figures inside NESTED list
        items at 4 spaces — the same indent as top-level <pre><code>
        samples. The list-context tracker must keep the figure processed
        (5 corpus books lost 19 images to the blanket indent guard)
        while the top-level code sample stays verbatim."""
        body = (
            "<h1>Kapitel 1</h1>"
            "<ul><li>Aeussere Punkt"
            "<ul><li><p>Innerer Punkt mit Figur:</p>"
            '<figure id="Fig22" class="Figure">'
            '<img src="../images/fig1.png" alt="Nested"/>'
            "<figcaption>Abb. 22</figcaption></figure></li></ul></li></ul>"
            "<p>Top-Level Code-Beispiel:</p>"
            '<pre><code>&lt;img src="../images/fig2.png"/&gt;</code></pre>'
        )
        epub = build_epub(
            tmp_path / "book.epub",
            {"text/c1.xhtml": body},
            {"images/fig1.png": _PNG, "images/fig2.png": _PNG},
        )
        out_md = tmp_path / "md" / "markdown.md"
        res = _run_worker(epub, out_md, tmp_path / "images")
        md = out_md.read_text(encoding="utf-8")

        # nested-list figure: inlined despite its 4-space indent
        assert "![Nested](image_0.png)" in md
        assert "Abb. 22" in md
        # top-level code sample: verbatim, no phantom ref
        assert '    <img src="../images/fig2.png"/>' in md
        assert md.count("![") == 1

        chunks, artifacts = _linkage(md, tmp_path / "images", res["image_mapping"])
        assert [a["ref"] for a in artifacts] == ["image-0000"]
        chunk_refs = [r for c in chunks for r in c["metadata"]["image_refs"]]
        assert chunk_refs == ["image-0000"]

    def test_round3_forms_code_lists_badges_titles(self, tmp_path):
        """Real-pandoc E2E over the round-2/3 corpus forms in ONE book:
        a <pre><code> HTML sample (indented output), a list-embedded
        figure (2-space continuation), a link-wrapped license badge, a
        title-carrying <img>, and a figure img with '>' inside a quoted
        alt. Every real image must end as a resolvable marker; the code
        sample must stay verbatim (no phantom ref)."""
        body = (
            "<h1>Kapitel 1</h1>"
            "<p>Ein Code-Beispiel:</p>"
            '<pre><code>&lt;figure&gt;&lt;img src="../images/fig1.png"/&gt;&lt;/figure&gt;</code></pre>'
            "<p>Eine Liste:</p><ul><li>Erster Punkt.</li>"
            "<li><p>Zweiter Punkt mit Bild:</p>"
            '<figure><img src="../images/fig2.png" alt="Liste"/><figcaption>Abb. L</figcaption></figure>'
            "</li><li><p>Dritter Punkt.</p></li></ul>"
            '<p>Lizenz: <a href="https://creativecommons.org/licenses/by-nc-nd/4.0/">'
            '<img src="../images/cc.png" alt="CC BY-NC-ND"/></a></p>'
            '<p>Mit Titel: <img src="../images/fig3.png" title="Der Titel" alt="Titelbild"/></p>'
            '<figure><img src="../images/fig4.png" alt="path: All &gt; catalogs"/>'
            "<figcaption>Abb. 4</figcaption></figure>"
        )
        epub = build_epub(
            tmp_path / "book.epub",
            {"text/c1.xhtml": body},
            {
                "images/fig1.png": _PNG,
                "images/fig2.png": _PNG,
                "images/cc.png": _PNG,
                "images/fig3.png": _PNG,
                "images/fig4.png": _PNG,
            },
        )
        out_md = tmp_path / "md" / "markdown.md"
        res = _run_worker(epub, out_md, tmp_path / "images")
        md = out_md.read_text(encoding="utf-8")

        # the <pre><code> sample rides as an indented block, verbatim —
        # no marker, no rewrite, no phantom ref
        assert '    <figure><img src="../images/fig1.png"/></figure>' in md

        # every real image is a marker on a SAVED name (badges inside the
        # link wrapper, list figure indented, title dropped, '>' alt kept)
        assert "![Liste](image_1.png)" in md
        assert "[![CC BY-NC-ND](image_0.png)](https://creativecommons.org/licenses/by-nc-nd/4.0/)" in md
        assert "![Titelbild](image_2.png)" in md
        assert '"Der Titel"' not in md
        assert "![path: All > catalogs](image_3.png)" in md
        assert "Abb. 4" in md and "Abb. L" in md

        # end-to-end: every chunk ref resolves to a declared artifact
        chunks, artifacts = _linkage(md, tmp_path / "images", res["image_mapping"])
        art_refs = {a["ref"] for a in artifacts}
        chunk_refs = [r for c in chunks
                      for r in c["metadata"]["image_refs"]]
        assert chunk_refs, "no image refs survived"
        assert set(chunk_refs) <= art_refs, \
            f"unresolved chunk refs: {set(chunk_refs) - art_refs}"
        assert len(artifacts) == 4  # fig1 lives only inside the code sample


# ── the parity gate (#274 DoD) ───────────────────────────────────────

# The PDF leg is a PROXY, not the pdf_worker path: marker-style markdown
# in the post-normalization shape the runner seam sees — page markers +
# ![alt](saved_name) + caption as text (pdf_worker's own output mapping
# is pinned by the existing marker suites; running real Marker/GPU here
# is out of scope for a dev gate). The gate proves the SHARED seam
# produces equivalent linkage for equivalent input semantics.
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
