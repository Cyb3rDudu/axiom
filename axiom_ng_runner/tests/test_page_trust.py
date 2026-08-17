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


def make_pdf(top_lines, repeat_labels=None, single_labels=False):
    """top_lines: per page the TOP line (folio candidate) or None for prose."""
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
    if repeat_labels or single_labels:
        d2 = pymupdf.open(f.name)
        if repeat_labels:
            half = max(1, len(top_lines) // 2)
            d2.set_page_labels([
                {"startpage": 0, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
                {"startpage": half, "prefix": repeat_labels, "style": "D", "firstpagenum": 1},
            ])
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
        # qa2/f17 shape: labels repeat/lag, printed folios 152..155
        labels, sources = pt.build_page_trust(
            self._pdf(["152", "153", "154", "155"], repeat_labels="1"))
        self.assertEqual([labels[i] for i in range(4)], ["152", "153", "154", "155"])
        for i in range(4):
            self.assertEqual(sources[i], pt.FOLIO_VERIFIED)

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


if __name__ == "__main__":
    unittest.main()
