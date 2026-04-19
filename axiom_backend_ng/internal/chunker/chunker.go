// Package chunker is the Go port of axiom_backend/ai_researcher/core_rag/chunker.py.
// It splits a markdown document into overlapping, section-aware,
// token-budgeted chunks matching the Python implementation's behaviour
// closely enough that the resulting chunk_ids can slot into the
// existing documents schema.
//
// Port status: the token-based chunking path (the production default)
// is ported in full. Paragraph-mode is intentionally omitted — it is a
// legacy compatibility path and the migration plan drops it.
package chunker

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultConfig matches the Python Chunker defaults in
// core_rag/chunker.py:21-29.
func DefaultConfig() Config {
	return Config{
		MaxChunkTokens: 512,
		OverlapTokens:  64,
		MinChunkTokens: 50,
	}
}

// Config tunes the chunker. See core_rag/chunker.py:21-29.
type Config struct {
	MaxChunkTokens int
	OverlapTokens  int
	MinChunkTokens int
}

// Chunk is the Go equivalent of a chunker.py chunk dict. Text is the
// chunk body and Metadata carries the ancillary fields the Python
// version emits: chunk_id, chunk_index, paragraph range, section
// hierarchy, image refs, page range.
type Chunk struct {
	Text     string
	Metadata ChunkMetadata
}

// ChunkMetadata mirrors the keys the Python _create_chunk helper
// populates. Keys not represented here ("doc_id") live on the parent
// Chunk via the DocID passed to Chunk().
type ChunkMetadata struct {
	DocID               string     `json:"doc_id"`
	ChunkID             string     `json:"chunk_id"`
	ChunkIndex          int        `json:"chunk_index"`
	StartParagraphIndex int        `json:"start_paragraph_index"`
	EndParagraphIndex   int        `json:"end_paragraph_index"`
	SectionTitles       []string   `json:"section_titles"`
	TokenCount          int        `json:"token_count"`
	ImageRefs           []ImageRef `json:"image_refs"`
	PageStart           string     `json:"page_start"`
	PageEnd             string     `json:"page_end"`
	// Extras are pass-through fields from doc_metadata (minus the keys
	// Python excludes). Preserved so callers can propagate title,
	// authors, etc. through to the chunk row without changing the
	// metadata schema.
	Extras map[string]any `json:"-"`
}

// ImageRef is one markdown image reference inside a chunk.
type ImageRef struct {
	AltText  string `json:"alt_text"`
	Path     string `json:"path"`
	Position int    `json:"position"`
}

// Regexes — compiled once and reused across calls. Expressions ported
// verbatim from the Python module.
var (
	pageMarkerRe       = regexp.MustCompile(`^\{(\d+)\}-{10,}$`)
	paragraphSplitRe   = regexp.MustCompile(`(\n\s*\n+)`)
	imageRe            = regexp.MustCompile(`!\[([^\]]*)\]\(([^\)]+)\)`)
	headingFirstLineRe = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
)

// Chunker holds one configured instance. Safe for concurrent use — the
// config fields are read-only after construction.
type Chunker struct {
	cfg Config
}

// New returns a Chunker populated with cfg. Zero-value fields fall back
// to DefaultConfig values.
func New(cfg Config) *Chunker {
	def := DefaultConfig()
	if cfg.MaxChunkTokens <= 0 {
		cfg.MaxChunkTokens = def.MaxChunkTokens
	}
	if cfg.OverlapTokens < 0 {
		cfg.OverlapTokens = def.OverlapTokens
	}
	if cfg.MinChunkTokens <= 0 {
		cfg.MinChunkTokens = def.MinChunkTokens
	}
	return &Chunker{cfg: cfg}
}

// DocMetadata is the subset of document-level fields the chunker
// consumes. DocID is required for chunk_id derivation; PageLabelMap
// translates Marker physical page indices to logical labels; Extras
// are merged into every emitted chunk's metadata.
type DocMetadata struct {
	DocID        string
	PageLabelMap map[int]string
	Extras       map[string]any
}

