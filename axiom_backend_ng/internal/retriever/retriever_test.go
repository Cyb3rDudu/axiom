package retriever_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/retriever"
)

type stubGPU struct {
	dense  []float32
	sparse map[string]float64
	err    error

	rerankHits []gpuworker.RerankHit
	rerankErr  error
}

func (s *stubGPU) EmbedQuery(_ context.Context, _ string) (gpuworker.EmbedResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := gpuworker.EmbedResult{}
	if s.dense != nil {
		v := make([]any, len(s.dense))
		for i, f := range s.dense {
			v[i] = f
		}
		out["dense"] = v
	}
	if s.sparse != nil {
		m := map[string]any{}
		for k, vv := range s.sparse {
			m[k] = vv
		}
		out["sparse"] = m
	}
	return out, nil
}

func (s *stubGPU) Rerank(_ context.Context, _ string, _ []string, _ int) ([]gpuworker.RerankHit, error) {
	return s.rerankHits, s.rerankErr
}

type stubOS struct {
	hits []opensearch.Hit
	err  error
}

func (s *stubOS) BM25Search(_ context.Context, _ opensearch.SearchOptions) ([]opensearch.Hit, error) {
	return s.hits, s.err
}

func TestRetrieveQueryRequired(t *testing.T) {
	t.Parallel()
	r := &retriever.Retriever{}
	_, err := r.Retrieve(context.Background(), retriever.Options{})
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestRetrieveOpenSearchOnlyWhenGPUMissing(t *testing.T) {
	t.Parallel()
	doc := uuid.New()
	r := &retriever.Retriever{
		OpenSearch: &stubOS{
			hits: []opensearch.Hit{
				{ChunkID: "x", DocID: doc, Text: "match", Score: 2.0},
			},
		},
	}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q", NResults: 5})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 1 || out[0].ChunkID != "x" {
		t.Errorf("results: %+v", out)
	}
	if out[0].FulltextScore != 2.0 {
		t.Errorf("fulltext score passthrough: %+v", out[0])
	}
}

func TestRetrieveFailsSoftOnOpenSearchError(t *testing.T) {
	t.Parallel()
	r := &retriever.Retriever{
		OpenSearch: &stubOS{err: errors.New("down")},
	}
	// No GPU, no OS hits → empty slice, not error.
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty: %+v", out)
	}
}

func TestRetrieveUsesReranker(t *testing.T) {
	t.Parallel()
	doc := uuid.New()
	gpu := &stubGPU{
		rerankHits: []gpuworker.RerankHit{{Index: 0, Score: 0.99}},
	}
	r := &retriever.Retriever{
		OpenSearch: &stubOS{
			hits: []opensearch.Hit{{ChunkID: "x", DocID: doc, Text: "match", Score: 1.0}},
		},
		GPU: gpu,
	}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q", NResults: 5, UseReranker: true})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("results: %+v", out)
	}
	if out[0].Score != 0.99 {
		t.Errorf("reranked score: %v", out[0].Score)
	}
}

func TestRetrieveRerankerFailureFallsBackToRRF(t *testing.T) {
	t.Parallel()
	doc := uuid.New()
	r := &retriever.Retriever{
		OpenSearch: &stubOS{
			hits: []opensearch.Hit{{ChunkID: "x", DocID: doc, Text: "match", Score: 1.0}},
		},
		GPU: &stubGPU{rerankErr: errors.New("gpu down")},
	}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q", UseReranker: true})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("fallback should still return RRF: %+v", out)
	}
}

func TestRetrieveTruncatesToNResults(t *testing.T) {
	t.Parallel()
	hits := []opensearch.Hit{}
	for i := 0; i < 10; i++ {
		hits = append(hits, opensearch.Hit{
			ChunkID: "h" + string(rune('a'+i)),
			DocID:   uuid.New(),
			Text:    "x",
			Score:   float64(i),
		})
	}
	r := &retriever.Retriever{OpenSearch: &stubOS{hits: hits}}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q", NResults: 3})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("expected 3 results, got %d", len(out))
	}
}

func TestRetrieveEmbedQueryFailureDegrades(t *testing.T) {
	t.Parallel()
	doc := uuid.New()
	r := &retriever.Retriever{
		GPU: &stubGPU{err: errors.New("CUDA OOM")},
		OpenSearch: &stubOS{
			hits: []opensearch.Hit{{ChunkID: "bm25", DocID: doc, Text: "x", Score: 1.0}},
		},
	}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q"})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 1 || out[0].ChunkID != "bm25" {
		t.Errorf("should fall through to BM25: %+v", out)
	}
}

func TestRetrieveNilOpenSearchAndNilGPUReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := &retriever.Retriever{}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q"})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %+v", out)
	}
}

func TestRetrieveRerankerIndexOutOfRangeIgnored(t *testing.T) {
	t.Parallel()
	doc := uuid.New()
	r := &retriever.Retriever{
		OpenSearch: &stubOS{
			hits: []opensearch.Hit{{ChunkID: "x", DocID: doc, Text: "match", Score: 1.0}},
		},
		GPU: &stubGPU{
			// Index 99 doesn't map to any initial hit — should be
			// silently dropped.
			rerankHits: []gpuworker.RerankHit{{Index: 99, Score: 0.5}},
		},
	}
	out, err := r.Retrieve(context.Background(), retriever.Options{Query: "q", UseReranker: true})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	// Reranker returned zero usable hits → retriever falls back.
	if len(out) != 1 {
		t.Errorf("fallback should surface initial hits: %+v", out)
	}
}
