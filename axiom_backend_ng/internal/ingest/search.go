package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/opensearch"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// ChunkReader is the subset of repo.Chunks OpenSearchIndexer needs.
// Kept narrow so tests can stub it without spinning up Postgres.
type ChunkReader interface {
	ListForDoc(ctx context.Context, docID uuid.UUID) ([]repo.IndexedChunk, error)
}

// OpenSearchIndex is the write subset of *opensearch.Client the
// indexer consumes.
type OpenSearchIndex interface {
	IndexChunks(ctx context.Context, docs []opensearch.ChunkDoc) error
	DeleteDocument(ctx context.Context, docID uuid.UUID) error
}

// OpenSearchIndexer pushes the chunks ChunkProcessor persisted into
// the BM25 index the retriever reads from. Runs after ChunkProcessor
// in the Chain; on reprocess it deletes the old doc first so the
// chunk_id space never accumulates stale entries.
//
// Parity target: axiom_backend/ai_researcher/core_rag/opensearch_store.py
// :add_chunks + :delete_document.
type OpenSearchIndexer struct {
	Store  ChunkReader
	Index  OpenSearchIndex
	Logger *slog.Logger
}

// Process implements Processor.
func (o OpenSearchIndexer) Process(ctx context.Context, job Job) error {
	if o.Store == nil {
		return fmt.Errorf("opensearch_indexer: Store not configured")
	}
	if o.Index == nil {
		// No client configured — treat as disabled. Matches the pattern
		// ChunkProcessor uses for SkipEmbeddings.
		return nil
	}
	chunks, err := o.Store.ListForDoc(ctx, job.DocID)
	if err != nil {
		return fmt.Errorf("opensearch_indexer: read chunks: %w", err)
	}
	// Always clear the old entries first so reprocessed documents
	// don't leave orphaned chunk_ids when the new chunking produced
	// a different count.
	if err := o.Index.DeleteDocument(ctx, job.DocID); err != nil {
		return fmt.Errorf("opensearch_indexer: clear previous: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	docs := make([]opensearch.ChunkDoc, 0, len(chunks))
	for _, c := range chunks {
		docs = append(docs, chunkToIndexDoc(job.DocID, c))
	}
	if err := o.Index.IndexChunks(ctx, docs); err != nil {
		return fmt.Errorf("opensearch_indexer: index: %w", err)
	}
	o.logger().Info("chunks indexed",
		slog.String("doc_id", job.DocID.String()),
		slog.Int("count", len(docs)),
	)
	return nil
}

func (o OpenSearchIndexer) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// chunkToIndexDoc projects a persisted chunk into the OpenSearch
// shape. Fields that the Python indexer strips (page_start, page_end,
// page_label_map, image_refs) are dropped here too so the document
// shape stays byte-compatible.
func chunkToIndexDoc(docID uuid.UUID, c repo.IndexedChunk) opensearch.ChunkDoc {
	metadata := map[string]any{}
	for k, v := range c.Metadata {
		if k == "page_start" || k == "page_end" ||
			k == "page_label_map" || k == "image_refs" {
			continue
		}
		metadata[k] = v
	}
	return opensearch.ChunkDoc{
		ChunkID:       c.ChunkID,
		DocID:         docID.String(),
		ChunkText:     c.Text,
		SectionTitles: joinSectionTitles(c.SectionTitles),
		ChunkIndex:    int(c.ChunkIndex),
		TokenCount:    c.TokenCount,
		Metadata:      metadata,
	}
}

// joinSectionTitles converts the list form stored in Postgres metadata
// into the space-joined string OpenSearch's text field expects.
func joinSectionTitles(titles []string) string {
	if len(titles) == 0 {
		return ""
	}
	return strings.Join(titles, " ")
}