// Chunk splits markdown into structure-aware chunks. Matches
// chunker.py::Chunker.chunk.
func (c *Chunker) Chunk(markdown string, doc DocMetadata) []Chunk {
	if markdown == "" {
		return nil
	}

	// First pass: full-content heading hierarchy.
	paragraphs := splitParagraphs(markdown)
	if len(paragraphs) == 0 {
		return nil
	}
	headings := extractHeadingHierarchy(paragraphs)

	// Strip Marker page markers and build paragraph → page mapping.
	clean, paraToPage := stripPageMarkers(paragraphs, doc.PageLabelMap)
	if len(clean) == 0 {
		return nil
	}

	// Re-extract headings if the stripping changed paragraph indices.
	if len(clean) != len(paragraphs) {
		headings = extractHeadingHierarchy(clean)
	}

	return c.chunkTokenBased(clean, headings, paraToPage, doc)
}

// chunkTokenBased is the port of Chunker._chunk_token_based.
// Intentionally preserves the Python logic's sequence of decisions so
// the resulting chunks line up closely with recorded golden outputs.
func (c *Chunker) chunkTokenBased(
	paragraphs []string,
	paragraphHeadings map[int][]string,
	paragraphToPage map[int]string,
	doc DocMetadata,
) []Chunk {
	docID := doc.DocID
	if docID == "" {
		docID = "unknown_doc"
	}
	var (
		chunks        []Chunk
		current       []string
		currentTokens int
		startIdx      int
		headingStack  []string
		pageStart     = pageAt(paragraphToPage, 0, "1")
		pageEnd       = pageStart
		chunkCounter  int
	)

	emit := func(endIdx int) {
		if len(current) == 0 {
			return
		}
		text := strings.Join(current, "\n\n")
		tokens := countTokens(text)
		if tokens > c.cfg.MaxChunkTokens {
			for _, sub := range c.recursiveSplit(text) {
				chunks = append(chunks, c.buildChunk(sub, docID, chunkCounter, startIdx, endIdx, headingStack, pageStart, pageEnd, doc))
				chunkCounter++
			}
		} else {
			chunks = append(chunks, c.buildChunk(text, docID, chunkCounter, startIdx, endIdx, headingStack, pageStart, pageEnd, doc))
			chunkCounter++
		}
	}

	for i, para := range paragraphs {
		paraTokens := countTokens(para)
		paraHeadings := paragraphHeadings[i]
		firstLine := firstNonEmptyLine(para)
		isHeading := headingFirstLineRe.MatchString(firstLine)

		shouldFlush := false
		if isHeading && len(current) > 0 {
			shouldFlush = true
			headingStack = paraHeadings
		} else if currentTokens+paraTokens > c.cfg.MaxChunkTokens && len(current) > 0 {
			shouldFlush = true
		}

		if shouldFlush {
			emit(startIdx + len(current) - 1)

			// Reset page tracking.
			pageStart = pageAt(paragraphToPage, i, pageEnd)
			pageEnd = pageStart

			// Start next chunk with optional overlap from the tail of
			// the previous one.
			if c.cfg.OverlapTokens > 0 && len(current) > 0 {
				last := current[len(current)-1]
				words := strings.Fields(last)
				if len(words) > c.cfg.OverlapTokens {
					overlap := strings.Join(words[len(words)-c.cfg.OverlapTokens:], " ")
					current = []string{overlap}
					currentTokens = countTokens(overlap)
				} else {
					current = nil
					currentTokens = 0
				}
			} else {
				current = nil
				currentTokens = 0
			}
			startIdx = i
		}

		current = append(current, para)
		currentTokens += paraTokens

		// Track page range for the current chunk. The Python code uses
		// len(current)==1 to seed pageStart at the first paragraph.
		pPage := pageAt(paragraphToPage, i, pageEnd)
		if len(current) == 1 {
			pageStart = pPage
		}
		pageEnd = pPage

		if !isHeading {
			headingStack = paraHeadings
		}
	}

	// Final chunk — maybe merge into the previous one if undersized.
	if len(current) > 0 {
		text := strings.Join(current, "\n\n")
		tokens := countTokens(text)
		endIdx := startIdx + len(current) - 1

		if tokens < c.cfg.MinChunkTokens && len(chunks) > 0 {
			last := &chunks[len(chunks)-1]
			last.Text = last.Text + "\n\n" + text
			last.Metadata.EndParagraphIndex = endIdx
			last.Metadata.TokenCount = countTokens(last.Text)
			last.Metadata.PageEnd = pageEnd
		} else if tokens > c.cfg.MaxChunkTokens {
			for _, sub := range c.recursiveSplit(text) {
				chunks = append(chunks, c.buildChunk(sub, docID, chunkCounter, startIdx, endIdx, headingStack, pageStart, pageEnd, doc))
				chunkCounter++
			}
		} else {
			chunks = append(chunks, c.buildChunk(text, docID, chunkCounter, startIdx, endIdx, headingStack, pageStart, pageEnd, doc))
		}
	}
	return chunks
}

