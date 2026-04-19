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

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/gpuworker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// stubGPU is an in-memory ExtractEntitiesClient. Returns a per-text
// map of entities so tests can stage chunk-specific results.
type stubGPU struct {
	mu         sync.Mutex
	resultsBy  map[string][]gpuworker.Entity
	defaults   []gpuworker.Entity
	err        error
	calls      int
	lastLabels []string
	lastThresh float64
}

func (g *stubGPU) ExtractEntities(_ context.Context, text string, labels []string, threshold float64, _ bool) ([]gpuworker.Entity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.lastLabels = labels
	g.lastThresh = threshold
	if g.err != nil {
		return nil, g.err
	}
	if r, ok := g.resultsBy[text]; ok {
		return r, nil
	}
	return g.defaults, nil
}

// stubEntityStore captures upserts + links. Returns a deterministic
// UUID per canonical key so callers can assert on which chunks were
// linked to which entity.
type stubEntityStore struct {
	mu         sync.Mutex
	upserts    map[string]uuid.UUID
	links      []repo.OccurrenceLink
	deletedDoc uuid.UUID
	upsertErr  error
	linkErr    error
	deleteErr  error
}

func newStubEntityStore() *stubEntityStore {
	return &stubEntityStore{upserts: map[string]uuid.UUID{}}
}

func (s *stubEntityStore) UpsertEntity(_ context.Context, in repo.EntityUpsert) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return uuid.Nil, s.upsertErr
	}
	key := in.CanonicalForm + "\x00" + in.Type
	if id, ok := s.upserts[key]; ok {
		return id, nil
	}
	id := uuid.New()
	s.upserts[key] = id
	return id, nil
}

func (s *stubEntityStore) LinkChunk(_ context.Context, in repo.OccurrenceLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.linkErr != nil {
		return s.linkErr
	}
	s.links = append(s.links, in)
	return nil
}

func (s *stubEntityStore) DeleteForDoc(_ context.Context, docID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletedDoc = docID
	return nil
}

func TestEntityProcessorHappyPath(t *testing.T) {
	t.Parallel()
	docID := uuid.New()
	reader := stubChunkReader{chunks: []repo.IndexedChunk{
		{ChunkID: "d_chunk_0000", Text: "Alice met Bob at Acme Corp."},
		{ChunkID: "d_chunk_0001", Text: "The Acme Corp research group published findings."},
	}}
	gpu := &stubGPU{resultsBy: map[string][]gpuworker.Entity{
		"Alice met Bob at Acme Corp.": {
			{Text: "Alice", Label: "person", Score: 0.9, Start: 0, End: 5},
			{Text: "Bob", Label: "person", Score: 0.85, Start: 10, End: 13},
			{Text: "Acme Corp", Label: "organization", Score: 0.95, Start: 17, End: 26},
		},
		"The Acme Corp research group published findings.": {
			{Text: "Acme Corp", Label: "organization", Score: 0.9, Start: 4, End: 13},
		},
	}}
	store := newStubEntityStore()
	p := ingest.EntityProcessor{
		Chunks:   reader,
		Entities: store,
		GPU:      gpu,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := p.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// Delete called before any links.
	if store.deletedDoc != docID {
		t.Errorf("DeleteForDoc not called: %v", store.deletedDoc)
	}
	// Threshold passed through (default).
	if gpu.lastThresh != ingest.DefaultGLiNERThreshold {
		t.Errorf("threshold: got %f want %f", gpu.lastThresh, ingest.DefaultGLiNERThreshold)
	}
	// GLiNER labels passed through.
	if len(gpu.lastLabels) != len(ingest.GLiNERLabels) || gpu.lastLabels[0] != "person" {
		t.Errorf("labels: %v", gpu.lastLabels)
	}
	// 3 distinct entities across two chunks: alice, bob, acme corp.
	if len(store.upserts) != 3 {
		t.Errorf("upserts: %+v", store.upserts)
	}
	// 4 occurrences: alice + bob + acme(c1) + acme(c2).
	if len(store.links) != 4 {
		t.Errorf("links: %d: %+v", len(store.links), store.links)
	}
	// acme should link to BOTH chunks.
	var acmeID uuid.UUID
	for k, v := range store.upserts {
		if strings.HasPrefix(k, "acme corp\x00") {
			acmeID = v
		}
	}
	if acmeID == uuid.Nil {
		t.Fatalf("acme entity id not found in upserts: %+v", store.upserts)
	}
	chunksForAcme := map[string]bool{}
	for _, l := range store.links {
		if l.EntityID == acmeID {
			chunksForAcme[l.ChunkID] = true
		}
	}
	if !chunksForAcme["d_chunk_0000"] || !chunksForAcme["d_chunk_0001"] {
		t.Errorf("acme should link to both chunks: %v", chunksForAcme)
	}
}

func TestEntityProcessorFiltersJunk(t *testing.T) {
	t.Parallel()
	reader := stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}}
	gpu := &stubGPU{defaults: []gpuworker.Entity{
		{Text: "Smith et al.", Label: "person", Score: 0.9},           // "et al." noise
		{Text: "A", Label: "person", Score: 0.9},                      // too short
		{Text: strings.Repeat("B", 120), Label: "person", Score: 0.9}, // too long
		{Text: "firm", Label: "concept", Score: 0.9},                  // generic word, single token
		{Text: "Carbon", Label: "not-a-label", Score: 0.9},            // unmapped label
		{Text: "Great work", Label: "concept", Score: 0.9},            // survivor
		{Text: "GREAT WORK", Label: "concept", Score: 0.8},            // dedup by case-insensitive text+type
	}}
	store := newStubEntityStore()
	p := ingest.EntityProcessor{
		Chunks: reader, Entities: store, GPU: gpu,
	}
	if err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("only 'Great work' should survive, got %+v", store.upserts)
	}
	if len(store.links) != 1 {
		t.Errorf("links: %d", len(store.links))
	}
}

