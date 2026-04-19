package repo

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/models"
)

// Entities owns document_entities + entity_chunk_occurrences writes
// for the ingest pipeline. Read-side knowledge-graph queries (which
// the retriever uses for graph expansion) live elsewhere.
type Entities struct{ gdb *gorm.DB }

// NewEntities wires the repo to the DB.
func NewEntities(gdb *gorm.DB) *Entities { return &Entities{gdb: gdb} }

// EntityUpsert is the input to UpsertEntity. Matches the subset the
// ingest pipeline can produce from GLiNER output (no LLM-refined
// description or embedding yet — those are deferred to later slices).
type EntityUpsert struct {
	Text          string
	Type          string
	CanonicalForm string
}

// UpsertEntity inserts or updates a (canonical_form, entity_type) row
// and returns its UUID. Mirrors Python's graph_store.add_entity
// semantics including the 255 / 2000 char truncation and the ON
// CONFLICT description accumulation rule — but we only ship the
// description path when the caller supplies one; GLiNER alone never
// produces one, so UpsertEntity leaves the column NULL.
func (e *Entities) UpsertEntity(ctx context.Context, in EntityUpsert) (uuid.UUID, error) {
	text := truncateString(in.Text, 255)
	canonical := truncateString(in.CanonicalForm, 255)
	var idStr string
	err := e.gdb.WithContext(ctx).Raw(`
		INSERT INTO document_entities
			(id, entity_text, entity_type, canonical_form, entity_metadata, created_at, updated_at)
		VALUES (gen_random_uuid(), ?, ?, ?, '{}'::jsonb, NOW(), NOW())
		ON CONFLICT (canonical_form, entity_type)
		DO UPDATE SET
			entity_text = EXCLUDED.entity_text,
			updated_at  = CURRENT_TIMESTAMP
		RETURNING id::text`, text, in.Type, canonical).Scan(&idStr).Error
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(idStr)
}

// OccurrenceLink is one (entity, chunk) edge to persist.
type OccurrenceLink struct {
	EntityID        uuid.UUID
	ChunkID         string
	DocID           uuid.UUID
	PositionInChunk int32
	RelevanceScore  float64
	ContextSnippet  string
}

// LinkChunk records that an entity occurs in a specific chunk.
// Mirrors Python's add_occurrence including the ON CONFLICT counter
// bump — repeated ingests don't lose the history. Empty
// ContextSnippet stores NULL to mirror the Python behaviour of
// passing context_snippet=None by default.
func (e *Entities) LinkChunk(ctx context.Context, in OccurrenceLink) error {
	var snippet any
	if in.ContextSnippet != "" {
		snippet = in.ContextSnippet
	}
	return e.gdb.WithContext(ctx).Exec(`
		INSERT INTO entity_chunk_occurrences
			(id, entity_id, chunk_id, doc_id, occurrence_count,
			 context_snippet, position_in_chunk, relevance_score, created_at)
		VALUES (gen_random_uuid(), ?, ?, ?, 1, ?, ?, ?, NOW())
		ON CONFLICT (entity_id, chunk_id) DO UPDATE SET
			occurrence_count = entity_chunk_occurrences.occurrence_count + 1`,
		in.EntityID, in.ChunkID, in.DocID, snippet, in.PositionInChunk, in.RelevanceScore,
	).Error
}

// DeleteForDoc removes every occurrence row for a document. The entity
// rows themselves are shared across documents via canonical_form, so
// we never delete those here — orphan cleanup is a separate concern.
func (e *Entities) DeleteForDoc(ctx context.Context, docID uuid.UUID) error {
	return e.gdb.WithContext(ctx).
		Where("doc_id = ?", docID).
		Delete(&models.EntityChunkOccurrence{}).Error
}

// truncateString clips s to at most n runes. Python uses string slicing
// (rune-safe in UTF-8 strings the ORM accepts); we match the byte
// length since all column limits are byte lengths in Postgres.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// CountEntitiesForDoc returns how many distinct entities have
// occurrences on this document. Used by tests to assert the ingest
// stage ran.
func (e *Entities) CountEntitiesForDoc(ctx context.Context, docID uuid.UUID) (int64, error) {
	var n int64
	err := e.gdb.WithContext(ctx).Model(&models.EntityChunkOccurrence{}).
		Where("doc_id = ?", docID).
		Distinct("entity_id").
		Count(&n).Error
	return n, err
}
