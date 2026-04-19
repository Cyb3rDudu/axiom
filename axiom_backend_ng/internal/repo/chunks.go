package repo

import (
	"context"
	"encoding/json"
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
		ID             uuid.UUID
		ChunkID        string
		ChunkIndex     int32
		ChunkText      string
		DocID          uuid.UUID
		ChunkMetadata  []byte
		CreatedAt      time.Time
		Filename       string
		MetadataTitle  *string
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

// GetByChunkID returns the full chunk text + its document context for a
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
