"""#173 page-trust tests: the four fault shapes from dudu's verification run
as synthetic fixtures (rot under the old label-blind pipeline).

  z5    PDF carries chapter-wise repeated labels (C1 x n) + printed folios
        4,5,6 in the text layer -> folio_verified with the TRUE folios
  qa2/  unique-but-offset/repeated labels lag behind printed folios
  f17   152..155 -> folio_verified
  sane  unique, monotone, covering labels, no folio lines -> pdf_label_sane
"""
import os
import tempfile
import unittest

import pymupdf
from axiom_ng_runner.compute_core import page_trust as pt


def make_pdf(top_lines, repeat_labels=None, single_labels=False, firstpagenum=None, half_labels=False, sections_spec=None):
    """top_lines: per page the TOP line (folio candidate) or None for prose.

    sections_spec (W12): raw set_page_labels list passthrough — healed
        anchor-plan shape (front-matter prefix section + arabic restarts).

    firstpagenum: single label range starting there — the TRUE offset fault
        (unique, monotone, covering labels lagging behind the printed folios).
    half_labels: label range covering EXACTLY the first half of the pages —
        the tier1*2 == n boundary fixture (S1).
    """
    doc = pymupdf.open()
    for i, top in enumerate(top_lines):
        page = doc.new_page()
        y = 20
        if top:
            page.insert_text((72, y), top, fontsize=10)
            y = 40
        page.insert_text((72, y), f"Fachtext Seite {i} des Kapitels.", fontsize=10)
    f = tempfile.NamedTemporaryFile(suffix=".pdf", delete=False)
    doc.save(f.name)
    doc.close()
    if sections_spec or repeat_labels or single_labels or firstpagenum or half_labels:
        d2 = pymupdf.open(f.name)
        if sections_spec:
            d2.set_page_labels(sections_spec)
        elif repeat_labels:
            half = max(1, len(top_lines) // 2)
            d2.set_page_labels([
                {"startpage": 0, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
                {"startpage": half, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
            ])
        elif half_labels:
            half = max(1, len(top_lines) // 2)
            d2.set_page_labels([
                {"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1},
                {"startpage": half, "prefix": "", "style": "none"},
            ])
        elif firstpagenum:
            d2.set_page_labels(
                [{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": firstpagenum}])
        else:
            d2.set_page_labels([{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}])
        out = f.name + ".l.pdf"
        d2.save(out)
        d2.close()
        os.unlink(f.name)
        return out
    return f.name


class PageTrustTests(unittest.TestCase):
    def setUp(self):
        self._files = []

    def tearDown(self):
        for f in self._files:
            try:
                os.unlink(f)
            except OSError:
                pass

    def _pdf(self, *args, **kw):
        f = make_pdf(*args, **kw)
        self._files.append(f)
        return f

    def test_z5_repeated_labels_folio_rescues(self):
        labels, sources, _ch = pt.build_page_trust(
            self._pdf(["4", "5", "6", None, None], repeat_labels="C1"))
        self.assertEqual([labels[i] for i in range(3)], ["4", "5", "6"])
        for i in range(3):
            self.assertEqual(sources[i], pt.FOLIO_VERIFIED, f"page {i}")
        # pages without a folio line fall back honestly
        for i in (3, 4):
            self.assertEqual(sources[i], pt.PHYSICAL_ONLY, f"page {i}")

    def test_offset_labels_folio_wins(self):
        # qa2/f17 TRUE shape: unique, monotone, covering labels starting at
        # 148 (lagging 4 behind the printed folios 152..155) — sanity CANNOT
        # see this; only the always-consulted folio sequence can (review C1:
        # the original delivery gated folio behind suspect labels and this
        # fault sailed through as pdf_label_sane).
        labels, sources, _ch = pt.build_page_trust(
            self._pdf(["152", "153", "154", "155"], firstpagenum=148))
        self.assertEqual([labels[i] for i in range(4)], ["152", "153", "154", "155"])
        for i in range(4):
            self.assertEqual(sources[i], pt.FOLIO_VERIFIED)

    def test_rot_vorher_old_pipeline_is_wrong(self):
        # DoD: rot-vorher demonstrated IN CODE — the old label-blind pipeline
        # (extract_page_labels alone) produces the wrong labels for the
        # fault fixtures the trust pipeline fixes.
        from axiom_ng_runner.compute_core.pdf_processing import extract_page_labels

        z5 = extract_page_labels(self._pdf(["4", "5", "6", None, None], repeat_labels="C1"))
        self.assertNotEqual([z5.get(i) for i in range(3)], ["4", "5", "6"],
                            "z5 fixture must be red under the old pipeline")
        off = extract_page_labels(self._pdf(["152", "153", "154", "155"], firstpagenum=148))
        self.assertNotEqual([off.get(i) for i in range(4)], ["152", "153", "154", "155"],
                            "offset fixture must be red under the old pipeline")

    def test_sane_labels_stay_sane(self):
        labels, sources, _ch = pt.build_page_trust(
            self._pdf([None] * 6, single_labels=True))
        self.assertEqual(sources[0], pt.PDF_LABEL_SANE)
        self.assertEqual(sources[5], pt.PDF_LABEL_SANE)

    def test_tier_boundary_exactly_half_is_physical(self):
        # S1: tier1*2 == n — at EXACTLY 50% label coverage the 3-tier
        # extractor has already fallen back to tier 2/3; the boundary clause
        # must route to physical_only, never stamp fabricated labels as
        # pdf_label_sane (boundary drift, review finding).
        labels, sources, _ch = pt.build_page_trust(self._pdf([None] * 4, half_labels=True))
        self.assertEqual(labels, {0: "1", 1: "2", 2: "3", 3: "4"})
        for i in range(4):
            self.assertEqual(sources[i], pt.PHYSICAL_ONLY, f"page {i}")

    def test_no_labels_no_folio_physical(self):
        labels, sources, _ch = pt.build_page_trust(self._pdf([None] * 4))
        self.assertEqual(sources[0], pt.PHYSICAL_ONLY)
        self.assertEqual(labels[2], "3")

    def test_assess_labels_rules(self):
        # repeated label -> suspect
        self.assertEqual(pt.assess_labels({0: "C1", 1: "C1", 2: "C1"}, 3)[0], pt.PHYSICAL_ONLY)
        # unique monotone covering -> sane
        sane = {i: str(i + 1) for i in range(10)}
        self.assertEqual(pt.assess_labels(sane, 10)[0], pt.PDF_LABEL_SANE)
        # non-monotone -> suspect
        bad = {i: str(v) for i, v in enumerate([5, 4, 3, 2, 1, 6, 7, 8, 9, 10])}
        self.assertEqual(pt.assess_labels(bad, 10)[0], pt.PHYSICAL_ONLY)

    def test_folio_sequence_requires_run(self):
        # isolated numbers without a +1 run verify nothing
        self.assertEqual(pt.verify_folio_sequence({0: "9", 5: "77", 9: "3"}), {})
        # a consistent 3-run verifies exactly its members
        verified = pt.verify_folio_sequence({0: "10", 1: "11", 2: "12", 5: "40"})
        self.assertEqual(verified, {0: "10", 1: "11", 2: "12"})
        # the 3-page minimum is load-bearing: a 2-run must NOT verify
        # (a bare year plus one successor is not a folio sequence)
        self.assertEqual(pt.verify_folio_sequence({0: "7", 1: "8"}), {})

    def test_chapter_restart_clash_drops_shorter_runs(self):
        # per-chapter numbering: a 4-page chapter run (1..4) and a 3-page
        # one (1..3) — the LONGEST run wins per value, the shorter drops
        # (a citation must not silently resolve to the earliest chapter)
        verified = pt.verify_folio_sequence(
            {0: "1", 1: "2", 2: "3", 3: "4", 5: "1", 6: "2", 7: "3"})
        self.assertEqual(verified, {0: "1", 1: "2", 2: "3", 3: "4"})
        # length ties are ambiguous — both drop (never guess)
        self.assertEqual(
            pt.verify_folio_sequence({0: "1", 1: "2", 2: "3", 10: "1", 11: "2", 12: "3"}), {})



class StampWitnessTests(unittest.TestCase):
    """#173 contract witness: EVERY adapted locator leaves stamped — the
    adapted branch is the REAL production path (the review critical: it once
    returned unstamped and the §11 gate would have terminal-failed every
    real PDF job)."""

    def test_adapted_chunk_gets_stamped(self):
        from axiom_ng_runner import runner

        old_style = {"text": "Fachtext", "metadata": {"page_start": "5", "page_end": "5",
                                                      "section_titles": [], "token_count": 2}}
        ch = runner._adapt_chunk(old_style, 0, {4: "5"}, {4: "folio_verified"})
        self.assertEqual(ch["locator"]["page_source"], "folio_verified")

        # reference mode (no map): honest physical_only, never a claim
        ch2 = runner._adapt_chunk(dict(old_style), 0, {4: "5"}, None)
        self.assertEqual(ch2["locator"]["page_source"], "physical_only")

    def test_stamp_drops_end_label_outside_start_level(self):
        # W5: a span starting on a folio_verified page but ENDING outside the
        # verified run must not carry the end label — two numbering spaces
        # must never render as one span.
        from axiom_ng_runner import runner

        old_style = {"text": "Fachtext", "metadata": {"page_start": "6", "page_end": "7",
                                                  "section_titles": [], "token_count": 2}}
        ch = runner._adapt_chunk(dict(old_style), 0, {5: "6", 6: "7"},
                                 {5: "folio_verified"})
        self.assertEqual(ch["locator"]["page_source"], "folio_verified")
        self.assertNotIn("page_label_end", ch["locator"])

        # same trust level on the end page: the end label survives
        ch2 = runner._adapt_chunk(dict(old_style), 0, {5: "6", 6: "7"},
                                  {5: "folio_verified", 6: "folio_verified"})
        self.assertEqual(ch2["locator"]["page_label_end"], "7")

    def test_pass_through_epub_gets_none(self):
        from axiom_ng_runner import runner

        contract = {"ref": "chunk-0000", "index": 0, "text": "x",
                    "locator": {"type": "epub_cfi", "cfi_start": "/6/4", "cfi_end": "/6/8", "source": "epub"}}
        ch = runner._adapt_chunk(contract, 0, {})
        self.assertEqual(ch["locator"]["page_source"], "none")


# ── W12: chapter-ordinal stamping (#188) ──────────────────────────────────
# Chapter-relative books (folios restart per chapter, healed anchor label
# sections) must reach folio_verified PER CHAPTER with a chapter ordinal per
# page — the pre-W12 code dropped the shorter clashing runs entirely, so
# chapter 2+ never verified. Mutually red-before: at 9443ff5 pages 8-11 came
# back physical_only (this class is the behavioral pin).

HEALED_SECTIONS = [
    {"startpage": 0, "prefix": "C", "style": "D", "firstpagenum": 1},   # front matter
    {"startpage": 2, "prefix": "", "style": "D", "firstpagenum": 1},    # Kap. 1
    {"startpage": 8, "prefix": "", "style": "D", "firstpagenum": 1},    # Kap. 2 (restart)
]

CH_BOOK_TOPLINES = [None, None, "1", "2", "3", "4", "5", "6", "1", "2", "3", "4"]


class ChapterOrdinalTests(unittest.TestCase):
    def test_chapter_restart_book_verifies_per_chapter(self):
        pdf = make_pdf(CH_BOOK_TOPLINES, sections_spec=HEALED_SECTIONS)
        labels, sources, chapters = pt.build_page_trust(pdf)
        # BOTH chapters verify — the restart values are no longer clashes.
        for i in range(2, 12):
            self.assertEqual(sources[i], pt.FOLIO_VERIFIED, f"page {i}")
        self.assertEqual({i: chapters[i] for i in range(2, 8)}, {i: 1 for i in range(2, 8)})
        self.assertEqual({i: chapters[i] for i in range(8, 12)}, {i: 2 for i in range(8, 12)})
        self.assertNotIn(0, chapters)
        self.assertNotIn(1, chapters)  # front matter carries no ordinal
        self.assertEqual(labels[9], "2")  # Kap. 2, Seite 2

    def test_continuous_labels_stay_legacy(self):
        # Same folios but ONE continuous label section: no restarts to
        # corroborate — byte-identical legacy behavior (restart runs clash,
        # chapter 2 loses, no chapter map).
        spec = [{"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}]
        pdf = make_pdf(CH_BOOK_TOPLINES, sections_spec=spec)
        labels, sources, chapters = pt.build_page_trust(pdf)
        self.assertEqual(sources[6], pt.FOLIO_VERIFIED)   # chapter-1 run wins
        self.assertNotEqual(sources[9], pt.FOLIO_VERIFIED)  # clash loser: no folio claim
        self.assertEqual(sources[9], pt.PDF_LABEL_SANE)     # sane single-section labels fill in
        self.assertEqual(chapters, {})

    def test_folio_contradiction_rejects_chapter_mode(self):
        # Label sections claim a restart at page 8 with value 1, but the
        # printed folios run 1..10 CONTINUOUSLY across the boundary — the
        # runs contradict the section math: never guess, legacy fallback.
        top = [None, None, "1", "2", "3", "4", "5", "6", "7", "8", "9", "10"]
        pdf = make_pdf(top, sections_spec=HEALED_SECTIONS)
        labels, sources, chapters = pt.build_page_trust(pdf)
        # The continuous run verifies as ONE run (no clash), but no chapter
        # ordinals are stamped.
        self.assertEqual(sources[11], pt.FOLIO_VERIFIED)
        self.assertEqual(chapters, {})

    def test_verify_key_is_chapter_plus_value(self):
        cands = {2: "1", 3: "2", 4: "3", 10: "1", 11: "2", 12: "3"}
        chapters = [(2, 1), (10, 2)]
        v = pt.verify_folio_sequence(cands, chapters)
        self.assertEqual(v, {2: "1", 3: "2", 4: "3", 10: "1", 11: "2", 12: "3"})
        # Tie WITHIN one chapter still drops (never guess): two 3-runs in
        # the SAME chapter claiming the same values.
        cands2 = {2: "1", 3: "2", 4: "3", 20: "1", 21: "2", 22: "3"}
        v2 = pt.verify_folio_sequence(cands2, [(2, 1), (30, 2)])  # both runs sit in chapter 1 (2..29)
        self.assertEqual(v2, {})  # 1,2,3 tied in chapter 1 -> all dropped

    def test_chapter_restarts_requires_two_arabic_sections(self):
        pdf = make_pdf([None, "1", "2", "3"])  # single section, no restart
        labels, sources, chapters = pt.build_page_trust(pdf)
        self.assertEqual(chapters, {})
        self.assertEqual(sources[3], pt.FOLIO_VERIFIED)


class ChapterStampWitnessTests(unittest.TestCase):
    """W12 locator stamping through _adapt_chunk: the chunker's exact
    ordinal (metadata['chapter']) wins; reference locators fall back to
    their physical page; empty map = locator untouched."""

    def test_metadata_chapter_stamps_locator(self):
        from axiom_ng_runner import runner

        old_style = {"text": "Fachtext", "metadata": {
            "page_start": "3", "page_end": "3", "section_titles": [],
            "token_count": 2, "chapter": 2}}
        ch = runner._adapt_chunk(old_style, 0, {9: "3"}, {9: "folio_verified"}, {9: 2})
        self.assertEqual(ch["locator"]["chapter"], 2)

    def test_reference_locator_falls_back_to_physical(self):
        from axiom_ng_runner import runner

        contract = {"ref": "chunk-0000", "index": 0, "text": "x",
                    "locator": {"type": "page_span", "physical_page_start": 9,
                                "physical_page_end": 9, "page_label_start": "3",
                                "page_label_end": "3", "source": "marker_paginate"}}
        runner._stamp_page_source(contract["locator"], {9: "folio_verified"})
        runner._stamp_chapter(contract["locator"], {9: 2})
        self.assertEqual(contract["locator"]["chapter"], 2)

    def test_no_map_leaves_locator_untouched(self):
        from axiom_ng_runner import runner

        locator = {"type": "page_span", "physical_page_start": 9,
                   "page_label_start": "3", "source": "marker_paginate"}
        before = dict(locator)
        runner._stamp_chapter(locator, None)
        runner._stamp_chapter(locator, {})
        self.assertEqual(locator, before)


if __name__ == "__main__":
    unittest.main()


class ChapterReviewHardeningTests(unittest.TestCase):
    """W12 auto-review round: the mutation classes that survived the first
    delivery, each now with its failing regression."""

    def test_value_math_corroboration_is_pinned(self):
        # Section 1 claims firstpagenum 5 but the printed folios start at 1:
        # the run contradicts the section math -> chapters rejected.
        spec = [
            {"startpage": 0, "prefix": "C", "style": "D", "firstpagenum": 1},
            {"startpage": 2, "prefix": "", "style": "D", "firstpagenum": 5},
            {"startpage": 8, "prefix": "", "style": "D", "firstpagenum": 1},
        ]
        pdf = make_pdf(CH_BOOK_TOPLINES, sections_spec=spec)
        _, _, chapters = pt.build_page_trust(pdf)
        self.assertEqual(chapters, {})

    def test_zero_folio_evidence_rejects_chapter_mode(self):
        # >= 2 arabic sections but NO printed folios anywhere: accepting the
        # tree alone would guess part ordinals as chapters (vacuous
        # corroboration) — rejected.
        pdf = make_pdf([None] * 12, sections_spec=HEALED_SECTIONS)
        _, _, chapters = pt.build_page_trust(pdf)
        self.assertEqual(chapters, {})

    def test_front_matter_run_crossing_into_section_rejects(self):
        # A front-matter-rooted run whose tail reaches into section 1
        # contradicts the section math on those pages — rejected, not skipped.
        top = ["5", "6", "7", "8", None, None, None, None, "1", "2", "3", "4"]
        pdf = make_pdf(top, sections_spec=HEALED_SECTIONS)
        _, _, chapters = pt.build_page_trust(pdf)
        self.assertEqual(chapters, {})

    def test_preflight_healed_chapter_book_reads_green(self):
        # The W7-loop guard: pre-heal the restart book reads reparierbar
        # (broken restart labels + folio evidence), post-heal (anchor
        # sections corroborated) it reads gesund — a healed chapter book
        # must never re-enter the repair queue.
        from axiom_ng_runner.compute_core import pdf_health

        # pre-heal: chapter-wise repeated labels (broken) + restart folios
        pre = pdf_health.analyze_pdf(make_pdf(CH_BOOK_TOPLINES, repeat_labels="C1"))
        self.assertIn("reparierbar", pre["verdacht"])
        post = pdf_health.analyze_pdf(make_pdf(CH_BOOK_TOPLINES, sections_spec=HEALED_SECTIONS))
        self.assertEqual(post["verdacht"], "🟢 gesund")

    def test_stamp_chapter_never_overwrites(self):
        from axiom_ng_runner import runner

        locator = {"type": "page_span", "physical_page_start": 9,
                   "page_label_start": "3", "source": "marker_paginate", "chapter": 7}
        runner._stamp_chapter(locator, {9: 2})
        self.assertEqual(locator["chapter"], 7)

    def test_physical_anchor_uses_chunker_marker_truth(self):
        # Review C1: chapter-relative books carry duplicate labels across
        # chapters; the label reverse-mapping (min of hits) resolved a
        # chapter-2 "3" chunk to chapter 1's page 4. The chunker's Marker
        # physical anchors must win end-to-end.
        from axiom_ng_runner.compute_core.chunker import Chunker
        from axiom_ng_runner import runner as R

        pdf = make_pdf(CH_BOOK_TOPLINES, sections_spec=HEALED_SECTIONS)
        labels, sources, chmap = pt.build_page_trust(pdf)
        md = "\n\n".join(
            f"{{{i}}}" + "-" * 10 + f"\n\nFachtext Seite {i} des Kapitels." for i in range(12)
        )
        chunks = Chunker(max_chunk_tokens=15, overlap_tokens=0, min_chunk_tokens=1).chunk(
            md, doc_metadata={"doc_id": "t", "page_label_map": labels, "page_chapter_map": chmap}
        )
        out = [R._adapt_chunk(c, i, labels, sources, chmap) for i, c in enumerate(chunks)]
        c10 = next(c for c in out if "Seite 10" in c["text"])  # chapter 2, label "3"
        self.assertEqual(c10["locator"]["physical_page_start"], 10)  # NOT chapter 1's page 4
        self.assertEqual(c10["locator"]["chapter"], 2)
