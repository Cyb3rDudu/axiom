package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/chunker"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// Embedder is the subset of gpuworker.Client ChunkProcessor depends on.
// The real client accepts and returns []map[string]any — we reuse that
// shape here so the Python embed_chunks contract is preserved.
type Embedder interface {
	EmbedChunks(ctx context.Context, chunks []map[string]any) ([]map[string]any, error)
}

// ChunkStore writes the emitted chunks + embeddings back to Postgres.
type ChunkStore interface {
	InsertChunks(ctx context.Context, docID uuid.UUID, chunks []repo.ChunkInsert) error
}

// ChunkCounter updates the documents.chunk_count column once the chunks
// have landed. Pulled into its own interface so ChunkProcessor doesn't
// need a reference to the full repo.Documents type.
type ChunkCounter interface {
	MarkStatus(ctx context.Context, docID uuid.UUID, userID int32, in repo.MarkStatusInput) error
}

// DefaultMaxMarkdownBytes caps the size of a markdown file the
// ChunkProcessor will load into memory. 64 MiB easily fits any
// realistic document (a 2000-page textbook converts to ~20 MiB of
// markdown) while preventing a 2 GiB raw-upload from pinning RSS.
const DefaultMaxMarkdownBytes int64 = 64 << 20

// ChunkProcessor reads the markdown file produced by PDFProcessor,
// runs the structure-aware chunker, calls the GPU worker's
// embed_chunks, then persists every chunk + embedding into
// document_chunks.
//
// It assumes PDFProcessor (or an equivalent) ran earlier in the chain
// and that {MarkdownDir}/{doc_id}.md exists on disk.
type ChunkProcessor struct {
	Chunker     *chunker.Chunker
	Embedder    Embedder
	Store       ChunkStore
	StatusStore ChunkCounter
	MarkdownDir string
	Logger      *slog.Logger
	// SkipEmbeddings omits the GPU call entirely. Useful when the
	// operator wants chunks persisted without spending a GPU
	// round-trip (e.g. Marker-only smoke tests, or a redeploy where
	// embeddings are re-computed later).
	SkipEmbeddings bool
	// MaxMarkdownBytes caps os.ReadFile before it pulls the whole
	// markdown into memory. Zero → DefaultMaxMarkdownBytes.
	MaxMarkdownBytes int64
}

// Process implements Processor.
func (p ChunkProcessor) Process(ctx context.Context, job Job) error {
	if p.Chunker == nil {
		return errors.New("chunk_processor: Chunker not configured")
	}
	if p.Store == nil {
		return errors.New("chunk_processor: Store not configured")
	}
	if p.MarkdownDir == "" {
		return errors.New("chunk_processor: MarkdownDir not configured")
	}
	mdPath := filepath.Join(p.MarkdownDir, job.DocID.String()+".md")
	body, err := readMarkdownBounded(mdPath, p.maxMarkdownBytes())
	if err != nil {
		return fmt.Errorf("read markdown: %w", err)
	}

	chunks := p.Chunker.Chunk(string(body), chunker.DocMetadata{DocID: job.DocID.String()})
	log := p.logger()
	log.Info("chunker emitted",
		slog.String("doc_id", job.DocID.String()),
		slog.Int("chunks", len(chunks)),
	)
	if len(chunks) == 0 {
		return p.persist(ctx, job, nil)
	}

	enriched, err := p.embedIfEnabled(ctx, chunks)
	if err != nil {
		return err
	}

	inserts, err := toChunkInserts(chunks, enriched)
	if err != nil {
		return err
	}
	if err := p.persist(ctx, job, inserts); err != nil {
		return err
	}
	log.Info("chunks persisted",
		slog.String("doc_id", job.DocID.String()),
		slog.Int("count", len(inserts)),
	)
	return nil
}

// embedIfEnabled returns the enriched slice when embeddings are
// configured, or nil when SkipEmbeddings is true / Embedder is nil.
//
// The payload must carry `metadata.section_titles` so the Python
// embedder can prepend the section context to the text before
// encoding — see axiom_backend/ai_researcher/core_rag/embedder.py
// lines ~175-181. Sending just `{text, chunk_id}` would produce
// observably different embeddings vs the Python pipeline on every
// document with headings.
func (p ChunkProcessor) embedIfEnabled(ctx context.Context, chunks []chunker.Chunk) ([]map[string]any, error) {
	if p.SkipEmbeddings || p.Embedder == nil {
		return nil, nil
	}
	payload := make([]map[string]any, len(chunks))
	for i, c := range chunks {
		payload[i] = map[string]any{
			"text":     c.Text,
			"chunk_id": c.Metadata.ChunkID,
			"metadata": map[string]any{
				"section_titles": sectionTitlesOrEmpty(c.Metadata.SectionTitles),
				"chunk_id":       c.Metadata.ChunkID,
				"chunk_index":    c.Metadata.ChunkIndex,
			},
		}
	}
	enriched, err := p.Embedder.EmbedChunks(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("embed_chunks: %w", err)
	}
	if len(enriched) != len(chunks) {
		return nil, fmt.Errorf("embed_chunks mismatch: want %d, got %d", len(chunks), len(enriched))
	}
	return enriched, nil
}

