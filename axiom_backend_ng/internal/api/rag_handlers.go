package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// ChunkStore is the subset of repo.Chunks the handlers need.
type ChunkStore interface {
	List(ctx context.Context, userID int32, opt repo.ChunkListOptions) (repo.PaginatedChunks, error)
	GetByChunkID(ctx context.Context, userID int32, chunkID string) (repo.Chunk, error)
}

// RAGDeps wires the chunk store. Entities/graph handlers are stubbed
// until the knowledge-graph schema lands; both return an empty payload
// so the frontend can render without errors during the migration.
type RAGDeps struct {
	Chunks ChunkStore
}

// ListChunks handles GET /api/rag/chunks.
func (d RAGDeps) ListChunks(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	q := r.URL.Query()
	opt := repo.ChunkListOptions{
		Page:    atoi(q.Get("page")),
		Limit:   atoi(q.Get("limit")),
		Search:  q.Get("search"),
		Preview: 500,
	}
	if s := q.Get("doc_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid doc_id")
			return
		}
		opt.DocID = &id
	}
	page, err := d.Chunks.List(r.Context(), uid, opt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chunk list failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// GetChunk handles GET /api/rag/chunks/{chunk_id}. Matches Python's
// shape including the relationships/entities arrays — empty until the
// knowledge-graph schema lands so the frontend's chunk-detail page
// renders without undefined-reference errors.
func (d RAGDeps) GetChunk(w http.ResponseWriter, r *http.Request) {
	uid := requireUserID(r)
	chunkID := chi.URLParam(r, "chunk_id")
	if chunkID == "" {
		writeError(w, http.StatusBadRequest, "invalid chunk id")
		return
	}
	c, err := d.Chunks.GetByChunkID(r.Context(), uid, chunkID)
	if errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusNotFound, "chunk not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chunk fetch failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                      c.ID,
		"chunk_id":                c.ChunkID,
		"chunk_index":             c.ChunkIndex,
		"text":                    c.Text,
		"doc_id":                  c.DocID,
		"document_filename":       c.DocumentFilename,
		"document_metadata_title": c.DocumentMetadataTitle,
		"metadata":                c.Metadata,
		"created_at":              c.CreatedAt,
		"relationships":           []any{},
		"entities":                []any{},
	})
}

// ListEntities handles GET /api/rag/entities. Stubbed to an empty page
// until the knowledge-graph tables are ported. Shape matches Python's
// PaginatedEntitiesResponse so the frontend renders without surprises.
func (d RAGDeps) ListEntities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"entities": []any{},
		"pagination": repo.Pagination{
			TotalCount:  0,
			Page:        1,
			Limit:       50,
			TotalPages:  0,
			HasNext:     false,
			HasPrevious: false,
		},
	})
}

// Graph handles GET /api/rag/graph. Stubbed until the graph store is
// wired.
func (d RAGDeps) Graph(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": []any{},
		"edges": []any{},
		"stats": map[string]any{
			"total_nodes":  0,
			"total_edges":  0,
			"entity_types": []string{},
		},
	})
}
