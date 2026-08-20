"""W10 extractor-v2 family fixtures (red-first: these fail on the top-only
bare-line extractor).

Every family was learned from the satellite's per-book zone dumps
(/tmp/v_*.txt, /tmp/b_*.txt, 2026-08-18) — the fixture reproduces the zone
signature synthetically; no book content is committed:

  bottom_bare        standalone number in the BOTTOM band (3L9YJBIB class)
  bottom_leading     'N <running title> v. 2025.2' bottom strip, folio
                     leading a text line (2Y4CNL2G / DORA class)
  head_verso_recto   alternating head position: '<N> Author' on versos,
                     '<running title> <N>' on rectos (LDNQ2VW8 class)
  journal_head       running heads with CONSTANT parts (issue/article no.)
                     plus the varying folio — the varying member must win
                     (F6AZQ6CM / G67ITKCB class)
  lseries            'L 333/80 Amtsblatt …' — folio is the after-slash
                     page of the L-series issue (4T2K5S9Y class)
  embedded_first     folio as its OWN line inside the top band, heading
                     text follows on later lines (LCM class)
  roman_bottom       roman folio trailing a bottom text line
                     (CQUZYLJJ / WEO class)
  imf_band           '<N> INTERNATIONAL MONETARY FUND' bottom-left band
                     (IMF Article IV class)
  junk_guard         label-only law text: repeated fragment numbers and
                     years in bands must NEVER verify (D64WSHPH class)
  spread_ambiguous   two bare numbers in one zone line = ambiguous page:
                     the page yields NO candidate; neighbors still verify
  offset_map         label↔folio relations: identity / constant shift /
                     divergent, emitted for wave stamping
"""
import tempfile
import unittest

import pymupdf
from axiom_ng_runner.compute_core import page_trust as pt


def make_pdf(pages, labels_spec=None):
    """pages: list of dicts {top: [lines], bot: [lines]} (0-based order).

    Top lines render at the head, bot lines at the foot, prose in between.
    labels_spec: raw set_page_labels passthrough.
    """
    doc = pymupdf.open()
    for i, spec in enumerate(pages):
        page = doc.new_page()
        h = page.rect.height
        y = 24
        for ln in spec.get("top") or []:
            page.insert_text((72, y), ln, fontsize=9)
            y += 12
        page.insert_text((72, h / 2), f"Laufender Fachtext, Seite {i}.", fontsize=10)
        y = h - 24 - 12 * max(0, len(spec.get("bot") or []) - 1)
        for ln in spec.get("bot") or []:
            page.insert_text((72, y), ln, fontsize=9)
            y += 12
    f = tempfile.NamedTemporaryFile(suffix=".pdf", delete=False)  # noqa: SIM115 — delete=False ist das Muster hier
    doc.save(f.name)
    doc.close()
    if labels_spec:
        d2 = pymupdf.open(f.name)
        d2.set_page_labels(labels_spec)
        d2.saveIncr()
        d2.close()
    return f.name


def n(k):
    return {"top": None, "bot": None, } if k is None else k


