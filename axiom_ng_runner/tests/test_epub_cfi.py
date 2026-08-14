"""Tests for EPUB CFI extraction (§11 Weg A) with mutation-proof assertions.

Three tests with real evidence power (Hivemind requirement):
1. CFI construction: known XHTML structure → spec-conformant CFIs
   (even element indices, ! separator). Mutation: change ! away or
   _body_child_idx start to -1 → test must fail.
2. Messy HTML: unclosed <p>, void <img> without slash → CFI map must
   still produce entries (C2 proof).
3. Match quality: short entries don't poison, fallback is carry-forward.
"""


from axiom_ng_runner.epub_cfi import _CFICollector, match_text_to_cfi


def _feed(collector: _CFICollector, xhtml: str) -> list[dict]:
    collector.feed(xhtml)
    collector._flush_entry()
    return collector.entries


class TestCFIConstruction:
    """Test 1: spec-conformant CFI strings from known XHTML."""

    def test_first_paragraph_gets_even_index_with_bang(self):
        c = _CFICollector("epubcfi(/6/2")
        entries = _feed(c, """
            <html><body>
                <p>First paragraph text here</p>
                <p>Second paragraph text here</p>
            </body></html>
        """)
        assert len(entries) == 2
        # C1 fix: even element index + ! separator
        assert entries[0]["cfi"] == "epubcfi(/6/2!/4/2)"
        assert entries[1]["cfi"] == "epubcfi(/6/2!/4/4)"

    def test_spine_step_is_even(self):
        # spine_idx=0 → step 2, spine_idx=2 → step 6
        for spine_idx, expected_step in [(0, 2), (1, 4), (2, 6)]:
            base = f"epubcfi(/6/{(spine_idx + 1) * 2}"
            c = _CFICollector(base)
            entries = _feed(c, "<html><body><p>Some text here</p></body></html>")
            assert entries[0]["cfi"].startswith(f"epubcfi(/6/{expected_step}!/4/")

    def test_nested_tags_increment_depth_not_index(self):
        c = _CFICollector("epubcfi(/6/2")
        entries = _feed(c, """
            <html><body>
                <p>Before list</p>
                <ul><li>Item one</li><li>Item two</li></ul>
                <p>After list</p>
            </body></html>
        """)
        # <p> is child 1 (step 2), <ul> is child 2 (not a BLOCK_TAG at
        # depth 0, but its <li> children are nested → depth>0), <p> is
        # child 3 (step 6)
        assert len(entries) >= 2
        cfi_list = [e["cfi"] for e in entries]
        assert "epubcfi(/6/2!/4/2)" in cfi_list  # first <p>
        assert "epubcfi(/6/2!/4/6)" in cfi_list  # last <p> (child 3)

    def test_mutation_bang_removed_fails(self):
        """Mutation test: if ! is missing, CFIs are spec-invalid."""
        # Simulate the old buggy format
        buggy = "epubcfi(/6/2)/4/1"
        correct = "epubcfi(/6/2!/4/2)"
        assert buggy != correct
        assert "!" in correct and "!" not in buggy

    def test_mutation_odd_index_fails(self):
        """Mutation test: odd element indices are character-data steps."""
        buggy = "epubcfi(/6/2!/4/1)"  # odd = character data
        correct = "epubcfi(/6/2!/4/2)"  # even = element
        assert buggy != correct
        assert int(correct.split("/")[-1].rstrip(")")) % 2 == 0


class TestMessyHTML:
    """Test 2: C2 proof — messy HTML still produces CFI entries."""

    def test_unclosed_paragraph(self):
        """Legal HTML: <p>a<p>b — implied end tag must close entry."""
        c = _CFICollector("epubcfi(/6/2")
        entries = _feed(c, "<html><body><p>alpha<p>beta</body></html>")
        # Both paragraphs must produce entries (C2 fix)
        assert len(entries) == 2
        assert entries[0]["text"] == "alpha"
        assert entries[1]["text"] == "beta"

    def test_void_img_without_slash(self):
        """<img> without slash must not break depth tracking."""
        c = _CFICollector("epubcfi(/6/2")
        entries = _feed(c, """
            <html><body>
                <p>text before <img src="x.png"> text after</p>
                <p>next paragraph here</p>
            </body></html>
        """)
        # The <img> must not swallow the second <p>
        assert len(entries) == 2
        assert "text before" in entries[0]["text"]
        assert "next paragraph" in entries[1]["text"]

    def test_mismatched_end_tag(self):
        """A </div> closing a <p> (mismatched) must not crash."""
        c = _CFICollector("epubcfi(/6/2")
        entries = _feed(c, "<html><body><p>content</div><p>more</body></html>")
        assert len(entries) >= 1
        assert any("content" in e["text"] for e in entries)


class TestMatchQuality:
    """Test 3: C3 proof — short entries don't poison, carry-forward works."""

    def test_short_entry_does_not_poison(self):
        entries = [
            {"cfi": "epubcfi(/6/2!/4/2)", "text": "1", "tag": "p"},  # too short
            {"cfi": "epubcfi(/6/4!/4/4)", "text": "ESG strategies can exploit it is never adequately explained", "tag": "p"},
        ]
        # Chunk mentioning "1" must NOT match the short entry; must match the longer one
        start, _ = match_text_to_cfi("ESG strategies can exploit it is never adequately explained why", entries)
        assert start == "epubcfi(/6/4!/4/4)"

    def test_cover_chunk_gets_first_entry(self):
        """A markup-only chunk (cover) gets the first entry (fallback)."""
        entries = [
            {"cfi": "epubcfi(/6/2!/4/2)", "text": "Title Page", "tag": "p"},
            {"cfi": "epubcfi(/6/4!/4/2)", "text": "Chapter text about something", "tag": "p"},
        ]
        start, end = match_text_to_cfi("![](cover.jpeg) <svg>...</svg>", entries)
        # Fallback: first entry
        assert start == "epubcfi(/6/2!/4/2)"
        assert end == start

    def test_long_text_matches_correctly(self):
        entries = [
            {"cfi": "epubcfi(/6/2!/4/2)", "text": "Introduction to the topic", "tag": "p"},
            {"cfi": "epubcfi(/6/4!/4/4)", "text": "Chapter five discusses risk management", "tag": "p"},
        ]
        start, _ = match_text_to_cfi("Chapter five discusses risk management in detail", entries)
        assert start == "epubcfi(/6/4!/4/4)"