func TestEntityProcessorCanonicalFormLowercaseNoPunct(t *testing.T) {
	t.Parallel()
	reader := stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}}
	gpu := &stubGPU{defaults: []gpuworker.Entity{
		{Text: "Müller, Alice!", Label: "person", Score: 0.9},
	}}
	store := newStubEntityStore()
	p := ingest.EntityProcessor{Chunks: reader, Entities: store, GPU: gpu}
	if err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// canonical key format: "canonical\x00TYPE".
	found := false
	for k := range store.upserts {
		if k == "müller alice\x00PERSON" {
			found = true
		}
	}
	if !found {
		t.Fatalf("canonical_form not normalised: %+v", store.upserts)
	}
}

func TestEntityProcessorDisabledWithoutGPU(t *testing.T) {
	t.Parallel()
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c"}}},
		Entities: newStubEntityStore(),
	}
	// No GPU → clean no-op.
	if err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()}); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

func TestEntityProcessorConfigGuards(t *testing.T) {
	t.Parallel()
	err := ingest.EntityProcessor{}.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "Chunks") {
		t.Fatalf("no Chunks: %v", err)
	}
	err = ingest.EntityProcessor{Chunks: stubChunkReader{}}.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "Entities") {
		t.Fatalf("no Entities: %v", err)
	}
}

func TestEntityProcessorSurfacesReadError(t *testing.T) {
	t.Parallel()
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{err: errors.New("pg offline")},
		Entities: newStubEntityStore(),
		GPU:      &stubGPU{},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "read chunks") {
		t.Fatalf("got %v", err)
	}
}

func TestEntityProcessorSurfacesDeleteError(t *testing.T) {
	t.Parallel()
	store := newStubEntityStore()
	store.deleteErr = errors.New("pg offline")
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c"}}},
		Entities: store,
		GPU:      &stubGPU{},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "clear occurrences") {
		t.Fatalf("got %v", err)
	}
}

func TestEntityProcessorSurfacesGPUError(t *testing.T) {
	t.Parallel()
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}},
		Entities: newStubEntityStore(),
		GPU:      &stubGPU{err: errors.New("gpu offline")},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "extract for chunk") {
		t.Fatalf("got %v", err)
	}
}

func TestEntityProcessorSurfacesUpsertError(t *testing.T) {
	t.Parallel()
	store := newStubEntityStore()
	store.upsertErr = errors.New("pg offline")
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}},
		Entities: store,
		GPU: &stubGPU{defaults: []gpuworker.Entity{
			{Text: "Alice", Label: "person", Score: 0.9},
		}},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "upsert") {
		t.Fatalf("got %v", err)
	}
}

func TestEntityProcessorSurfacesLinkError(t *testing.T) {
	t.Parallel()
	store := newStubEntityStore()
	store.linkErr = errors.New("pg offline")
	p := ingest.EntityProcessor{
		Chunks:   stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}},
		Entities: store,
		GPU: &stubGPU{defaults: []gpuworker.Entity{
			{Text: "Alice", Label: "person", Score: 0.9},
		}},
	}
	err := p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("got %v", err)
	}
}

func TestEntityProcessorThresholdOverride(t *testing.T) {
	t.Parallel()
	gpu := &stubGPU{}
	p := ingest.EntityProcessor{
		Chunks:    stubChunkReader{chunks: []repo.IndexedChunk{{ChunkID: "c", Text: "x"}}},
		Entities:  newStubEntityStore(),
		GPU:       gpu,
		Threshold: 0.7,
	}
	_ = p.Process(context.Background(), ingest.Job{DocID: uuid.New()})
	if gpu.lastThresh != 0.7 {
		t.Errorf("threshold override ignored: %f", gpu.lastThresh)
	}
}
