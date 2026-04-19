package ingest_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

type stubChunkReader struct {
	chunks []repo.IndexedChunk
	err    error
}

func (s stubChunkReader) ListForDoc(_ context.Context, _ uuid.UUID) ([]repo.IndexedChunk, error) {
	return s.chunks, s.err
}

type stubOSIndex struct {
	mu        sync.Mutex
	indexed   []opensearch.ChunkDoc
	deletedID uuid.UUID
	deleteErr error
	indexErr  error
}

func (s *stubOSIndex) IndexChunks(_ context.Context, docs []opensearch.ChunkDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexErr != nil {
		return s.indexErr
	}
	s.indexed = append(s.indexed, docs...)
	return nil
}

func (s *stubOSIndex) DeleteDocument(_ context.Context, docID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedID = docID
	return s.deleteErr
}

func TestOpenSearchIndexerHappyPath(t *testing.T) {
	t.Parallel()
	docID := uuid.New()
	reader := stubChunkReader{chunks: []repo.IndexedChunk{
		{
			ChunkID:       "x_chunk_0000",
			ChunkIndex:    0,
			Text:          "hello world",
			SectionTitles: []string{"Intro", "Background"},
			TokenCount:    2,
			Metadata: map[string]any{
				"title":          "Paper",
				"page_start":     "1",
				"page_end":       "2",
				"page_label_map": map[string]any{"0": "i"},
				"image_refs":     []any{},
			},
		},
	}}
	idx := &stubOSIndex{}
	p := ingest.OpenSearchIndexer{
		Store:  reader,
		Index:  idx,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := p.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if idx.deletedID != docID {
		t.Errorf("delete target: want %s, got %s", docID, idx.deletedID)
	}
	if len(idx.indexed) != 1 {
		t.Fatalf("indexed count: %d", len(idx.indexed))
	}
	got := idx.indexed[0]
	if got.ChunkID != "x_chunk_0000" || got.DocID != docID.String() {
		t.Errorf("ids drifted: %+v", got)
	}
	if got.SectionTitles != "Intro Background" {
		t.Errorf("section_titles join: %q", got.SectionTitles)
	}
	// Python strips these four keys before indexing — we must too.
	for _, k := range []string{"page_start", "page_end", "page_label_map", "image_refs"} {
		if _, ok := got.Metadata[k]; ok {
			t.Errorf("metadata still carries %q", k)
		}
	}
	if got.Metadata["title"] != "Paper" {
		t.Errorf("title lost from metadata: %+v", got.Metadata)
	}
}

func TestOpenSearchIndexerDeletesEvenWhenNoChunks(t *testing.T) {
	t.Parallel()
	docID := uuid.New()
	reader := stubChunkReader{chunks: nil}
	idx := &stubOSIndex{}
	p := ingest.OpenSearchIndexer{Store: reader, Index: idx}
	if err := p.Process(context.Background(), ingest.Job{DocID: docID}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if idx.deletedID != docID {
		t.Errorf("delete should still run for empty doc: got %s", idx.deletedID)
	}
	if len(idx.indexed) != 0 {
		t.Errorf("should not index anything: %+v", idx.indexed)
	}
}

func TestOpenSearchIndexerDisabledWhenNoIndex(t *testing.T) {
	t.Parallel()
	// No Index client → clean no-op.
	p := ingest.OpenSearchIndexer{Store: stubChunkReader{}}
	if err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()}); err != nil {
		t.Fatalf("Process: %v", err)
	}
}

func TestOpenSearchIndexerSurfacesReadError(t *testing.T) {
	t.Parallel()
	p := ingest.OpenSearchIndexer{
		Store: stubChunkReader{err: errors.New("pg offline")},
		Index: &stubOSIndex{},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "read chunks") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenSearchIndexerSurfacesDeleteError(t *testing.T) {
	t.Parallel()
	idx := &stubOSIndex{deleteErr: errors.New("os unreachable")}
	p := ingest.OpenSearchIndexer{
		Store: stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "x"}}},
		Index: idx,
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "clear previous") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenSearchIndexerSurfacesIndexError(t *testing.T) {
	t.Parallel()
	idx := &stubOSIndex{indexErr: errors.New("index failed")}
	p := ingest.OpenSearchIndexer{
		Store: stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "x"}}},
		Index: idx,
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "index:") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenSearchIndexerNoStore(t *testing.T) {
	t.Parallel()
	p := ingest.OpenSearchIndexer{}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "Store") {
		t.Fatalf("got %v", err)
	}
}