// buildChunk mirrors Chunker._create_chunk.
func (c *Chunker) buildChunk(
	text, docID string, index int,
	startPara, endPara int,
	sectionTitles []string,
	pageStart, pageEnd string,
	doc DocMetadata,
) Chunk {
	refs := extractImageRefs(text)
	meta := ChunkMetadata{
		DocID:               docID,
		ChunkID:             fmt.Sprintf("%s_chunk_%04d", docID, index),
		ChunkIndex:          index,
		StartParagraphIndex: startPara,
		EndParagraphIndex:   endPara,
		SectionTitles:       append([]string{}, sectionTitles...),
		TokenCount:          countTokens(text),
		ImageRefs:           refs,
		PageStart:           pageStart,
		PageEnd:             pageEnd,
	}
	if len(doc.Extras) > 0 {
		meta.Extras = make(map[string]any, len(doc.Extras))
		for k, v := range doc.Extras {
			if k == "doc_id" || k == "page_label_map" {
				continue
			}
			meta.Extras[k] = v
		}
	}
	return Chunk{Text: text, Metadata: meta}
}

// recursiveSplit is the port of Chunker._recursive_split. Separators
// hierarchy and overlap semantics match the Python implementation.
func (c *Chunker) recursiveSplit(text string) []string {
	if countTokens(text) <= c.cfg.MaxChunkTokens {
		return []string{text}
	}
	type sep struct {
		s string
	}
	separators := []sep{
		{"\n\n"}, {"\n"}, {". "}, {"; "}, {", "},
	}
	for _, sp := range separators {
		if !strings.Contains(text, sp.s) {
			continue
		}
		parts := strings.Split(text, sp.s)
		if len(parts) <= 1 {
			continue
		}
		mid := len(parts) / 2
		var leftText, rightText string
		for i := mid; i > 0; i-- {
			candidate := strings.Join(parts[:i], sp.s)
			if countTokens(candidate) <= c.cfg.MaxChunkTokens {
				leftText = candidate
				rightText = strings.Join(parts[i:], sp.s)
				break
			}
		}
		if leftText == "" {
			for i := 1; i < len(parts); i++ {
				candidate := strings.Join(parts[:i], sp.s)
				if countTokens(candidate) > c.cfg.MaxChunkTokens {
					if i > 1 {
						leftText = strings.Join(parts[:i-1], sp.s)
					} else {
						leftText = parts[0]
					}
					start := i - 1
					if start < 1 {
						start = 1
					}
					rightText = strings.Join(parts[start:], sp.s)
					break
				}
			}
		}
		if leftText != "" && rightText != "" {
			if c.cfg.OverlapTokens > 0 {
				leftWords := strings.Fields(leftText)
				take := c.cfg.OverlapTokens
				if take > len(leftWords) {
					take = len(leftWords)
				}
				overlap := strings.Join(leftWords[len(leftWords)-take:], " ")
				rightText = overlap + " " + rightText
			}
			out := c.recursiveSplit(leftText)
			out = append(out, c.recursiveSplit(rightText)...)
			return out
		}
	}
	// Fallback: binary split at word midpoint.
	words := strings.Fields(text)
	if len(words) > c.cfg.MaxChunkTokens {
		midW := len(words) / 2
		left := strings.Join(words[:midW], " ")
		var right string
		if c.cfg.OverlapTokens > 0 && midW-c.cfg.OverlapTokens >= 0 {
			right = strings.Join(words[midW-c.cfg.OverlapTokens:], " ")
		} else {
			right = strings.Join(words[midW:], " ")
		}
		out := c.recursiveSplit(left)
		out = append(out, c.recursiveSplit(right)...)
		return out
	}
	return []string{text}
}

