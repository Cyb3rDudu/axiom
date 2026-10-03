package repo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SupersededEntity is the archived semantic evidence of an entity row deleted
// by exact-form consolidation.
type SupersededEntity struct {
	ID               string    `json:"id"`
	SurvivorEntityID string    `json:"survivor_entity_id"`
	LoserEntityID    string    `json:"loser_entity_id"`
	LoserRef         string    `json:"loser_ref"`
	LoserText        string    `json:"loser_text"`
	LoserCanonical   *string   `json:"loser_canonical_form,omitempty"`
	LoserType        *string   `json:"loser_type,omitempty"`
	LoserDescription *string   `json:"loser_description,omitempty"`
	LoserSnapshotID  string    `json:"loser_snapshot_id"`
	LoserDocumentID  string    `json:"loser_document_id"`
	MentionCount     int       `json:"mention_count"`
	Operation        string    `json:"operation"`
	ArchivedAt       time.Time `json:"archived_at"`
}

func archiveSupersededEntitiesTx(ctx context.Context, tx pgx.Tx, survivors, losers []string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO kg_superseded_entities
		  (survivor_entity_id, loser_entity_id, loser_ref, loser_text,
		   loser_canonical_form, loser_type, loser_description,
		   loser_snapshot_id, loser_document_id, mention_count, operation)
		SELECT v.survivor, e.id, e.ref, e.text, e.canonical_form, e.type, e.description,
		       e.snapshot_id, s.document_id, count(DISTINCT m.chunk_id)::int,
		       'entity_consolidation'
		FROM (SELECT unnest($1::uuid[]) AS survivor, unnest($2::uuid[]) AS loser) v
		JOIN processing_entities e ON e.id = v.loser
		JOIN processing_snapshots s ON s.id = e.snapshot_id AND s.active
		LEFT JOIN processing_entity_mentions m ON m.entity_id = e.id
		GROUP BY v.survivor, e.id, e.ref, e.text, e.canonical_form, e.type,
		         e.description, e.snapshot_id, s.document_id
		ON CONFLICT (survivor_entity_id, loser_entity_id, operation) DO NOTHING`, survivors, losers)
	return err
}

// SupersededEntityHistory lists deleted entity rows archived under a survivor.
func (r *Repo) SupersededEntityHistory(ctx context.Context, survivorEntityID string) ([]SupersededEntity, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, survivor_entity_id::text, loser_entity_id::text,
		       loser_ref, loser_text, loser_canonical_form, loser_type,
		       loser_description, loser_snapshot_id::text, loser_document_id::text,
		       mention_count, operation, archived_at
		FROM kg_superseded_entities
		WHERE survivor_entity_id = $1::uuid
		ORDER BY archived_at, loser_entity_id`, survivorEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SupersededEntity{}
	for rows.Next() {
		var e SupersededEntity
		if err := rows.Scan(&e.ID, &e.SurvivorEntityID, &e.LoserEntityID,
			&e.LoserRef, &e.LoserText, &e.LoserCanonical, &e.LoserType,
			&e.LoserDescription, &e.LoserSnapshotID, &e.LoserDocumentID,
			&e.MentionCount, &e.Operation, &e.ArchivedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
