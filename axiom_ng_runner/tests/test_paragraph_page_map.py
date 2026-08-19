"""#194 per-paragraph page map tests.

The proven live case as fixture: Altenburger chunk 04881089 cites the span
S. 8-11 (folio_verified) while the cited sentence sits on print page 9 —
the paragraph map must let any consumer derive page 9 from the hit
position. Red-first: no map exists pre-#194.
"""
import unittest

from axiom_ng_runner.compute_core.chunker import Chunker


def marker(idx):
    # Marker-Konvention des Chunkers: '{7}----------'
    return "{" + str(idx) + "}" + "-" * 12


class ParagraphPageMapTests(unittest.TestCase):
    def _chunker(self):
        return Chunker(max_chunk_tokens=400, min_chunk_tokens=20, overlap_tokens=0)

    def test_span_chunk_carries_page_boundaries(self):
        # markers are their OWN paragraphs (chunker strips them); content
        # follows on pages 8, 9, 10, 11 (label map: physical 7->8, ...)
        paras = [
            marker(7),
            "Absatz auf Seite acht. " * 12,
            "Mehr Text auf acht.",
            marker(8),
            "Der zitierte Satz steht auf Seite neun. " * 10,
            marker(9),
            "Seite zehn Inhalt. " * 14,
            marker(10),
            "Seite elf Inhalt. " * 14,
        ]
        md = "\n\n".join(paras)
        doc = {"doc_id": "d", "page_label_map": {7: "8", 8: "9", 9: "10", 10: "11"}}
        chunks = self._chunker().chunk(md, doc_metadata=doc)
        spans = [c for c in chunks
                 if c.get("metadata", {}).get("page_start") == "8"
                 and c.get("metadata", {}).get("page_end") == "11"]
        self.assertTrue(spans, f"expected an 8-11 span chunk, got "
                               f"{[(c['metadata'].get('page_start'), c['metadata'].get('page_end')) for c in chunks]}")
        meta = spans[0]["metadata"]
        bounds = meta.get("paragraph_pages")
        self.assertIsNotNone(bounds, "span chunk must carry paragraph_pages")
        self.assertEqual(bounds[0], [0, "8"])
        pages = [b[1] for b in bounds]
        self.assertIn("9", pages)
        # offsets strictly increase, pages change at each boundary
        offs = [b[0] for b in bounds]
        self.assertEqual(offs, sorted(set(offs)))
        self.assertEqual(len(pages), len(set(pages)), "each boundary must change the page")

    def test_exact_page_of_hit_position(self):
        """The acceptance: the S.9 sentence yields page 9, not the 8-11 span."""
        sentence = "Der zitierte Satz steht auf Seite neun."
        paras = [
            marker(7),
            "Absatz auf Seite acht. " * 12,
            "Mehr Text auf acht.",
            marker(8),
            sentence + " " + sentence,
            marker(9),
            "Seite zehn Inhalt. " * 14,
            marker(10),
            "Seite elf Inhalt. " * 14,
        ]
        md = "\n\n".join(paras)
        doc = {"doc_id": "d", "page_label_map": {7: "8", 8: "9", 9: "10", 10: "11"}}
        chunks = self._chunker().chunk(md, doc_metadata=doc)
        spans = [c for c in chunks
                 if c.get("metadata", {}).get("page_start") == "8"
                 and c.get("metadata", {}).get("page_end") == "11"]
        chunk = spans[0]
        pos = chunk["text"].find(sentence)
        self.assertGreaterEqual(pos, 0)
        page = None
        for off, lab in chunk["metadata"]["paragraph_pages"]:
            if off <= pos:
                page = lab
        self.assertEqual(page, "9", "the S.9 sentence must resolve to page 9")

    def test_single_page_chunk_has_single_bound(self):
        md = "\n\n".join([marker(3), "Nur Seite vier. " * 20, "Noch mehr vier."])
        doc = {"doc_id": "d", "page_label_map": {3: "4"}}
        chunks = self._chunker().chunk(md, doc_metadata=doc)
        withmap = [c for c in chunks if c["metadata"].get("paragraph_pages")]
        for c in withmap:
            self.assertEqual(c["metadata"]["paragraph_pages"], [[0, "4"]])

    def test_recursive_split_shifts_bounds(self):
        # one huge paragraph pair crossing pages forces _recursive_split
        big = "Sehr langer Absatz. " * 400
        md = "\n\n".join([marker(5), big, marker(6), big])
        doc = {"doc_id": "d", "page_label_map": {5: "6", 6: "7"}}
        ch = Chunker(max_chunk_tokens=200, min_chunk_tokens=20, overlap_tokens=0)
        chunks = ch.chunk(md, doc_metadata=doc)
        subs = [c for c in chunks if len(c["text"]) < len(big)]
        self.assertTrue(subs)
        for c in subs:
            bounds = c["metadata"].get("paragraph_pages")
            if bounds:
                self.assertEqual(bounds[0][0], 0)
                offs = [b[0] for b in bounds]
                self.assertEqual(offs, sorted(offs))
                # every page reachable in the parent span
                self.assertTrue(all(lab in ("6", "7") for _, lab in bounds))


if __name__ == "__main__":
    unittest.main()
