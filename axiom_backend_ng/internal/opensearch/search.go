package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	opensearchapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Hit is the BM25 result row shape. Mirrors the Python response items
// in opensearch_store.py: one chunk per hit, scored by OpenSearch's
// native _score.
type Hit struct {
	ChunkID       string         `json:"chunk_id"`
	DocID         uuid.UUID      `json:"doc_id"`
	Text          string         `json:"text"`
	Score         float64        `json:"score"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	SectionTitles string         `json:"section_titles,omitempty"`
	// Raw holds the full _source map in case the caller wants extra
	// fields that aren't in the typed shape.
	Raw map[string]any `json:"-"`
}

// SearchOptions drives BM25Search. DocIDs constrains results to that
// slice; Size is the `size` parameter (default 30).
type SearchOptions struct {
	Query  string
	DocIDs []uuid.UUID
	Size   int
}

// BM25Search executes the canonical Python query body against the
// configured index. Returns Hits ordered by score DESC (OpenSearch's
// default). When OpenSearch is unreachable or returns an error, the
// function returns a nil slice and the underlying error — the handler
// decides whether to surface 503 or fall back.
func (c *Client) BM25Search(ctx context.Context, opt SearchOptions) ([]Hit, error) {
	if c == nil || c.os == nil {
		return nil, ErrDisabled
	}
	if opt.Size <= 0 {
		opt.Size = 30
	}

	body := buildBM25Body(opt)
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("opensearch: marshal query: %w", err)
	}

	req := opensearchapi.SearchReq{
		Indices: []string{c.cfg.Index},
		Body:    bytes.NewReader(buf),
	}
	resp, err := c.os.Search(ctx, &req)
	if err != nil {
		return nil, fmt.Errorf("opensearch: search: %w", err)
	}
	return decodeHits(resp)
}

// Health pings the cluster. Matches axiom_backend/ai_researcher/core_rag/
// opensearch_store.py:health_check, which just verifies the cluster
// responds.
func (c *Client) Health(ctx context.Context) error {
	if c == nil || c.os == nil {
		return ErrDisabled
	}
	resp, err := c.os.Info(ctx, nil)
	if err != nil {
		return fmt.Errorf("opensearch: info: %w", err)
	}
	if resp != nil {
		raw := resp.Inspect().Response
		if raw != nil && raw.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("opensearch: unhealthy status %d", raw.StatusCode)
		}
	}
	return nil
}

// buildBM25Body produces the exact query body the Python backend sends.
// The shape is pinned in opensearch_store.py and matched in tests.
func buildBM25Body(opt SearchOptions) map[string]any {
	must := []map[string]any{
		{
			"match": map[string]any{
				"chunk_text": map[string]any{
					"query":                opt.Query,
					"operator":             "or",
					"minimum_should_match": "50%",
				},
			},
		},
	}
	filter := []map[string]any{}
	if len(opt.DocIDs) > 0 {
		docIDs := make([]string, len(opt.DocIDs))
		for i, id := range opt.DocIDs {
			docIDs[i] = id.String()
		}
		filter = append(filter, map[string]any{
			"terms": map[string]any{"doc_id": docIDs},
		})
	}
	return map[string]any{
		"size": opt.Size,
		"query": map[string]any{
			"bool": map[string]any{
				"must":   must,
				"filter": filter,
			},
		},
		"_source": []string{"chunk_id", "doc_id", "chunk_text", "metadata", "section_titles"},
	}
}

// searchResponse models the minimal subset of the OpenSearch response
// body axiom-ng cares about.
type searchResponse struct {
	Hits struct {
		Hits []struct {
			Score  float64        `json:"_score"`
			Source map[string]any `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// decodeHits parses the response body into []Hit. Handles the
// opensearch-go response wrapper which exposes an io.Reader.
func decodeHits(resp any) ([]Hit, error) {
	type inspectable interface {
		Inspect() opensearchapi.Inspect
	}
	ins, ok := resp.(inspectable)
	if !ok {
		return nil, fmt.Errorf("opensearch: unknown response type %T", resp)
	}
	raw := ins.Inspect().Response
	if raw == nil {
		return nil, fmt.Errorf("opensearch: empty response")
	}
	if raw.StatusCode >= 400 {
		b, _ := io.ReadAll(raw.Body)
		return nil, fmt.Errorf("opensearch: status %d: %s", raw.StatusCode, string(b))
	}
	defer func() { _ = raw.Body.Close() }()
	var body searchResponse
	if err := json.NewDecoder(raw.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("opensearch: decode response: %w", err)
	}
	out := make([]Hit, 0, len(body.Hits.Hits))
	for _, h := range body.Hits.Hits {
		hit := Hit{
			Score: h.Score,
			Raw:   h.Source,
		}
		if s, ok := h.Source["chunk_id"].(string); ok {
			hit.ChunkID = s
		}
		if s, ok := h.Source["chunk_text"].(string); ok {
			hit.Text = s
		}
		if s, ok := h.Source["section_titles"].(string); ok {
			hit.SectionTitles = s
		}
		if s, ok := h.Source["doc_id"].(string); ok {
			if id, err := uuid.Parse(s); err == nil {
				hit.DocID = id
			}
		}
		if m, ok := h.Source["metadata"].(map[string]any); ok {
			hit.Metadata = m
		}
		out = append(out, hit)
	}
	return out, nil
}
