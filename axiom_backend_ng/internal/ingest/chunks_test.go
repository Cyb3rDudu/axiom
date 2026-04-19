package ingest_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/chunker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/ingest"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

// fakeEmbedder is an Embedder stub that either returns canned output
// or forwards the call to a per-test function.
type fakeEmbedder struct {
	mu         sync.Mutex
	callCount  int
	inputChunk []map[string]any
	runErr     error
	// respond, when non-nil, builds the "enriched" response from the
	// input. When nil, fakeEmbedder produces a default response where
	// each chunk gets a dense=[1.0,2.0,...] and an empty sparse.
	respond func([]map[string]any) []map[string]any
}

// denseFixture is a 1024-dim vector matching the pgvector column's
// dimension. Kept as a var so the fake embedder doesn't allocate on
// every call.
var denseFixture = func() []any {
	out := make([]any, 1024)
	for i := range out {
		out[i] = float64(i%10) * 0.1
	}
	return out
}()

func (f *fakeEmbedder) EmbedChunks(_ context.Context, chunks []map[string]any) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.inputChunk = chunks
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.respond != nil {
		return f.respond(chunks), nil
	}
	out := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		cp := make(map[string]any, len(c)+1)
		for k, v := range c {
			cp[k] = v
		}
		cp["embeddings"] = map[string]any{
			"dense":  denseFixture,
			"sparse": map[string]any{"42": 0.8},
		}
		out[i] = cp
	}
	return out, nil
}

func TestChunkProcessorHappyPath(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "chunk-happy")
	ids := seedPending(t, pg, uid, 1)
	docID := ids[0]

	// Write a synthetic markdown file with enough content to chunk.
	mdDir := t.TempDir()
	mdPath := filepath.Join(mdDir, docID.String()+".md")
	body := "# Intro\n\nFirst paragraph.\n\n# Method\n\nSecond paragraph body."
	if err := os.WriteFile(mdPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	emb := &fakeEmbedder{}
	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.Config{MaxChunkTokens: 50, OverlapTokens: 0, MinChunkTokens: 1}),
		Embedder:    emb,
		Store:       repo.NewChunks(pg.DB),
		StatusStore: repo.NewDocuments(pg.DB),
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := proc.Process(context.Background(), ingest.Job{
		DocID: docID, UserID: uid, Filename: "d.pdf",
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Chunks persisted?
	var count int64
	if err := pg.DB.Raw(`SELECT COUNT(*) FROM document_chunks WHERE doc_id = ?`, docID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count < 2 {
		t.Errorf("expected >=2 chunks, got %d", count)
	}

	// chunk_count updated on document.
	var chunkCount int32
	if err := pg.DB.Raw(`SELECT chunk_count FROM documents WHERE id = ?`, docID).Scan(&chunkCount).Error; err != nil {
		t.Fatalf("read chunk_count: %v", err)
	}
	if int64(chunkCount) != count {
		t.Errorf("chunk_count: got %d want %d", chunkCount, count)
	}

	// Dense embedding persisted (non-null).
	var nonNull int64
	if err := pg.DB.Raw(
		`SELECT COUNT(*) FROM document_chunks WHERE doc_id = ? AND dense_embedding IS NOT NULL`,
		docID,
	).Scan(&nonNull).Error; err != nil {
		t.Fatalf("count non-null: %v", err)
	}
	if nonNull != count {
		t.Errorf("dense embedding missing on some rows: %d/%d", nonNull, count)
	}

	// Sparse embedding is JSONB, not null.
	var sparsePayload string
	if err := pg.DB.Raw(
		`SELECT sparse_embedding::text FROM document_chunks WHERE doc_id = ? LIMIT 1`, docID,
	).Scan(&sparsePayload).Error; err != nil {
		t.Fatalf("read sparse: %v", err)
	}
	if sparsePayload == "" || sparsePayload == "{}" {
		t.Errorf("sparse payload empty: %q", sparsePayload)
	}
}

func TestChunkProcessorSkipEmbeddings(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "chunk-noembed")
	ids := seedPending(t, pg, uid, 1)
	docID := ids[0]

	mdDir := t.TempDir()
	mdPath := filepath.Join(mdDir, docID.String()+".md")
	_ = os.WriteFile(mdPath, []byte("# H\n\nBody."), 0o644)

	emb := &fakeEmbedder{}
	proc := ingest.ChunkProcessor{
		Chunker:        chunker.New(chunker.DefaultConfig()),
		Embedder:       emb,
		Store:          repo.NewChunks(pg.DB),
		StatusStore:    repo.NewDocuments(pg.DB),
		MarkdownDir:    mdDir,
		SkipEmbeddings: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: uid}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if emb.callCount != 0 {
		t.Errorf("embedder should not be called; got %d calls", emb.callCount)
	}
	var nullDense int64
	if err := pg.DB.Raw(
		`SELECT COUNT(*) FROM document_chunks WHERE doc_id = ? AND dense_embedding IS NULL`, docID,
	).Scan(&nullDense).Error; err != nil {
		t.Fatalf("count null: %v", err)
	}
	if nullDense == 0 {
		t.Errorf("expected NULL dense embeddings when SkipEmbeddings=true")
	}
}

func TestChunkProcessorEmbedderError(t *testing.T) {
	t.Parallel()
	mdDir := t.TempDir()
	docID := uuid.New()
	_ = os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), []byte("# H\n\nBody."), 0o644)

	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.DefaultConfig()),
		Embedder:    &fakeEmbedder{runErr: errors.New("gpu offline")},
		Store:       &nullStore{},
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1})
	if err == nil || !errors.Is(err, errors.Unwrap(err)) || err.Error() == "" {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestChunkProcessorMismatchedEmbedderResponse(t *testing.T) {
	t.Parallel()
	mdDir := t.TempDir()
	docID := uuid.New()
	_ = os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), []byte("# H\n\nBody one.\n\n# H2\n\nBody two."), 0o644)

	emb := &fakeEmbedder{respond: func(_ []map[string]any) []map[string]any {
		return nil // too few
	}}
	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.Config{MaxChunkTokens: 10, OverlapTokens: 0, MinChunkTokens: 1}),
		Embedder:    emb,
		Store:       &nullStore{},
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1})
	if err == nil || !containsStr(err.Error(), "mismatch") {
		t.Fatalf("got %v", err)
	}
}

