"""#186 section-trail fencepost: a chunk's deepest section is the heading
under which its FIRST content sits — the trail state at chunk START, not
the state after the closing boundary.

Mutation barriers (each test names the defect it must survive):

  heading_boundary_chunk_keeps_own_section_not_next
        fencepost: when a split lands exactly on a heading, the OLD code
        updated the heading stack BEFORE emitting the closing chunk, so a
        chunk beginning with "## 1 ..." reported section "## 2 ..." as its
        deepest section (3,762 chunks / 82 books / 19.1% of heading-first
        chunks at issue-filing census; corpus-state dependent).

  size_split_mid_section_keeps_own_deepest
        the fix must not overcorrect: chunks closed by the token budget
        mid-section keep the section active at their own start. The
        fixture's LAST body chunk additionally closes at the next heading
        (fencepost path) — both must report their own section.

  deeper_heading_inside_chunk_does_not_hijack_trail
        a deeper heading (### x.y) landing INSIDE a chunk via the final-chunk
        merge must not become the chunk's deepest section either — first
        content wins.

  chunk_opening_at_deeper_heading_includes_it
        a chunk that OPENS at a deeper heading carries that heading as its
        deepest section (its first content sits under it).

Run: python3 -m pytest axiom_ng_runner/tests/test_chunker_section_trail.py
"""

import os
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
        # CHUNKER_MODE=paragraph in a dev shell would silently bypass the
        # token chunker under test — force the default.
        os.environ.pop("CHUNKER_MODE", None)
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
        # Long section body -> budget splits inside the section, and the
        # last body chunk closes at the '## 5' heading boundary (fencepost
        # path); deepest stays the section active at each chunk's own start.
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

    def test_overlap_path_first_non_overlap_content_decides(self):
        # Production default overlap_tokens=64: at a heading boundary the
        # closing chunk's last paragraph is recycled as an overlap fragment,
        # and the fragment opens the NEW chunk TOGETHER with the heading
        # (the heading split has already fired). The chunk's first TEXT is
        # previous-section fragment, but its first NON-overlap content is
        # the new heading — that decides the trail (contract wording).
        para = " ".join(f"Wort{i}" for i in range(70))  # > 64 words -> trimmed fragment
        doc = (
            "# Buchtitel\n\n## 1. Erste Sektion\n\n" + para + "\n\n"
            "## 2. Zweite Sektion\n\n" + para + "\n"
        )
        chunker = Chunker(max_chunk_tokens=512, overlap_tokens=64, min_chunk_tokens=1)
        chunks = chunker.chunk(doc)
        overlap_chunks = [
            c for c in chunks
            if c["text"].startswith("Wort") and "Wort69" in c["text"]
        ]
        self.assertTrue(overlap_chunks, "fixture must produce an overlap-carrying chunk")
        oc = overlap_chunks[0]
        self.assertIn(
            "## 2. Zweite Sektion", oc["text"],
            "fixture shape: fragment + heading open the new chunk together",
        )
        self.assertEqual(
            _deepest(oc),
            "2. Zweite Sektion",
            "first NON-overlap content (the opening heading) decides the trail",
        )


if __name__ == "__main__":
    unittest.main()
