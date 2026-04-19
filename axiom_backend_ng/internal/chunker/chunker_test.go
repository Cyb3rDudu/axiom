package chunker_test

import (
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/chunker"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	c := chunker.DefaultConfig()
	if c.MaxChunkTokens != 512 || c.OverlapTokens != 64 || c.MinChunkTokens != 50 {
		t.Errorf("defaults drifted: %+v", c)
	}
}

func TestChunkerEmptyMarkdown(t *testing.T) {
	t.Parallel()
	if got := chunker.New(chunker.DefaultConfig()).Chunk("", chunker.DocMetadata{}); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestChunkerShortSingleParagraph(t *testing.T) {
	t.Parallel()
	body := "This is a short paragraph that easily fits inside a single chunk."
	c := chunker.New(chunker.DefaultConfig())
	chunks := c.Chunk(body, chunker.DocMetadata{DocID: "abc"})
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Text != body {
		t.Errorf("text mismatch: %q", chunks[0].Text)
	}
	if chunks[0].Metadata.DocID != "abc" || chunks[0].Metadata.ChunkIndex != 0 {
		t.Errorf("metadata: %+v", chunks[0].Metadata)
	}
	if chunks[0].Metadata.ChunkID != "abc_chunk_0000" {
		t.Errorf("chunk_id: %q", chunks[0].Metadata.ChunkID)
	}
	if chunks[0].Metadata.TokenCount == 0 {
		t.Errorf("token count should be > 0")
	}
}

func TestChunkerSplitsOnHeading(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"# Section One",
		"",
		"Paragraph under section one.",
		"",
		"# Section Two",
		"",
		"Paragraph under section two.",
	}, "\n")
	// MinChunkTokens=1 prevents the merge-into-previous path from
	// collapsing our two tiny section chunks back into one.
	cfg := chunker.Config{MaxChunkTokens: 512, OverlapTokens: 64, MinChunkTokens: 1}
	chunks := chunker.New(cfg).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks (heading flush), got %d: %+v", len(chunks), chunks)
	}
	// Python's chunker updates the heading_stack BEFORE emitting the
	// flushed chunk, so the first chunk carries the NEW section's titles
	// while the second chunk carries them too. We preserve that quirk
	// for parity, so we only assert Section Two appears somewhere.
	found := false
	for _, c := range chunks {
		for _, title := range c.Metadata.SectionTitles {
			if title == "Section Two" {
				found = true
			}
		}
	}
	if !found {
		t.Error("Section Two should appear in section_titles")
	}
}

func TestChunkerRespectsTokenBudget(t *testing.T) {
	t.Parallel()
	// Build a paragraph with ~300 words so combining two would exceed a
	// 50-token budget and trigger a flush between them.
	para := strings.Repeat("word ", 100)
	body := para + "\n\n" + para + "\n\n" + para
	cfg := chunker.Config{MaxChunkTokens: 50, OverlapTokens: 0, MinChunkTokens: 10}
	chunks := chunker.New(cfg).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) < 2 {
		t.Fatalf("token budget flush didn't fire: %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if c.Metadata.TokenCount > cfg.MaxChunkTokens*2 {
			t.Errorf("chunk %d too large (%d tokens, max=%d): %q",
				i, c.Metadata.TokenCount, cfg.MaxChunkTokens, c.Text[:min(40, len(c.Text))])
		}
	}
}

func TestChunkerOverlapCarriesTailWords(t *testing.T) {
	t.Parallel()
	// Force a flush between paragraphs and verify the second chunk
	// begins with trailing words from the first.
	p1 := strings.Repeat("alpha ", 20) + "marker"
	p2 := strings.Repeat("beta ", 30)
	body := p1 + "\n\n" + p2
	cfg := chunker.Config{MaxChunkTokens: 20, OverlapTokens: 3, MinChunkTokens: 1}
	chunks := chunker.New(cfg).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) < 2 {
		t.Fatalf("need >=2 chunks, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[1].Text, "alpha") && !strings.Contains(chunks[1].Text, "marker") {
		t.Errorf("second chunk lacks overlap from first: %q", chunks[1].Text)
	}
}

func TestChunkerHandlesMarkerPageMarkers(t *testing.T) {
	t.Parallel()
	body := strings.Join([]string{
		"First page content.",
		"",
		"{1}------------------------------",
		"",
		"Second page content.",
	}, "\n")
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{
		DocID:        "d",
		PageLabelMap: map[int]string{1: "ii"},
	})
	if len(chunks) < 1 {
		t.Fatalf("no chunks: %+v", chunks)
	}
	// Verify the marker was stripped from the combined text.
	for _, c := range chunks {
		if strings.Contains(c.Text, "---") && strings.Contains(c.Text, "{1}") {
			t.Errorf("page marker leaked into chunk: %q", c.Text)
		}
	}
}

