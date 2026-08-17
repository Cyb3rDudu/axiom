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


def make_pdf(top_lines, repeat_labels=None, single_labels=False, firstpagenum=None):
    """top_lines: per page the TOP line (folio candidate) or None for prose.

    firstpagenum: single label range starting there — the TRUE offset fault
    (unique, monotone, covering labels lagging behind the printed folios).
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
    if repeat_labels or single_labels or firstpagenum:
        d2 = pymupdf.open(f.name)
        if repeat_labels:
            half = max(1, len(top_lines) // 2)
            d2.set_page_labels([
                {"startpage": 0, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
                {"startpage": half, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
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
        labels, sources = pt.build_page_trust(
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
        labels, sources = pt.build_page_trust(
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
        labels, sources = pt.build_page_trust(
            self._pdf([None] * 6, single_labels=True))
        self.assertEqual(sources[0], pt.PDF_LABEL_SANE)
        self.assertEqual(sources[5], pt.PDF_LABEL_SANE)

    def test_no_labels_no_folio_physical(self):
        labels, sources = pt.build_page_trust(self._pdf([None] * 4))
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


if __name__ == "__main__":
    unittest.main()