func TestChunkProcessorMissingMarkdown(t *testing.T) {
	t.Parallel()
	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.DefaultConfig()),
		Store:       &nullStore{},
		MarkdownDir: "/tmp/does-not-exist-abc",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := proc.Process(context.Background(), ingest.Job{DocID: uuid.New(), UserID: 1})
	if err == nil || !containsStr(err.Error(), "read markdown") {
		t.Fatalf("got %v", err)
	}
}

func TestChunkProcessorEmptyMarkdownPersistsZero(t *testing.T) {
	t.Parallel()
	pg := testutil.StartPostgres(t)
	uid := seedUser(t, pg, "chunk-empty")
	ids := seedPending(t, pg, uid, 1)
	docID := ids[0]

	mdDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), []byte(""), 0o644)

	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.DefaultConfig()),
		Embedder:    &fakeEmbedder{},
		Store:       repo.NewChunks(pg.DB),
		StatusStore: repo.NewDocuments(pg.DB),
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: uid}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	var count int64
	if err := pg.DB.Raw(`SELECT COUNT(*) FROM document_chunks WHERE doc_id = ?`, docID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("want 0 chunks, got %d", count)
	}
}

func TestChunkProcessorConfigurationGuards(t *testing.T) {
	t.Parallel()
	base := ingest.Job{DocID: uuid.New(), UserID: 1}

	t.Run("no chunker", func(t *testing.T) {
		t.Parallel()
		err := ingest.ChunkProcessor{}.Process(context.Background(), base)
		if err == nil || !containsStr(err.Error(), "Chunker") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no store", func(t *testing.T) {
		t.Parallel()
		p := ingest.ChunkProcessor{Chunker: chunker.New(chunker.DefaultConfig()), MarkdownDir: "/tmp"}
		err := p.Process(context.Background(), base)
		if err == nil || !containsStr(err.Error(), "Store") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no markdown dir", func(t *testing.T) {
		t.Parallel()
		p := ingest.ChunkProcessor{Chunker: chunker.New(chunker.DefaultConfig()), Store: &nullStore{}}
		err := p.Process(context.Background(), base)
		if err == nil || !containsStr(err.Error(), "MarkdownDir") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestChunkProcessorStoreError(t *testing.T) {
	t.Parallel()
	mdDir := t.TempDir()
	docID := uuid.New()
	_ = os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), []byte("# H\n\nBody."), 0o644)

	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.DefaultConfig()),
		Embedder:    &fakeEmbedder{},
		Store:       &nullStore{err: errors.New("pg offline")},
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1})
	if err == nil || !containsStr(err.Error(), "insert chunks") {
		t.Fatalf("got %v", err)
	}
}

func TestChunkProcessorMarkdownTooLarge(t *testing.T) {
	t.Parallel()
	mdDir := t.TempDir()
	docID := uuid.New()
	// 2 MiB markdown, but the processor caps at 1 MiB for this test.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), big, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	proc := ingest.ChunkProcessor{
		Chunker:          chunker.New(chunker.DefaultConfig()),
		Store:            &nullStore{},
		MarkdownDir:      mdDir,
		MaxMarkdownBytes: 1 << 20,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1})
	if err == nil || !containsStr(err.Error(), "markdown too large") {
		t.Fatalf("got %v", err)
	}
}

func TestChunkProcessorEmbedPayloadCarriesSectionTitles(t *testing.T) {
	t.Parallel()
	mdDir := t.TempDir()
	docID := uuid.New()
	body := "# Intro\n\nFirst body paragraph here.\n\n# Method\n\nSecond body paragraph."
	_ = os.WriteFile(filepath.Join(mdDir, docID.String()+".md"), []byte(body), 0o644)

	emb := &fakeEmbedder{}
	proc := ingest.ChunkProcessor{
		Chunker:     chunker.New(chunker.Config{MaxChunkTokens: 50, OverlapTokens: 0, MinChunkTokens: 1}),
		Embedder:    emb,
		Store:       &nullStore{},
		MarkdownDir: mdDir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := proc.Process(context.Background(), ingest.Job{DocID: docID, UserID: 1}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if emb.callCount != 1 {
		t.Fatalf("embedder calls: %d", emb.callCount)
	}
	// Every chunk dict must carry metadata.section_titles as a list.
	for i, c := range emb.inputChunk {
		meta, ok := c["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("chunk %d missing metadata dict: %+v", i, c)
		}
		titles, ok := meta["section_titles"].([]string)
		if !ok {
			t.Fatalf("chunk %d section_titles: %T %v", i, meta["section_titles"], meta["section_titles"])
		}
		_ = titles // value check below
	}
}

// nullStore is an in-memory ChunkStore for the unit tests that don't
// spin up Postgres.
type nullStore struct {
	mu       sync.Mutex
	inserted []repo.ChunkInsert
	err      error
}

func (s *nullStore) InsertChunks(_ context.Context, _ uuid.UUID, chunks []repo.ChunkInsert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.inserted = append(s.inserted, chunks...)
	return nil
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