// sectionTitlesOrEmpty guarantees a non-nil slice in the JSON/msgpack
// encoding so the Python embedder's `.get("section_titles", [])`
// fallback path never has to fire.
func sectionTitlesOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// persist writes chunks + updates chunk_count on the parent document.
// Both writes use a fresh-ctx fallback when the parent ctx is already
// cancelled — otherwise a SIGTERM mid-persist would leave
// document_chunks rows inserted but chunk_count still 0 (or the
// chunks themselves half-committed if the InsertChunks transaction
// rolled back).
func (p ChunkProcessor) persist(ctx context.Context, job Job, inserts []repo.ChunkInsert) error {
	writeCtx, cancel := freshCtxIfCancelled(ctx, persistFallbackTimeout)
	if cancel != nil {
		defer cancel()
	}
	if err := p.Store.InsertChunks(writeCtx, job.DocID, inserts); err != nil {
		return fmt.Errorf("insert chunks: %w", err)
	}
	if p.StatusStore == nil {
		return nil
	}
	count := int32(len(inserts))
	if err := p.StatusStore.MarkStatus(writeCtx, job.DocID, job.UserID, repo.MarkStatusInput{
		// Leave processing_status where the pool set it ("processing");
		// the pool flips to 'completed' after the chain returns.
		Status:     repo.StatusProcessing,
		ChunkCount: &count,
	}); err != nil {
		return fmt.Errorf("update chunk_count: %w", err)
	}
	return nil
}

// persistFallbackTimeout is the grace period for committing chunks
// after ctx is already cancelled. Matches the pool's fallback.
const persistFallbackTimeout = 5 * time.Second

// freshCtxIfCancelled returns ctx untouched when it's still live, or a
// fresh timeout-bound context (+ its cancel func) when ctx is already
// cancelled. The caller defers the returned cancel when non-nil.
func freshCtxIfCancelled(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, nil
	}
	return context.WithTimeout(context.Background(), timeout)
}

// readMarkdownBounded reads the markdown file at path but refuses to
// load more than maxBytes into RAM. Returning an error here is safer
// than OOM-killing the worker process.
func readMarkdownBounded(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path under configured dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("markdown too large: %d bytes (max %d)", info.Size(), maxBytes)
	}
	return io.ReadAll(io.LimitReader(f, maxBytes+1))
}

func (p ChunkProcessor) maxMarkdownBytes() int64 {
	if p.MaxMarkdownBytes > 0 {
		return p.MaxMarkdownBytes
	}
	return DefaultMaxMarkdownBytes
}

func (p ChunkProcessor) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// toChunkInserts fuses the chunker output with the embed_chunks
// response. When enriched is nil, dense/sparse stay empty and the
// caller persists the chunks without embeddings (see SkipEmbeddings).
func toChunkInserts(chunks []chunker.Chunk, enriched []map[string]any) ([]repo.ChunkInsert, error) {
	out := make([]repo.ChunkInsert, len(chunks))
	for i, c := range chunks {
		metaJSON, err := chunkMetaToMap(c.Metadata)
		if err != nil {
			return nil, err
		}
		ins := repo.ChunkInsert{
			ChunkID:    c.Metadata.ChunkID,
			ChunkIndex: int32(c.Metadata.ChunkIndex),
			Text:       c.Text,
			Metadata:   metaJSON,
		}
		if enriched != nil {
			e := enriched[i]
			embs, ok := e["embeddings"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("chunk %d missing embeddings field", i)
			}
			if dense, ok := embs["dense"]; ok && dense != nil {
				vec, err := asFloat32Slice(dense)
				if err != nil {
					return nil, fmt.Errorf("chunk %d dense: %w", i, err)
				}
				ins.Dense = vec
			}
			if sparse, ok := embs["sparse"]; ok {
				if m, ok := sparse.(map[string]any); ok {
					ins.Sparse = m
				}
			}
		}
		out[i] = ins
	}
	return out, nil
}

// chunkMetaToMap serialises ChunkMetadata to a generic map so it fits
// into chunk_metadata JSONB. The Extras map is inlined at the top
// level to match the Python shape.
func chunkMetaToMap(m chunker.ChunkMetadata) (map[string]any, error) {
	// Round-trip via JSON so the exported field names line up with
	// what the Python consumer / frontend expects.
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for k, v := range m.Extras {
		out[k] = v
	}
	return out, nil
}

// asFloat32Slice converts msgpack-decoded "any" values ([]any of
// float64 or []float32) into []float32 for pgvector storage.
func asFloat32Slice(v any) ([]float32, error) {
	switch t := v.(type) {
	case []float32:
		return t, nil
	case []float64:
		out := make([]float32, len(t))
		for i, f := range t {
			out[i] = float32(f)
		}
		return out, nil
	case []any:
		out := make([]float32, len(t))
		for i, el := range t {
			switch x := el.(type) {
			case float64:
				out[i] = float32(x)
			case float32:
				out[i] = x
			default:
				return nil, fmt.Errorf("element %d is %T", i, el)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected dense type %T", v)
	}
}