class TestHarvestFamilies(unittest.TestCase):
    def setUp(self):
        self.old = None

    def _cands(self, path):
        doc = pymupdf.open(path)
        try:
            return pt.extract_folio_candidates(doc)
        finally:
            doc.close()

    def test_bottom_bare(self):
        path = make_pdf([{"bot": [str(i + 4)]} for i in range(6)])
        c = self._cands(path)
        self.assertEqual([c.get(i) for i in range(6)], ["4", "5", "6", "7", "8", "9"])

    def test_bottom_leading_text(self):
        pages = [{"bot": ["3 Executive summary v. 2025.2"]}]
        pages += [{"bot": [f"{i + 4} Executive summary v. 2025.2"]} for i in range(4)]
        c = self._cands(make_pdf(pages))
        self.assertEqual(c.get(0), "3")
        self.assertEqual(c.get(4), "7")

    def test_head_verso_recto(self):
        pages = []
        for i in range(6):
            v = i + 12
            if v % 2 == 0:
                pages.append({"top": [f"{v} J. Herget und H. Strobl"]})
            else:
                pages.append({"top": [f"Unternehmenskultur – Worüber reden wir? {v}"]})
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(6)], [str(i + 12) for i in range(6)])

    def test_journal_head_constants_lose(self):
        # China-Syndrome form: versos 'VOL. 103 NO. 6 <folio> autor et al.',
        # rectos '<folio> october 2013 THE AMERICAN ECONOMIC REVIEW' — the
        # varying folio must win, never the repeating issue constants.
        pages = []
        for i in range(6):
            f = 2150 + i
            if i % 2 == 0:
                pages.append({"top": [f"VOL. 103 NO. 6 {f} autor et al.: the china syndrome"]})
            else:
                pages.append({"top": [f"{f} october 2013 THE AMERICAN ECONOMIC REVIEW"]})
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(6)], [str(2150 + i) for i in range(6)])
        self.assertNotIn("103", c.values())
        self.assertNotIn("6", c.values())

    def test_citation_head_constants_lose(self):
        # F6AZQ6CM form: EVERY page repeats the full citation head (journal
        # 212 (2021), article 1076) while the folio sits bare in the bottom
        # band — the repeated citation numbers are constants and lose.
        pages = [{"top": ["B. Lu et al. Reliability Engineering and System Safety 212 (2021) 1076"],
                  "bot": [str(1076 + i)]} for i in range(8)]
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(8)], [str(1076 + i) for i in range(8)])
        for const in ("212", "1076", "2021"):
            # '1076' is BOTH the article id (constant, top) and page 0's
            # folio (bare, bottom) — the bare line keeps it where it is a
            # folio; the top citation constant drops everywhere else
            if const != "1076":
                self.assertNotIn(const, c.values())

    def test_lseries(self):
        pages = [{"top": ["DE L 333/80 Amtsblatt der Europäischen Union 27.12.2022"]}]
        pages += [{"top": ["DE 27.12.2022 Amtsblatt der Europäischen Union L 333/81"]}]
        pages += [{"top": ["DE L 333/82 Amtsblatt der Europäischen Union 27.12.2022"]}]
        pages += [{"top": ["DE 27.12.2022 Amtsblatt der Europäischen Union L 333/83"]}]
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(4)], ["80", "81", "82", "83"])

    def test_embedded_first_line(self):
        pages = [{"top": [str(410 + i), "6 Lebensphasenbezogene Dienstleistungen"]} for i in range(3)]
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(3)], ["410", "411", "412"])

    def test_roman_bottom(self):
        pages = [
            {"bot": ["International Monetary Fund", "October 2025 iii"]},
            {"bot": ["International Monetary Fund", "October 2025 iv"]},
            {"bot": ["International Monetary Fund", "October 2025 v"]},
        ]
        c = self._cands(make_pdf(pages))
        self.assertEqual([c.get(i) for i in range(3)], ["iii", "iv", "v"])

    def test_imf_band(self):
        pages = [{"bot": [f"{i + 2} INTERNATIONAL MONETARY FUND"]} for i in range(5)]
        c = self._cands(make_pdf(pages))
        self.assertEqual(c.get(0), "2")
        self.assertEqual(c.get(4), "6")

    def test_junk_guard_never_verifies(self):
        # law text: fragment number repeated on every page + copyright years
        pages = [{"bot": ["(22) Die Mitgliedstaaten … 22"]} for _ in range(8)]
        pages[0]["top"] = ["Verordnung (EU) 2023/2854 … 2006"]
        doc = pymupdf.open(make_pdf(pages, labels_spec=[
            {"startpage": 0, "prefix": "", "style": "D", "firstpagenum": 1}]))
        try:
            cands = pt.extract_folio_candidates(doc)
            chapters = pt.chapter_restarts(doc, cands)
            verified = pt.verify_folio_sequence(cands, chapters)
        finally:
            doc.close()
        self.assertEqual(verified, {}, "junk fragments must never verify")

    def test_years_excluded(self):
        pages = [{"top": ["©2025 International Monetary Fund", "ii"]},
                 {"top": ["©2025 International Monetary Fund", "iii"]},
                 {"top": ["©2025 International Monetary Fund", "iv"]}]
        c = self._cands(make_pdf(pages))
        self.assertNotIn("2025", c.values())
        self.assertEqual(c.get(2), "iv")

    def test_spread_ambiguous_page(self):
        pages = [{"bot": ["11"]}, {"bot": ["12                    13"]}, {"bot": ["14"]}]
        c = self._cands(make_pdf(pages))
        self.assertNotIn(1, c, "two bare numbers on one line = ambiguous, no candidate")

    def test_run_proof_still_gates(self):
        # bottom family with a broken sequence: only the ascending run verifies
        pages = [{"bot": [str(i + 4)]} for i in range(4)] + [{"bot": ["99"]}] + [{"bot": ["9"]}]
        doc = pymupdf.open(make_pdf(pages))
        try:
            cands = pt.extract_folio_candidates(doc)
            verified = pt.verify_folio_sequence(cands)
        finally:
            doc.close()
        self.assertIn(0, verified)
        self.assertNotIn(4, verified)
        self.assertNotIn(5, verified)


class TestOffsetMap(unittest.TestCase):
    def test_identity(self):
        m = pt.offset_map({i: str(i + 1) for i in range(5)}, {i: str(i + 1) for i in range(5)})
        self.assertEqual(m["type"], "identity")

    def test_constant_shift(self):
        # labels lag print by +2 (z5 class: label 149, print 151)
        m = pt.offset_map({i: str(i + 149) for i in range(5)}, {i: str(i + 151) for i in range(5)})
        self.assertEqual(m["type"], "shift")
        self.assertEqual(m["offset"], 2)

    def test_divergent(self):
        # folios restart per chapter while labels run globally -> divergent
        labels = {i: str(i + 1) for i in range(10)}
        folios = {i: str(i + 1) for i in range(5)}
        folios.update({i: str(i - 4) for i in range(5, 10)})
        m = pt.offset_map(labels, folios)
        self.assertEqual(m["type"], "divergent")

    def test_empty(self):
        m = pt.offset_map({0: "1"}, {})
        self.assertEqual(m["type"], "none")


if __name__ == "__main__":
    unittest.main()
