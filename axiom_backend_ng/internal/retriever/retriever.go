package retriever

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
)

// Default weights mirror retriever.py:187.
const (
	DefaultDenseWeight    = 0.7
	DefaultFulltextWeight = 0.3
)

// Result is the public shape returned by Retrieve.
type Result struct {
	ChunkID       string          `json:"chunk_id"`
	DocID         uuid.UUID       `json:"doc_id"`
	Text          string          `json:"text"`
	Score         float64         `json:"score"`
	DenseScore    float64         `json:"dense_score,omitempty"`
	SparseScore   float64         `json:"sparse_score,omitempty"`
	FulltextScore float64         `json:"fulltext_score,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	SectionTitles string          `json:"section_titles,omitempty"`
}

// Options drives a single Retrieve call.
type Options struct {
	Query          string
	NResults       int
	DocIDs         []uuid.UUID
	UseReranker    bool
	DenseWeight    float64
	FulltextWeight float64
}

// GPU is the subset of gpuworker.Client the retriever uses.
type GPU interface {
	EmbedQuery(ctx context.Context, text string) (gpuworker.EmbedResult, error)
	Rerank(ctx context.Context, query string, items []string, topN int) ([]gpuworker.RerankHit, error)
}

// OpenSearch is the subset of opensearch.Client the retriever uses.
type OpenSearch interface {
	BM25Search(ctx context.Context, opt opensearch.SearchOptions) ([]opensearch.Hit, error)
}

// Retriever combines the pg pool, OpenSearch client, and GPU worker
// into a single typed entry point. Nil OpenSearch / GPU fields degrade
// the pipeline instead of failing: fulltext skipped when OpenSearch
// is nil, dense/sparse skipped when GPU is nil.
type Retriever struct {
	DB         *gorm.DB
	OpenSearch OpenSearch
	GPU        GPU
}

// Retrieve runs the hybrid search pipeline. Returns up to NResults
// items ordered by the reranker (when enabled) or the RRF-fused score
// otherwise.
func (r *Retriever) Retrieve(ctx context.Context, opt Options) ([]Result, error) {
	if opt.Query == "" {
		return nil, fmt.Errorf("retriever: query is required")
	}
	if opt.NResults <= 0 {
		opt.NResults = 10
	}
	fetchN := opt.NResults
	if opt.UseReranker {
		fetchN = opt.NResults * 3 // retriever.py:131
	}

	denseVec, sparseVec := r.maybeEmbedQuery(ctx, opt.Query)

	inputs := []FusionInput{}
	if len(denseVec) > 0 {
		if hits, err := r.denseSearch(ctx, denseVec, opt.DocIDs, fetchN); err == nil && len(hits) > 0 {
			inputs = append(inputs, FusionInput{Name: "dense", Weight: weightOrDefault(opt.DenseWeight, DefaultDenseWeight), Hits: hits})
		}
		if len(sparseVec) > 0 {
			if hits, err := r.sparseSearch(ctx, sparseVec, opt.DocIDs, fetchN); err == nil && len(hits) > 0 {
				// Sparse rides the same weight as dense: both are
				// embedder channels. Python retriever.py merges sparse
				// into the dense channel's weight bucket too.
				inputs = append(inputs, FusionInput{Name: "sparse", Weight: weightOrDefault(opt.DenseWeight, DefaultDenseWeight), Hits: hits})
			}
		}
	}
	if r.OpenSearch != nil {
		hits, err := r.OpenSearch.BM25Search(ctx, opensearch.SearchOptions{
			Query:  opt.Query,
			DocIDs: opt.DocIDs,
			Size:   fetchN,
		})
		if err == nil && len(hits) > 0 {
			channel := make([]Ranked, 0, len(hits))
			for _, h := range hits {
				channel = append(channel, Ranked{
					ChunkID: h.ChunkID,
					Score:   h.Score,
					Payload: map[string]any{
						"doc_id":         h.DocID,
						"text":           h.Text,
						"metadata":       h.Metadata,
						"section_titles": h.SectionTitles,
						"source":         "opensearch",
					},
				})
			}
			inputs = append(inputs, FusionInput{Name: "fulltext", Weight: weightOrDefault(opt.FulltextWeight, DefaultFulltextWeight), Hits: channel})
		}
	}

	fused := RRF(inputs, DefaultRRFConstant)
	results := make([]Result, 0, len(fused))
	for _, f := range fused {
		results = append(results, resultFromFused(f))
	}
	if len(results) > fetchN {
		results = results[:fetchN]
	}

	if opt.UseReranker && r.GPU != nil && len(results) > 0 {
		if reranked, err := r.rerank(ctx, opt.Query, results, opt.NResults); err == nil && len(reranked) > 0 {
			return reranked, nil
		}
	}
	if len(results) > opt.NResults {
		results = results[:opt.NResults]
	}
	return results, nil
}

// maybeEmbedQuery asks the GPU worker for dense + sparse vectors.
// Returns nils on any failure so the caller can proceed with
// OpenSearch-only retrieval.
func (r *Retriever) maybeEmbedQuery(ctx context.Context, q string) ([]float32, SparseVector) {
	if r.GPU == nil {
		return nil, nil
	}
	ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := r.GPU.EmbedQuery(ectx, q)
	if err != nil {
		return nil, nil
	}
	dense := extractDense(resp)
	sparse := extractSparse(resp)
	return dense, sparse
}

// denseSearch executes the pgvector cosine-distance query. The caller
// passes a dense vector of whatever dimension the worker produced
// (1024 for BGE-M3).
func (r *Retriever) denseSearch(ctx context.Context, vec []float32, docIDs []uuid.UUID, n int) ([]Ranked, error) {
	if r.DB == nil {
		return nil, fmt.Errorf("retriever: no DB configured")
	}
	vecStr := pgvectorLiteral(vec)
	where := "dc.dense_embedding IS NOT NULL"
	if len(docIDs) > 0 {
		where = "dc.dense_embedding IS NOT NULL AND dc.doc_id IN ?"
	}
	query := fmt.Sprintf(`
		SELECT dc.chunk_id, dc.doc_id, dc.chunk_text, dc.chunk_metadata,
		       1 - (dc.dense_embedding <=> ?::vector) AS similarity
		FROM document_chunks dc
		WHERE %s
		ORDER BY dc.dense_embedding <=> ?::vector
		LIMIT ?
	`, where)
	return r.runChunkQuery(ctx, query, denseArgs(docIDs, vecStr, n))
}

// sparseSearch fetches chunks with a sparse_embedding and computes
// cosine similarity client-side (matches Python behaviour — no SQL
// UDF). Fetching only top-n candidates keeps the memory footprint
// bounded.
func (r *Retriever) sparseSearch(ctx context.Context, query SparseVector, docIDs []uuid.UUID, n int) ([]Ranked, error) {
	if r.DB == nil {
		return nil, fmt.Errorf("retriever: no DB configured")
	}
	// Over-fetch 5× so the client-side cosine top-n is meaningful;
	// bounded to avoid blowing out memory on huge libraries.
	limit := n * 5
	if limit < 50 {
		limit = 50
	}

	baseQ := r.DB.WithContext(ctx).
		Table("document_chunks AS dc").
		Select("dc.chunk_id, dc.doc_id, dc.chunk_text, dc.chunk_metadata, dc.sparse_embedding").
		Where("dc.sparse_embedding IS NOT NULL AND dc.sparse_embedding::text <> '{}'")
	if len(docIDs) > 0 {
		baseQ = baseQ.Where("dc.doc_id IN ?", docIDs)
	}

	type row struct {
		ChunkID         string
		DocID           uuid.UUID
		ChunkText       string
		ChunkMetadata   []byte
		SparseEmbedding []byte
	}
	var rows []row
	if err := baseQ.Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}

	hits := make([]Ranked, 0, len(rows))
	for _, r := range rows {
		stored := DecodeSparse(r.SparseEmbedding)
		score := query.Cosine(stored)
		if score <= 0 {
			continue
		}
		hits = append(hits, Ranked{
			ChunkID: r.ChunkID,
			Score:   score,
			Payload: map[string]any{
				"doc_id":   r.DocID,
				"text":     r.ChunkText,
				"metadata": json.RawMessage(r.ChunkMetadata),
				"source":   "sparse",
			},
		})
	}
	if len(hits) > n {
		sortByScore(hits)
		hits = hits[:n]
	} else {
		sortByScore(hits)
	}
	return hits, nil
}

func (r *Retriever) runChunkQuery(ctx context.Context, q string, args []any) ([]Ranked, error) {
	type row struct {
		ChunkID       string
		DocID         uuid.UUID
		ChunkText     string
		ChunkMetadata []byte
		Similarity    float64
	}
	var rows []row
	if err := r.DB.WithContext(ctx).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	hits := make([]Ranked, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, Ranked{
			ChunkID: r.ChunkID,
			Score:   r.Similarity,
			Payload: map[string]any{
				"doc_id":   r.DocID,
				"text":     r.ChunkText,
				"metadata": json.RawMessage(r.ChunkMetadata),
				"source":   "dense",
			},
		})
	}
	return hits, nil
}

// rerank calls the GPU worker's cross-encoder. Failures fall through
// to the pre-rerank results so the overall Retrieve call still
// succeeds.
func (r *Retriever) rerank(ctx context.Context, query string, initial []Result, topN int) ([]Result, error) {
	texts := make([]string, len(initial))
	for i, res := range initial {
		texts[i] = res.Text
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hits, err := r.GPU.Rerank(rctx, query, texts, topN)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		if h.Index < 0 || h.Index >= len(initial) {
			continue
		}
		r := initial[h.Index]
		r.Score = h.Score
		out = append(out, r)
	}
	return out, nil
}

// helpers

func weightOrDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func pgvectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconvFloat32(f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func strconvFloat32(f float32) string {
	return fmt.Sprintf("%g", float64(f))
}

func denseArgs(docIDs []uuid.UUID, vec string, n int) []any {
	// Placeholder order in the SQL template:
	//   1. SELECT  1 - (embedding <=> ?::vector)
	//   2. WHERE   doc_id IN ?          (only when docIDs != nil)
	//   3. ORDER   embedding <=> ?::vector
	//   4. LIMIT   ?
	if len(docIDs) > 0 {
		return []any{vec, docIDs, vec, n}
	}
	return []any{vec, vec, n}
}

// sortByScore sorts Ranked slices in-place by Score DESC.
func sortByScore(hits []Ranked) {
	// Local bubble (fine for small N) — avoids importing sort again
	// and keeps the function inlinable.
	for i := range hits {
		for j := i + 1; j < len(hits); j++ {
			if hits[j].Score > hits[i].Score {
				hits[i], hits[j] = hits[j], hits[i]
			}
		}
	}
}

func resultFromFused(f FusedHit) Result {
	res := Result{
		ChunkID:       f.ChunkID,
		Score:         f.CombinedScore,
		DenseScore:    f.ChannelScores["dense"],
		SparseScore:   f.ChannelScores["sparse"],
		FulltextScore: f.ChannelScores["fulltext"],
	}
	if f.Payload != nil {
		if v, ok := f.Payload["doc_id"].(uuid.UUID); ok {
			res.DocID = v
		}
		if v, ok := f.Payload["text"].(string); ok {
			res.Text = v
		}
		if v, ok := f.Payload["metadata"].(json.RawMessage); ok {
			res.Metadata = v
		}
		if v, ok := f.Payload["section_titles"].(string); ok {
			res.SectionTitles = v
		}
	}
	return res
}

// extractDense pulls a []float32 out of whatever shape the Python
// worker packs into the EmbedResult map. Looks for common keys
// ("dense", "vector", "embedding") in that order.
func extractDense(r gpuworker.EmbedResult) []float32 {
	for _, k := range []string{"dense", "vector", "embedding"} {
		if v, ok := r[k]; ok {
			if vec := toFloat32Slice(v); len(vec) > 0 {
				return vec
			}
		}
	}
	return nil
}

// extractSparse pulls out the sparse JSONB-like shape — Python sends
// it as `{"sparse": {"token_id": weight}}`.
func extractSparse(r gpuworker.EmbedResult) SparseVector {
	v, ok := r["sparse"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := SparseVector{}
	for k, val := range m {
		if f, ok := toFloat64(val); ok {
			out[k] = f
		}
	}
	return out
}

func toFloat32Slice(v any) []float32 {
	switch s := v.(type) {
	case []float32:
		return s
	case []any:
		out := make([]float32, 0, len(s))
		for _, x := range s {
			if f, ok := toFloat64(x); ok {
				out = append(out, float32(f))
			}
		}
		return out
	case []float64:
		out := make([]float32, len(s))
		for i, f := range s {
			out[i] = float32(f)
		}
		return out
	}
	return nil
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
