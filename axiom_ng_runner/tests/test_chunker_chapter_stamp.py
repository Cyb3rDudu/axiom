"""W12 chapter-ordinal stamping: the chunker carries the chapter ordinal of
the page its FIRST content sits on (snapshot-at-open discipline, same as
the #186 section trail) from doc_metadata["page_chapter_map"] into chunk
metadata. The runner then stamps locator["chapter"] (W4 renders
"Kap. N, S. X"). Mutation barrier: without the stamping, chapter-relative
books cannot produce a single chapter-numbered locator.

Run: python3 -m pytest axiom_ng_runner/tests/test_chunker_chapter_stamp.py
"""

import unittest

from axiom_ng_runner.compute_core.chunker import Chunker

DOC = """{0}------------------------------------------------

# Erster Abschnitt

Inhalt auf Seite eins des ersten Kapitels mit Text.

{2}------------------------------------------------

Mehr Inhalt im ersten Kapitel auf Seite drei.

{5}------------------------------------------------

# Zweiter Abschnitt

Inhalt im zweiten Kapitel hier auf Seite sechs.
"""


class ChapterStampTests(unittest.TestCase):
    def test_chapter_of_first_content_page(self):
        chmap = {0: 1, 5: 2}
        chunks = Chunker(max_chunk_tokens=1200, overlap_tokens=0, min_chunk_tokens=1).chunk(
            DOC, doc_metadata={"doc_id": "t", "page_label_map": {}, "page_chapter_map": chmap}
        )
        self.assertTrue(chunks)
        first = _first_heading_chunk(chunks, "# Erster Abschnitt")
        self.assertEqual(first["metadata"]["chapter"], 1)
        second = _first_heading_chunk(chunks, "# Zweiter Abschnitt")
        self.assertEqual(second["metadata"]["chapter"], 2)
        # a chunk opened BEFORE the first mapped page (front matter) stays unstamped
        for c in chunks:
            self.assertIn("chapter", c["metadata"], "all fixture content is on mapped pages")

    def test_no_chapter_map_means_no_stamp(self):
        chunks = Chunker(max_chunk_tokens=1200, overlap_tokens=0, min_chunk_tokens=1).chunk(
            DOC, doc_metadata={"doc_id": "t", "page_label_map": {}}
        )
        self.assertTrue(chunks)
        for c in chunks:
            self.assertNotIn("chapter", c["metadata"])

    def test_unmapped_pages_stay_unstamped(self):
        # chapter map covers only page 5; chunks opening on page 0 carry no ordinal
        chunks = Chunker(max_chunk_tokens=1200, overlap_tokens=0, min_chunk_tokens=1).chunk(
            DOC, doc_metadata={"doc_id": "t", "page_label_map": {}, "page_chapter_map": {5: 2}}
        )
        first = _first_heading_chunk(chunks, "# Erster Abschnitt")
        self.assertNotIn("chapter", first["metadata"])
        second = _first_heading_chunk(chunks, "# Zweiter Abschnitt")
        self.assertEqual(second["metadata"]["chapter"], 2)

    def test_string_keys_from_json_are_coerced(self):
        chmap = {"0": 1, "5": 2}  # JSON round-trip shape
        chunks = Chunker(max_chunk_tokens=1200, overlap_tokens=0, min_chunk_tokens=1).chunk(
            DOC, doc_metadata={"doc_id": "t", "page_label_map": {}, "page_chapter_map": chmap}
        )
        second = _first_heading_chunk(chunks, "# Zweiter Abschnitt")
        self.assertEqual(second["metadata"]["chapter"], 2)


def _first_heading_chunk(chunks, heading):
    for c in chunks:
        if c["text"].startswith(heading):
            return c
    raise AssertionError(f"no chunk starts with {heading!r}")


if __name__ == "__main__":
    unittest.main()
