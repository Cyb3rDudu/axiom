"""#173 W7 — fixture tests for locator_rescan.plan_updates (the pure planning
core; no DB, no OS, no network). Each test pins one decision rule of the
re-trust pass: folio heal with evidence, the two hold rules, the epub-only
scope, the missing-PDF downgrade, and convergence (stamps = full planned
set regardless of the DB diff)."""
from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(REPO_ROOT))

_spec = importlib.util.spec_from_file_location(
    "locator_rescan", Path(__file__).resolve().parent.parent / "scripts" / "locator_rescan.py"
)
assert _spec is not None and _spec.loader is not None
rescan = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rescan)
pt = rescan.pt  # axiom_ng_runner.compute_core.page_trust constants


# A book whose start page is folio_verified WITH counter-check evidence:
# labels must heal to the printed folios.
TRUST_FOLIO = (
    {0: "4", 1: "5", 2: "6", 3: "7"},          # label_map (printed folios)
    {0: pt.FOLIO_VERIFIED, 1: pt.FOLIO_VERIFIED,
     2: pt.FOLIO_VERIFIED, 3: pt.PHYSICAL_ONLY},  # source_map: run ends at 2
)


class PlanUpdatesTests(unittest.TestCase):
    def test_folio_heal_with_evidence(self):
        rows = [("c1", {"type": "page_span", "physical_page_start": 0,
                       "physical_page_end": 1, "page_label_start": "I-1",
                       "page_label_end": "I-2", "source": "marker_paginate"},
                 "/books/Folio Buch.pdf")]
        updates, stamps, dist, held, heals, by_doc, _ = rescan.plan_updates(
            rows, {"/books/Folio Buch.pdf": TRUST_FOLIO},
            heal_books={"Folio Buch.pdf"}, skip_books=set())
        self.assertEqual(len(updates), 1)
        self.assertEqual(updates[0][1]["page_source"], pt.FOLIO_VERIFIED)
        self.assertEqual(updates[0][1]["page_label_start"], "4")   # healed
        self.assertEqual(updates[0][1]["page_label_end"], "5")     # same verified run
        self.assertEqual(heals, 1)
        self.assertEqual(dist[pt.FOLIO_VERIFIED], 1)
        self.assertEqual(len(stamps), 1)
        self.assertEqual(dict(held), {})

    def test_no_evidence_folio_holds_legacy(self):
        rows = [("c1", {"type": "page_span", "physical_page_start": 0,
                       "page_label_start": "1", "source": "marker_paginate"},
                 "/books/Unevidenced.pdf")]
        updates, _, _, held, heals, _, _ = rescan.plan_updates(
            rows, {"/books/Unevidenced.pdf": TRUST_FOLIO},
            heal_books=set(), skip_books=set())
        self.assertEqual(updates, [])               # legacy stays untouched
        self.assertEqual(heals, 0)
        self.assertEqual(held["no_evidence_folio"], 1)

    def test_skip_book_holds_page_spans_but_stamps_epub(self):
        rows = [
            ("c1", {"type": "page_span", "physical_page_start": 0,
                    "page_label_start": "1", "source": "marker_paginate"},
             "/books/Amtsblatt.pdf"),
            ("c2", {"type": "epub_cfi", "cfi_start": "/6/4", "cfi_end": "/6/8",
                    "source": "epub"}, "/books/Amtsblatt.epub"),
        ]
        updates, _, _, held, _, _, _ = rescan.plan_updates(
            rows, {"/books/Amtsblatt.pdf": TRUST_FOLIO},
            heal_books=set(), skip_books={"Amtsblatt.pdf"})
        self.assertEqual([cid for cid, _ in updates], ["c2"])  # only the epub stamp
        self.assertEqual(updates[0][1]["page_source"], pt.NONE)
        self.assertEqual(held["skip_book"], 1)

    def test_epub_only_mode_plans_only_none_stamps(self):
        rows = [
            ("c1", {"type": "page_span", "physical_page_start": 0,
                    "page_label_start": "1", "source": "marker_paginate"},
             "/books/Folio Buch.pdf"),
            ("c2", {"type": "epub_cfi", "cfi_start": "/6/4", "cfi_end": "/6/8",
                    "source": "epub"}, "/books/Book.epub"),
        ]
        updates, stamps, dist, _, _, _, _ = rescan.plan_updates(
            rows, {"/books/Folio Buch.pdf": TRUST_FOLIO},
            heal_books={"Folio Buch.pdf"}, skip_books=set(), apply_epub_only=True)
        self.assertEqual([cid for cid, _ in updates], ["c2"])
        self.assertEqual([cid for cid, _ in stamps], ["c2"])    # PDF locator NOT in scope
        self.assertEqual(dist[pt.FOLIO_VERIFIED], 1)           # stats still see it
        self.assertEqual(dist[pt.NONE], 1)

    def test_missing_pdf_downgrades_to_physical_only(self):
        rows = [("c1", {"type": "page_span", "physical_page_start": 3,
                       "page_label_start": "99", "source": "marker_paginate"},
                 "/books/Verschollen.pdf")]
        updates, _, dist, _, _, _, _ = rescan.plan_updates(
            rows, {}, heal_books=set(), skip_books=set())    # no trust entry
        self.assertEqual(updates[0][1]["page_source"], pt.PHYSICAL_ONLY)
        self.assertEqual(updates[0][1]["page_label_start"], "99")  # label kept, claim dropped
        self.assertEqual(dist[pt.PHYSICAL_ONLY], 1)

    def test_end_label_outside_verified_run_is_dropped(self):
        # W5: span starts on a verified folio page but ENDS on an unverified
        # one (different numbering space) — the end label must not survive.
        rows = [("c1", {"type": "page_span", "physical_page_start": 2,
                       "physical_page_end": 3, "page_label_start": "I-6",
                       "page_label_end": "I-7", "source": "marker_paginate"},
                 "/books/Folio Buch.pdf")]
        updates, _, _, _, _, _, _ = rescan.plan_updates(
            rows, {"/books/Folio Buch.pdf": TRUST_FOLIO},
            heal_books={"Folio Buch.pdf"}, skip_books=set())
        loc = updates[0][1]
        self.assertEqual(loc["page_source"], pt.FOLIO_VERIFIED)
        self.assertEqual(loc["page_label_start"], "6")
        self.assertNotIn("page_label_end", loc)               # mixed space: dropped

    def test_stamps_are_the_full_planned_set_not_the_diff(self):
        # W1 convergence: an already-stamped row (diff == 0, e.g. after a
        # crash between DB commit and the OS bulk) still lands in stamps.
        rows = [
            ("c1", {"type": "page_span", "physical_page_start": 0,
                    "page_label_start": "4", "source": "marker_paginate",
                    "page_source": pt.FOLIO_VERIFIED},       # already planned state
             "/books/Folio Buch.pdf"),
        ]
        updates, stamps, _, _, _, _, _ = rescan.plan_updates(
            rows, {"/books/Folio Buch.pdf": TRUST_FOLIO},
            heal_books={"Folio Buch.pdf"}, skip_books=set())
        self.assertEqual(updates, [])                        # DB diff empty
        self.assertEqual(len(stamps), 1)                     # OS still reconciled
        self.assertEqual(stamps[0][1]["page_source"], pt.FOLIO_VERIFIED)


if __name__ == "__main__":
    unittest.main()
