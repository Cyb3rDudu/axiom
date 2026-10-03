package processor

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Query-side runner endpoints (contract §7a, epic #130 R1 #131 / R2 #132).
// These reuse the same Client/transport/per-call budgets as the ingest
// routes; the runner serves them from warm singletons, so warm latency is
// tens of milliseconds while a COLD first call pays the model load (tens of
// seconds) — the budgets below cover cold loads so the first search after a
// runner restart succeeds instead of degrading.

const (
	budgetEmbed  = 45 * time.Second // cold BGE-M3 load ~30s, warm ~50ms
	budgetRerank = 45 * time.Second // cold reranker load ~15s, warm 20 pairs ~6s (MPS)
)

// EmbedRequest is POST /v1/embed (contract §7a).
type EmbedRequest struct {
	ContractVersion string   `json:"contract_version"`
	Texts           []string `json:"texts"`
	// IncludeSparse (R5 #135) additionally returns learned lexical weights.
	IncludeSparse bool `json:"include_sparse,omitempty"`
}

// EmbedResponse is POST /v1/embed (contract §7a).
type EmbedResponse struct {
	ContractVersion string      `json:"contract_version"`
	Model           string      `json:"model"`
	Dimensions      int         `json:"dimensions"`
	Embeddings      [][]float32 `json:"embeddings"`
	// Sparse is present iff the request set include_sparse: one
	// {token: weight} map per text (R5 #135).
	Sparse []map[string]float64 `json:"sparse,omitempty"`
}

// RerankRequest is POST /v1/rerank (contract §7a).
type RerankRequest struct {
	ContractVersion string   `json:"contract_version"`
	Query           string   `json:"query"`
	Texts           []string `json:"texts"`
	TopN            int      `json:"top_n"`
}

// RerankScore is one entry of the rerank response scores.
type RerankScore struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// RerankResponse is POST /v1/rerank (contract §7a).
type RerankResponse struct {
	ContractVersion string        `json:"contract_version"`
	Model           string        `json:"model"`
	Scores          []RerankScore `json:"scores"`
}

// EmbedQueries returns dense query embeddings for texts (runner warm path).
func (c *Client) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	var out EmbedResponse
	if err := c.do(ctx, budgetEmbed, http.MethodPost, "/v1/embed", &EmbedRequest{
		ContractVersion: ContractVersion,
		Texts:           texts,
	}, &out); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: got %d embeddings for %d texts", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// EmbedQueriesSparse returns dense embeddings AND learned lexical weights
// for the query texts in one runner call (R5 #135 sparse arm).
func (c *Client) EmbedQueriesSparse(ctx context.Context, texts []string) ([][]float32, []map[string]float64, error) {
	var out EmbedResponse
	if err := c.do(ctx, budgetEmbed, http.MethodPost, "/v1/embed", &EmbedRequest{
		ContractVersion: ContractVersion,
		Texts:           texts,
		IncludeSparse:   true,
	}, &out); err != nil {
		return nil, nil, fmt.Errorf("embed sparse: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, nil, fmt.Errorf("embed sparse: got %d embeddings for %d texts", len(out.Embeddings), len(texts))
	}
	if len(out.Sparse) != len(texts) {
		return nil, nil, fmt.Errorf("embed sparse: got %d sparse maps for %d texts", len(out.Sparse), len(texts))
	}
	return out.Embeddings, out.Sparse, nil
}

// Rerank returns cross-encoder scores for the (query, texts) pairs;
// scores[i].index refers to texts[i]. Ordering is the caller's concern.
func (c *Client) Rerank(ctx context.Context, query string, texts []string, topN int) ([]RerankScore, error) {
	var out RerankResponse
	if err := c.do(ctx, budgetRerank, http.MethodPost, "/v1/rerank", &RerankRequest{
		ContractVersion: ContractVersion,
		Query:           query,
		Texts:           texts,
		TopN:            topN,
	}, &out); err != nil {
		return nil, fmt.Errorf("rerank: %w", err)
	}
	return out.Scores, nil
}
