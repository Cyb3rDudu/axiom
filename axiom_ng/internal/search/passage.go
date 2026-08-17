// /api/passage/{chunk_id} — the A1 context primitive (#165): one round-trip
// delivers everything a client needs for a citation: the chunk (text,
// section, locator), its ±1 neighbors within the same attachment (book
// boundary respected — first/last chunks have at most one neighbor), and the
// unified SourceView block.
package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// ErrPassageNotFound is the plain 404 (chunk unknown to index AND database).
var ErrPassageNotFound = errors.New("passage: chunk not found")

// InactiveSnapshotError marks a chunk that exists in the DB but belongs to an
// inactive (superseded) snapshot — the OS index only carries active-snapshot
// chunks (outbox tombstones remove superseded ones), so the 404 can hint at
// the superseded state instead of saying "never existed".
type InactiveSnapshotError struct {
	ChunkID      string `json:"chunk_id"`
	SnapshotID   string `json:"snapshot_id"`
	AttachmentID string `json:"attachment_id"`
}

func (e *InactiveSnapshotError) Error() string {
	return fmt.Sprintf("passage: chunk %s belongs to inactive snapshot %s (attachment %s)", e.ChunkID, e.SnapshotID, e.AttachmentID)
}

// PassageNeighbor is one adjacent chunk (chunk_index ± 1, same attachment).
type PassageNeighbor struct {
	ChunkID    string      `json:"chunk_id"`
	ChunkIndex int         `json:"chunk_index"`
	Text       string      `json:"text"`
	Section    []string    `json:"section"`
	Locator    LocatorView `json:"locator"`
}

// Passage is the /api/passage/{id} response body.
type Passage struct {
	ChunkID      string            `json:"chunk_id"`
	DocumentID   string            `json:"document_id"`
	SnapshotID   string            `json:"snapshot_id"`
	AttachmentID string            `json:"attachment_id"`
	ChunkIndex   int               `json:"chunk_index"`
	Text         string            `json:"text"`
	Section      []string          `json:"section"`
	Locator      LocatorView       `json:"locator"`
	Source       repo.SourceView   `json:"source"`
	Neighbors    []PassageNeighbor `json:"neighbors"`
}

// osPost is the shared request path for passage queries (house style of
// osClient.search, minus the search-specific decoding).
func (c *osClient) osPost(ctx context.Context, path string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := c.base + "/" + IndexName + path
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		hreq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))
	}
	hres, err := c.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hres.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(hres.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if hres.StatusCode != http.StatusOK && hres.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("opensearch %s: status %d: %s", url, hres.StatusCode, truncateChars(string(rb), 300))
	}
	return rb, nil
}

// osGet is the read-side sibling of osPost (GET — POST on _doc/{id} would be
// an index-write attempt, which OpenSearch rightly rejects with a 400).
func (c *osClient) osGet(ctx context.Context, path string) ([]byte, error) {
	url := c.base + "/" + IndexName + path
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		hreq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))
	}
	hres, err := c.hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hres.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(hres.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if hres.StatusCode != http.StatusOK {
		// A 404 is "not found" ONLY for a doc-shaped body ({"found":false});
		// a missing/unavailable index answers 404 with an "error" root —
		// that is an outage and must surface as an error (→ route 503),
		// not as found:false.
		if hres.StatusCode == http.StatusNotFound {
			var probe struct {
				Error json.RawMessage `json:"error"`
			}
			if jerr := json.Unmarshal(rb, &probe); jerr == nil && len(probe.Error) > 0 && string(probe.Error) != "null" {
				return nil, fmt.Errorf("opensearch %s: status %d: %s", url, hres.StatusCode, truncateChars(string(rb), 300))
			}
			return rb, nil
		}
		return nil, fmt.Errorf("opensearch %s: status %d: %s", url, hres.StatusCode, truncateChars(string(rb), 300))
	}
	return rb, nil
}

// GetPassage resolves one chunk with neighbors and source hydration.
func (s *Service) GetPassage(ctx context.Context, chunkID string) (*Passage, error) {
	var got struct {
		Found bool `json:"found"`
		Inner struct {
			ChunkID      string          `json:"chunk_id"`
			DocumentID   string          `json:"document_id"`
			SnapshotID   string          `json:"snapshot_id"`
			AttachmentID string          `json:"attachment_id"`
			ChunkIndex   int             `json:"chunk_index"`
			Text         string          `json:"text"`
			Locator      json.RawMessage `json:"locator"`
			Sections     []string        `json:"section_titles"`
		} `json:"_source"`
	}
	rb, err := s.os.osGet(ctx, "/_doc/"+chunkID)
	if err != nil {
		return nil, fmt.Errorf("passage: fetch chunk: %w", err)
	}
	if err := json.Unmarshal(rb, &got); err != nil {
		return nil, fmt.Errorf("passage: decode chunk: %w", err)
	}
	if !got.Found {
		// OS only carries active snapshots: distinguish superseded from unknown.
		if probe, ok := s.docs.(repo.ChunkLivenessProbe); ok {
			if l, lerr := probe.ChunkLiveness(ctx, chunkID); lerr == nil && l != nil && !l.Active {
				return nil, &InactiveSnapshotError{ChunkID: chunkID, SnapshotID: l.SnapshotID, AttachmentID: l.AttachmentID}
			}
		}
		return nil, ErrPassageNotFound
	}
	c := got.Inner

	var meta repo.DocumentMeta
	if s.docs != nil {
		if m, err := s.docs.DocumentMetaByIDs(ctx, []string{c.DocumentID}); err != nil {
			s.log.Printf("passage: source hydration failed: %v", err)
		} else {
			meta = m[c.DocumentID] // zero value degrades to empty fields (#158 lesson)
		}
	}

	neighbors := s.fetchNeighbors(ctx, c.AttachmentID, c.ChunkIndex, c.ChunkID)

	return &Passage{
		ChunkID: c.ChunkID, DocumentID: c.DocumentID, SnapshotID: c.SnapshotID,
		AttachmentID: c.AttachmentID, ChunkIndex: c.ChunkIndex,
		Text: c.Text, Section: c.Sections,
		Locator:   locatorView(c.Locator, c.Sections),
		Source:    meta.View(c.DocumentID),
		Neighbors: neighbors,
	}, nil
}