// countTokens approximates token count with the same len(.split) * 1.3
// heuristic Python uses.
func countTokens(s string) int {
	return int(float64(len(strings.Fields(s))) * 1.3)
}

// splitParagraphs reproduces the Python split logic.
func splitParagraphs(md string) []string {
	// The Python code uses re.split with a capturing group so separators
	// are retained, then joins each paragraph with its trailing
	// whitespace before stripping. We reimplement that here.
	idx := paragraphSplitRe.FindAllStringIndex(md, -1)
	if len(idx) == 0 {
		if s := strings.TrimSpace(md); s != "" {
			return []string{s}
		}
		return nil
	}
	var paragraphs []string
	current := ""
	cursor := 0
	for _, m := range idx {
		start, end := m[0], m[1]
		// text between cursor and start is the paragraph body; the
		// match itself is the separator.
		current += md[cursor:start]
		// The Python version appends the separator onto the paragraph
		// before stripping, so effectively the paragraph body is
		// whatever preceded the separator.
		if stripped := strings.TrimSpace(current); stripped != "" {
			paragraphs = append(paragraphs, stripped)
		}
		current = ""
		cursor = end
	}
	current += md[cursor:]
	if stripped := strings.TrimSpace(current); stripped != "" {
		paragraphs = append(paragraphs, stripped)
	}
	return paragraphs
}

// extractHeadingHierarchy ports _extract_heading_hierarchy.
func extractHeadingHierarchy(paragraphs []string) map[int][]string {
	stack := [][2]string{} // level(as single-char int) + title
	out := make(map[int][]string, len(paragraphs))
	for i, para := range paragraphs {
		first := firstNonEmptyLine(para)
		if m := headingFirstLineRe.FindStringSubmatch(first); m != nil {
			level := len(m[1])
			title := strings.TrimSpace(m[2])
			for len(stack) > 0 && len(stack[len(stack)-1][0]) >= level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, [2]string{strings.Repeat("#", level), title})
		}
		titles := make([]string, 0, len(stack))
		for _, s := range stack {
			titles = append(titles, s[1])
		}
		out[i] = titles
	}
	return out
}

// stripPageMarkers removes Marker page-break paragraphs (e.g.
// "{3}------------") and returns the clean list plus paragraph→page
// mapping. labelMap is optional; missing entries default to
// str(physical_idx + 1) for parity with the Python fallback.
func stripPageMarkers(paragraphs []string, labelMap map[int]string) ([]string, map[int]string) {
	clean := make([]string, 0, len(paragraphs))
	paraToPage := make(map[int]string, len(paragraphs))
	currentPage := "1"
	for _, para := range paragraphs {
		if m := pageMarkerRe.FindStringSubmatch(strings.TrimSpace(para)); m != nil {
			var physical int
			if _, err := fmt.Sscanf(m[1], "%d", &physical); err != nil {
				continue
			}
			if v, ok := labelMap[physical]; ok {
				currentPage = v
			} else {
				currentPage = fmt.Sprintf("%d", physical+1)
			}
			continue
		}
		paraToPage[len(clean)] = currentPage
		clean = append(clean, para)
	}
	return clean, paraToPage
}

// extractImageRefs ports _extract_images_from_text.
func extractImageRefs(text string) []ImageRef {
	matches := imageRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]ImageRef, 0, len(matches))
	for _, m := range matches {
		out = append(out, ImageRef{
			AltText:  text[m[2]:m[3]],
			Path:     text[m[4]:m[5]],
			Position: m[0],
		})
	}
	return out
}

func firstNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func pageAt(m map[int]string, i int, def string) string {
	if v, ok := m[i]; ok {
		return v
	}
	return def
}