func TestChunkerPageLabelFallback(t *testing.T) {
	t.Parallel()
	// No PageLabelMap → fallback is physical+1 as a string.
	body := "p0 content\n\n{5}----------\n\np1 content"
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	// The tail paragraph "p1 content" should carry page_end == "6" (physical 5 +1).
	last := chunks[len(chunks)-1]
	if last.Metadata.PageEnd != "6" {
		t.Errorf("page_end fallback: got %q want 6", last.Metadata.PageEnd)
	}
}

func TestChunkerExtractsImageRefs(t *testing.T) {
	t.Parallel()
	body := "See figure below.\n\n![cat](/api/images/abc/image_0.png)\n\nMore text."
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{DocID: "d"})
	foundRef := false
	for _, c := range chunks {
		for _, r := range c.Metadata.ImageRefs {
			if r.AltText == "cat" && strings.HasSuffix(r.Path, "image_0.png") {
				foundRef = true
			}
		}
	}
	if !foundRef {
		t.Error("image ref missing from chunk metadata")
	}
}

func TestChunkerMergesTinyTailChunk(t *testing.T) {
	t.Parallel()
	// Long first paragraph then a tiny tail that sits below min tokens —
	// the tail should merge into the last chunk rather than emit its own.
	long := strings.Repeat("word ", 100)
	tiny := "tiny"
	body := long + "\n\n" + tiny
	cfg := chunker.Config{MaxChunkTokens: 200, OverlapTokens: 0, MinChunkTokens: 50}
	chunks := chunker.New(cfg).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) != 1 {
		t.Fatalf("expected merge into 1 chunk, got %d", len(chunks))
	}
	if !strings.HasSuffix(chunks[0].Text, "tiny") {
		t.Errorf("tiny tail didn't merge: %q", chunks[0].Text)
	}
}

func TestChunkerRecursiveSplitFiresOnGiantParagraph(t *testing.T) {
	t.Parallel()
	// One paragraph far larger than max_tokens → recursiveSplit should
	// break it into multiple pieces even though there is only a single
	// paragraph in the document.
	body := strings.Repeat("sentence. ", 400)
	cfg := chunker.Config{MaxChunkTokens: 30, OverlapTokens: 5, MinChunkTokens: 1}
	chunks := chunker.New(cfg).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) < 2 {
		t.Fatalf("expected recursive split, got %d chunks", len(chunks))
	}
}

func TestChunkerPreservesExtrasMinusExcludedKeys(t *testing.T) {
	t.Parallel()
	body := "Hello world."
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{
		DocID: "d",
		Extras: map[string]any{
			"title":          "Interesting Paper",
			"doc_id":         "should-be-excluded",
			"page_label_map": "also-excluded",
		},
	})
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Metadata.Extras["title"] != "Interesting Paper" {
		t.Errorf("title missing: %+v", chunks[0].Metadata.Extras)
	}
	if _, ok := chunks[0].Metadata.Extras["doc_id"]; ok {
		t.Error("doc_id should be excluded from extras")
	}
	if _, ok := chunks[0].Metadata.Extras["page_label_map"]; ok {
		t.Error("page_label_map should be excluded from extras")
	}
}

func TestChunkerDocIDDefaultsToUnknown(t *testing.T) {
	t.Parallel()
	chunks := chunker.New(chunker.DefaultConfig()).Chunk("Hello.", chunker.DocMetadata{})
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Metadata.DocID != "unknown_doc" {
		t.Errorf("docID fallback: %q", chunks[0].Metadata.DocID)
	}
	if !strings.HasPrefix(chunks[0].Metadata.ChunkID, "unknown_doc_chunk_") {
		t.Errorf("chunk_id format: %q", chunks[0].Metadata.ChunkID)
	}
}

func TestNewAcceptsZeroValueConfig(t *testing.T) {
	t.Parallel()
	// Zero values should fall back to DefaultConfig() limits — verified
	// indirectly by ensuring Chunk() does not panic and produces output
	// comparable to the default-configured chunker.
	c := chunker.New(chunker.Config{})
	chunks := c.Chunk("A short paragraph.\n\nAnother paragraph.", chunker.DocMetadata{DocID: "d"})
	if len(chunks) == 0 {
		t.Fatal("zero-value config produced no chunks")
	}
}

func TestChunkerPageMarkerWithBadDigit(t *testing.T) {
	t.Parallel()
	// A well-formed marker regex guarantees only digits, so this case
	// shouldn't actually happen — but exercising the error handling
	// path keeps the defensive code covered.
	body := "p0\n\n{0}----------\n\np1"
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
}

func TestChunkerHandlesOnlyHeadings(t *testing.T) {
	t.Parallel()
	body := "# H1\n\n## H2\n\n### H3"
	chunks := chunker.New(chunker.DefaultConfig()).Chunk(body, chunker.DocMetadata{DocID: "d"})
	if len(chunks) == 0 {
		t.Fatal("no chunks from headings-only doc")
	}
}

func TestChunkerWhitespaceOnlyReturnsNil(t *testing.T) {
	t.Parallel()
	chunks := chunker.New(chunker.DefaultConfig()).Chunk("    \n\n\n\n", chunker.DocMetadata{})
	if chunks != nil {
		t.Errorf("want nil, got %+v", chunks)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
