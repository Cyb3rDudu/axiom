"""#186 section-trail fencepost: a chunk's deepest section is the heading
under which its FIRST content sits — the trail state at chunk START, not
the state after the closing boundary.

Mutation barriers (each test names the defect it must survive):

  heading_boundary_chunk_keeps_own_section_not_next
        fencepost: when a split lands exactly on a heading, the OLD code
        updated the heading stack BEFORE emitting the closing chunk, so a
        chunk beginning with "## 1 ..." reported section "## 2 ..." as its
        deepest section (3,762 chunks / 82 books / 19.1% of heading-first
        chunks corpus-wide).

  size_split_mid_section_keeps_own_deepest
        the fix must not overcorrect: a chunk closed by the token budget
        mid-section keeps the section active at its own start.

  deeper_heading_inside_chunk_does_not_hijack_trail
        a deeper heading (### x.y) landing INSIDE a chunk via the final-chunk
        merge must not become the chunk's deepest section either — first
        content wins.

  chunk_opening_at_deeper_heading_includes_it
        a chunk that OPENS at a deeper heading carries that heading as its
        deepest section (its first content sits under it).

Run: python3 -m pytest axiom_ng_runner/tests/test_chunker_section_trail.py
"""

import unittest

from axiom_ng_runner.compute_core.chunker import Chunker


def _deepest(chunk):
    titles = chunk["metadata"]["section_titles"]
    return titles[-1] if titles else None


def _first_heading_chunk(chunks, heading_prefix):
    for c in chunks:
        if c["text"].startswith(heading_prefix):
            return c
    raise AssertionError(f"no chunk starts with {heading_prefix!r}")


DOC = """# Buchtitel

## 1. Erste Sektion

Inhalt unter Sektion eins. Erste Absatz mit etwas Text.

## 2. Zweite Sektion

Inhalt unter Sektion zwei. Erster Absatz.

Zweiter Absatz unter Sektion zwei, damit die Sektion mehr als einen Absatz hat.

### 2.1 Tiefe Teilsektion

Inhalt der tiefen Teilsektion, eigenständig.

## 3. Dritte Sektion

Inhalt unter Sektion drei. Kurzer Absatz.
"""


class SectionTrailFencepost(unittest.TestCase):
    def setUp(self):
        # Small budget so every heading forces a chunk boundary; no overlap
        # so chunk boundaries are exactly the heading positions.
        self.chunker = Chunker(max_chunk_tokens=64, overlap_tokens=0, min_chunk_tokens=1)

    def test_heading_boundary_chunk_keeps_own_section_not_next(self):
        chunks = self.chunker.chunk(DOC)
        c1 = _first_heading_chunk(chunks, "## 1. Erste Sektion")
        self.assertEqual(
            _deepest(c1),
            "1. Erste Sektion",
            "chunk opening at '## 1' must report its own section, not the next one",
        )
        c2 = _first_heading_chunk(chunks, "## 2. Zweite Sektion")
        self.assertEqual(_deepest(c2), "2. Zweite Sektion")

    def test_chunk_opening_at_deeper_heading_includes_it(self):
        chunks = self.chunker.chunk(DOC)
        c21 = _first_heading_chunk(chunks, "### 2.1 Tiefe Teilsektion")
        self.assertEqual(
            c21["metadata"]["section_titles"],
            ["Buchtitel", "2. Zweite Sektion", "2.1 Tiefe Teilsektion"],
        )

    def test_size_split_mid_section_keeps_own_deepest(self):
        # Long section body -> the token budget, not a heading, closes the
        # chunk; deepest stays the section active at the chunk's start.
        body = "\n\n".join(
            f"Absatz {i} mit eigenem Text und mehreren Woertern zum Fuellen." for i in range(12)
        )
        doc = f"# Buchtitel\n\n## 4. Vierte Sektion\n\n{body}\n\n## 5. Fuenfte Sektion\n\nKurzer Schluss.\n"
        chunks = self.chunker.chunk(doc)
        section4_chunks = [c for c in chunks if "Vierte Sektion" not in c["text"] and "Absatz " in c["text"]]
        self.assertTrue(section4_chunks, "fixture must produce body chunks")
        for c in section4_chunks:
            self.assertEqual(
                _deepest(c),
                "4. Vierte Sektion",
                "size-split chunk must keep its own section as deepest",
            )

    def test_deeper_heading_inside_chunk_does_not_hijack_trail(self):
        # Headings force boundaries unconditionally, so a deeper heading can
        # only land INSIDE a chunk via the final-chunk merge (below
        # min_chunk_tokens). The merged chunk keeps the trail snapshotted at
        # its OWN opening — the late '### 2.1' must not hijack it.
        chunker = Chunker(max_chunk_tokens=400, overlap_tokens=0, min_chunk_tokens=30)
        merged_doc = (
            "# Buchtitel\n\n## 2. Zweite Sektion\n\n"
            "Inhalt unter Sektion zwei. Erster Absatz mit etwas Text.\n\n"
            "Zweiter Absatz unter Sektion zwei mit weiteren Woertern.\n\n"
            "### 2.1 Tiefe Teilsektion\n\nKurzer Schluss.\n"
        )
        chunks = chunker.chunk(merged_doc)
        inside = [
            c
            for c in chunks
            if "### 2.1 Tiefe Teilsektion" in c["text"]
            and not c["text"].startswith("### 2.1")
        ]
        self.assertTrue(inside, "fixture must merge the tiny '### 2.1' tail into the section-2 chunk")
        self.assertEqual(_deepest(inside[0]), "2. Zweite Sektion")


if __name__ == "__main__":
    unittest.main()