// fetchNeighbors pulls chunk_index ± 1 within the SAME attachment (attachment
// = book file: the boundary is respected structurally — the first/last chunk
// of a book yields at most one neighbor). The center chunk is excluded IN THE
// QUERY (must_not) so size:2 never wastes a slot on it; the residual
// client-side check is defense in depth. Failures degrade to none (logged).
func (s *Service) fetchNeighbors(ctx context.Context, attachmentID string, idx int, centerChunkID string) []PassageNeighbor {
	lo, hi := idx-1, idx+1
	if lo < 0 {
		lo = 0
	}
	body := map[string]any{
		// The window [idx-1, idx+1] matches the CENTER chunk too — without the
		// must_not, OpenSearch truncates to size BEFORE the client-side
		// exclusion, so a middle chunk loses its +1 neighbor to its own entry.
		"size": 2,
		"query": map[string]any{"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]any{"attachment_id.keyword": attachmentID}},
				map[string]any{"range": map[string]any{"chunk_index": map[string]any{"gte": lo, "lte": hi}}},
			},
			"must_not": []any{
				map[string]any{"term": map[string]any{"chunk_id.keyword": centerChunkID}},
			},
		}},
		"_source": []string{"chunk_id", "chunk_index", "text", "section_titles", "locator"},
		"sort":    []map[string]any{{"chunk_index": "asc"}},
	}
	rb, err := s.os.osPost(ctx, "/_search", body)
	if err != nil {
		s.log.Printf("passage: neighbor fetch failed (serving without): %v", err)
		return nil
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				Source struct {
					ChunkID    string          `json:"chunk_id"`
					ChunkIndex int             `json:"chunk_index"`
					Text       string          `json:"text"`
					Sections   []string        `json:"section_titles"`
					Locator    json.RawMessage `json:"locator"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		s.log.Printf("passage: neighbor decode failed (serving without): %v", err)
		return nil
	}
	out := make([]PassageNeighbor, 0, 2)
	for _, h := range parsed.Hits.Hits {
		src := h.Source
		if src.ChunkID == "" || src.ChunkID == centerChunkID {
			continue
		}
		out = append(out, PassageNeighbor{
			ChunkID: src.ChunkID, ChunkIndex: src.ChunkIndex, Text: src.Text,
			Section: src.Sections, Locator: locatorView(src.Locator, src.Sections),
		})
	}
	return out
}

// KGSources hydrates repo.SourceView for chunk ids (A1 Ziel 2: the unified
// source block on KG evidence). Chunk→document comes from the index (one
// mget), metadata from the DocSource — one DB round-trip for all chunks.
func (s *Service) KGSources(ctx context.Context, chunkIDs []string) (map[string]repo.SourceView, error) {
	out := make(map[string]repo.SourceView, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	var got struct {
		Docs []struct {
			Found  bool `json:"found"`
			Source struct {
				ChunkID    string `json:"chunk_id"`
				DocumentID string `json:"document_id"`
			} `json:"_source"`
		} `json:"docs"`
	}
	docs := make([]map[string]any, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		docs = append(docs, map[string]any{"_id": id, "_source": []string{"chunk_id", "document_id"}})
	}
	rb, err := s.os.osPost(ctx, "/_mget", map[string]any{"docs": docs})
	if err != nil {
		return nil, fmt.Errorf("kg sources: mget: %w", err)
	}
	if err := json.Unmarshal(rb, &got); err != nil {
		return nil, fmt.Errorf("kg sources: decode: %w", err)
	}
	docIDs := make([]string, 0, len(got.Docs))
	byChunk := make(map[string]string, len(got.Docs))
	for _, d := range got.Docs {
		if d.Found && d.Source.ChunkID != "" && d.Source.DocumentID != "" {
			byChunk[d.Source.ChunkID] = d.Source.DocumentID
			docIDs = append(docIDs, d.Source.DocumentID)
		}
	}
	if s.docs == nil || len(docIDs) == 0 {
		return out, nil
	}
	meta, err := s.docs.DocumentMetaByIDs(ctx, docIDs)
	if err != nil {
		return nil, fmt.Errorf("kg sources: hydration: %w", err)
	}
	for chunkID, docID := range byChunk {
		out[chunkID] = meta[docID].View(docID)
	}
	return out, nil
}
