package repo

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
)

// Chunk is the handler-facing view of a document_chunks row. Truncated
// vs full text is the caller's choice.
type Chunk struct {
	ID                    uuid.UUID       `json:"id"`
	ChunkID               string          `json:"chunk_id"`
	ChunkIndex            int32           `json:"chunk_index"`
	Text                  string          `json:"text"`
	DocID                 uuid.UUID       `json:"doc_id"`
	DocumentFilename      string          `json:"document_filename,omitempty"`
	DocumentMetadataTitle string          `json:"document_metadata_title,omitempty"`
	Metadata              json.RawMessage `json:"metadata,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
}

// PaginatedChunks is the /api/rag/chunks envelope.
type PaginatedChunks struct {
	Chunks     []Chunk    `json:"chunks"`
	Pagination Pagination `json:"pagination"`
}

// Chunks owns document_chunks read queries.
type Chunks struct{ gdb *gorm.DB }

// NewChunks wires the repo to the DB.
func NewChunks(gdb *gorm.DB) *Chunks { return &Chunks{gdb: gdb} }

// ChunkListOptions drives /api/rag/chunks pagination + filtering.
type ChunkListOptions struct {
	Page    int
	Limit   int
	DocID   *uuid.UUID
	Search  string
	Preview int // truncate chunk_text at this many runes; 0 = full
}

// List returns a page of chunks for the given user. It uses a join so
// ownership is enforced at the SQL level.
func (c *Chunks) List(ctx context.Context, userID int32, opt ChunkListOptions) (PaginatedChunks, error) {
	if opt.Page < 1 {
		opt.Page = 1
	}
	if opt.Limit < 1 || opt.Limit > 500 {
		opt.Limit = 50
	}

	q := c.gdb.WithContext(ctx).Table("document_chunks AS dc").
		Joins("JOIN documents d ON d.id = dc.doc_id").
		Where("d.user_id = ?", userID)
	if opt.DocID != nil {
		q = q.Where("dc.doc_id = ?", *opt.DocID)
	}
	if opt.Search != "" {
		q = q.Where("dc.chunk_text ILIKE ?", "%"+opt.Search+"%")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return PaginatedChunks{}, err
	}

	type row struct {
		ID            uuid.UUID
		ChunkID       string
		ChunkIndex    int32
		ChunkText     string
		DocID         uuid.UUID
		ChunkMetadata []byte
		CreatedAt     time.Time
		Filename      string
		MetadataTitle *string
	}
	var rows []row
	err := q.
		Select(`dc.id, dc.chunk_id, dc.chunk_index, dc.chunk_text, dc.doc_id,
		        dc.chunk_metadata, dc.created_at, d.filename,
		        d.metadata_->>'title' AS metadata_title`).
		Order("dc.created_at DESC").
		Limit(opt.Limit).
		Offset((opt.Page - 1) * opt.Limit).
		Scan(&rows).Error
	if err != nil {
		return PaginatedChunks{}, err
	}

	items := make([]Chunk, 0, len(rows))
	for _, r := range rows {
		text := r.ChunkText
		if opt.Preview > 0 && len(text) > opt.Preview {
			text = text[:opt.Preview]
		}
		ch := Chunk{
			ID:               r.ID,
			ChunkID:          r.ChunkID,
			ChunkIndex:       r.ChunkIndex,
			Text:             text,
			DocID:            r.DocID,
			DocumentFilename: r.Filename,
			CreatedAt:        r.CreatedAt,
		}
		if r.MetadataTitle != nil {
			ch.DocumentMetadataTitle = *r.MetadataTitle
		}
		if len(r.ChunkMetadata) > 0 {
			ch.Metadata = json.RawMessage(r.ChunkMetadata)
		}
		items = append(items, ch)
	}

	totalPages := 0
	if opt.Limit > 0 {
		totalPages = int((total + int64(opt.Limit) - 1) / int64(opt.Limit))
	}
	return PaginatedChunks{
		Chunks: items,
		Pagination: Pagination{
			TotalCount:  total,
			Page:        opt.Page,
			Limit:       opt.Limit,
			TotalPages:  totalPages,
			HasNext:     opt.Page < totalPages,
			HasPrevious: opt.Page > 1,
		},
	}, nil
}

// ChunkInsert carries the fields the ingest pipeline writes to
// document_chunks. Dense is nil when the embedding failed; Sparse
// defaults to {} to satisfy the NOT NULL column.
type ChunkInsert struct {
	ChunkID    string
	ChunkIndex int32
	Text       string
	Dense      []float32      // pgvector dim 1024; nil → NULL column
	Sparse     map[string]any // JSONB; nil → {}
	Metadata   map[string]any // JSONB; merged into chunk_metadata
}

// IndexedChunk is the view of a persisted chunk the ingest indexer
// consumes. Populated by ListForDoc from document_chunks rows +
// unmarshalled chunk_metadata JSONB.
type IndexedChunk struct {
	ChunkID       string
	ChunkIndex    int32
	Text          string
	SectionTitles []string
	TokenCount    int
	Metadata      map[string]any
}

// ListForDoc returns every chunk for a document in chunk_index order.
// Kept narrow — the OpenSearch indexer is the only caller so we don't
// pay for a general-purpose ORDER BY + filter API.
func (c *Chunks) ListForDoc(ctx context.Context, docID uuid.UUID) ([]IndexedChunk, error) {
	var rows []models.DocumentChunk
	err := c.gdb.WithContext(ctx).
		Where("doc_id = ?", docID).
		Order("chunk_index ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]IndexedChunk, 0, len(rows))
	for _, r := range rows {
		ic := IndexedChunk{
			ChunkID:    r.ChunkID,
			ChunkIndex: r.ChunkIndex,
			Text:       r.ChunkText,
		}
		if len(r.ChunkMetadata) > 0 {
			var meta map[string]any
			if err := json.Unmarshal(r.ChunkMetadata, &meta); err == nil {
				ic.Metadata = meta
				if v, ok := meta["token_count"]; ok {
					switch n := v.(type) {
					case float64:
						ic.TokenCount = int(n)
					case int:
						ic.TokenCount = n
					}
				}
				if v, ok := meta["section_titles"].([]any); ok {
					titles := make([]string, 0, len(v))
					for _, s := range v {
						if str, ok := s.(string); ok {
							titles = append(titles, str)
						}
					}
					ic.SectionTitles = titles
				}
			}
		}
		out = append(out, ic)
	}
	return out, nil
}

// InsertChunks replaces all chunks for a document in a single
// transaction. The delete-then-insert pattern mirrors what the Python
// doc-processor does on reprocess and keeps the chunk_id uniqueness
// constraint honest. Empty input is a no-op.
func (c *Chunks) InsertChunks(ctx context.Context, docID uuid.UUID, chunks []ChunkInsert) error {
	return c.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("doc_id = ?", docID).Delete(&models.DocumentChunk{}).Error; err != nil {
			return err
		}
		if len(chunks) == 0 {
			return nil
		}
		// pgvector's Go driver would add a dep; we format the literal
		// ourselves since the insert only touches this column when a
		// dense embedding is actually present.
		for _, ch := range chunks {
			metaJSON, err := json.Marshal(ch.Metadata)
			if err != nil {
				return err
			}
			sparse := ch.Sparse
			if sparse == nil {
				sparse = map[string]any{}
			}
			sparseJSON, err := json.Marshal(sparse)
			if err != nil {
				return err
			}
			var dense any
			if ch.Dense != nil {
				dense = gorm.Expr("?::vector", formatPGVector(ch.Dense))
			}
			if err := tx.Exec(`
				INSERT INTO document_chunks
					(id, doc_id, chunk_id, chunk_index, chunk_text,
					 dense_embedding, sparse_embedding, chunk_metadata, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, NOW())`,
				uuid.New(), docID, ch.ChunkID, ch.ChunkIndex, ch.Text,
				dense, string(sparseJSON), string(metaJSON),
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// formatPGVector renders a float32 slice into the pgvector text form
// "[1.0,2.0,...]". %g is compact and round-trips cleanly for the
// [-1,1] magnitudes BGE-M3 produces.
func formatPGVector(v []float32) string {
	var b []byte
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(f), 'f', -1, 32)
	}
	b = append(b, ']')
	return string(b)
}

// GetByChunkID returns a single chunk (scoped by user via join) for the
// given chunk_id (the Python API key, format '{doc_id}_{index}').
func (c *Chunks) GetByChunkID(ctx context.Context, userID int32, chunkID string) (Chunk, error) {
	var m models.DocumentChunk
	err := c.gdb.WithContext(ctx).
		Joins(`JOIN documents d ON d.id = document_chunks.doc_id`).
		Where("document_chunks.chunk_id = ? AND d.user_id = ?", chunkID, userID).
		First(&m).Error
	if err != nil {
		return Chunk{}, mapErr(err)
	}

	var doc models.Document
	err = c.gdb.WithContext(ctx).Where("id = ?", m.DocID).First(&doc).Error
	if err != nil {
		return Chunk{}, err
	}

	ch := Chunk{
		ID:               m.ID,
		ChunkID:          m.ChunkID,
		ChunkIndex:       m.ChunkIndex,
		Text:             m.ChunkText,
		DocID:            m.DocID,
		DocumentFilename: doc.Filename,
		CreatedAt:        m.CreatedAt,
	}
	if len(m.ChunkMetadata) > 0 {
		ch.Metadata = json.RawMessage(m.ChunkMetadata)
	}
	if len(doc.Metadata) > 0 {
		var meta map[string]any
		if err := json.Unmarshal(doc.Metadata, &meta); err == nil {
			if v, ok := meta["title"].(string); ok {
				ch.DocumentMetadataTitle = v
			}
		}
	}
	return ch, nil
}
