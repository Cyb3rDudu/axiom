package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	opensearchapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ChunkDoc is the shape of a chunk as indexed in OpenSearch.
// Mirrors the `doc` dict built in
// axiom_backend/ai_researcher/core_rag/opensearch_store.py::add_chunks.
//
// section_titles is a single space-joined string (not the list form
// used in Postgres metadata) because the OpenSearch mapping uses
// `type: text` with the standard analyzer — same as Python.
type ChunkDoc struct {
	ChunkID       string         `json:"chunk_id"`
	DocID         string         `json:"doc_id"`
	ChunkText     string         `json:"chunk_text"`
	SectionTitles string         `json:"section_titles"`
	ChunkIndex    int            `json:"chunk_index"`
	TokenCount    int            `json:"token_count"`
	Metadata      map[string]any `json:"metadata"`
}

// EnsureIndex creates the configured index with the canonical Python
// mapping when it does not already exist. No-op if the index is
// present. Safe to call on every ingest.
func (c *Client) EnsureIndex(ctx context.Context) error {
	if c == nil || c.os == nil {
		return ErrDisabled
	}
	exists, err := c.indexExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	body := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"chunk_id": map[string]any{"type": "keyword"},
				"doc_id":   map[string]any{"type": "keyword"},
				"chunk_text": map[string]any{
					"type":            "text",
					"analyzer":        "standard",
					"search_analyzer": "standard",
				},
				"section_titles": map[string]any{
					"type":     "text",
					"analyzer": "standard",
				},
				"chunk_index": map[string]any{"type": "integer"},
				"token_count": map[string]any{"type": "integer"},
				"metadata":    map[string]any{"type": "object", "enabled": true},
			},
		},
		"settings": map[string]any{
			"number_of_shards":   1,
			"number_of_replicas": 0,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opensearch: marshal index body: %w", err)
	}
	req := opensearchapi.IndicesCreateReq{
		Index: c.cfg.Index,
		Body:  bytes.NewReader(buf),
	}
	resp, err := c.os.Indices.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("opensearch: create index: %w", err)
	}
	return checkStatus(resp, "create index")
}

// IndexChunks upserts every ChunkDoc into the configured index and
// refreshes so the next BM25Search sees them immediately — matches
// Python add_chunks which ends with indices.refresh.
//
// Empty input is a no-op (still refreshes so the caller gets parity).
func (c *Client) IndexChunks(ctx context.Context, docs []ChunkDoc) error {
	if c == nil || c.os == nil {
		return ErrDisabled
	}
	if err := c.EnsureIndex(ctx); err != nil {
		return err
	}
	for _, d := range docs {
		if d.ChunkID == "" {
			continue
		}
		buf, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("opensearch: marshal chunk %s: %w", d.ChunkID, err)
		}
		req := opensearchapi.IndexReq{
			Index:      c.cfg.Index,
			DocumentID: d.ChunkID,
			Body:       bytes.NewReader(buf),
		}
		resp, err := c.os.Index(ctx, req)
		if err != nil {
			return fmt.Errorf("opensearch: index chunk %s: %w", d.ChunkID, err)
		}
		if err := checkStatus(resp, "index chunk "+d.ChunkID); err != nil {
			return err
		}
	}
	refreshReq := opensearchapi.IndicesRefreshReq{
		Indices: []string{c.cfg.Index},
	}
	resp, err := c.os.Indices.Refresh(ctx, &refreshReq)
	if err != nil {
		return fmt.Errorf("opensearch: refresh index: %w", err)
	}
	return checkStatus(resp, "refresh index")
}

// DeleteDocument removes every chunk for a given doc_id via
// delete_by_query, matching Python's `delete_document`.
func (c *Client) DeleteDocument(ctx context.Context, docID uuid.UUID) error {
	if c == nil || c.os == nil {
		return ErrDisabled
	}
	body := map[string]any{
		"query": map[string]any{
			"term": map[string]any{"doc_id": docID.String()},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opensearch: marshal delete body: %w", err)
	}
	req := opensearchapi.DocumentDeleteByQueryReq{
		Indices: []string{c.cfg.Index},
		Body:    bytes.NewReader(buf),
	}
	resp, err := c.os.Document.DeleteByQuery(ctx, req)
	if err != nil {
		return fmt.Errorf("opensearch: delete_by_query: %w", err)
	}
	return checkStatus(resp, "delete_by_query")
}

// indexExists returns true when the configured index is present.
// opensearch-go's Indices.Exists issues HEAD /{index} and surfaces a
// non-200 as an error. 404 → (false, nil); anything else is an error.
func (c *Client) indexExists(ctx context.Context) (bool, error) {
	req := opensearchapi.IndicesExistsReq{Indices: []string{c.cfg.Index}}
	resp, err := c.os.Indices.Exists(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("opensearch: indices.exists: %w", err)
	}
	if resp == nil {
		return false, nil
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("opensearch: indices.exists status %d: %s", resp.StatusCode, string(b))
	}
}

// checkStatus verifies an OpenSearch response succeeded; used by the
// write-side calls where a 4xx/5xx is always an error.
func checkStatus(resp any, op string) error {
	type inspectable interface {
		Inspect() opensearchapi.Inspect
	}
	ins, ok := resp.(inspectable)
	if !ok {
		return nil
	}
	raw := ins.Inspect().Response
	if raw == nil {
		return nil
	}
	if raw.StatusCode >= 400 {
		b, _ := io.ReadAll(raw.Body)
		return fmt.Errorf("opensearch: %s status %d: %s", op, raw.StatusCode, string(b))
	}
	return nil
}
