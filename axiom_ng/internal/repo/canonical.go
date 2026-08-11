// Canonical mirror persistence: writes lossless zotero_items/collections and
// derives the normalized document/attachment projections in one atomic apply.
package repo

import (
	"context"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
	"github.com/jackc/pgx/v5"
)

// CanonicalCursor returns the separate canonical sync cursor for a source.
func (r *Repo) CanonicalCursor(ctx context.Context, sourceID string) (int64, error) {
	var v int64
	err := r.pool.QueryRow(ctx,
		`SELECT canonical_last_modified_version FROM zotero_sources WHERE id = $1`, sourceID).Scan(&v)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("canonical cursor: %w", err)
	}
	return v, nil
}

// SetCanonicalCursorTx advances the canonical cursor within the caller's
// transaction so the apply + cursor commit is atomic.
func (r *Repo) SetCanonicalCursorTx(ctx context.Context, tx pgx.Tx, sourceID string, version int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE zotero_sources
		SET canonical_last_modified_version = GREATEST(canonical_last_modified_version, $2)
		WHERE id = $1
	`, sourceID, version)
	if err != nil {
		return fmt.Errorf("set canonical cursor (tx): %w", err)
	}
	return nil
}

func (r *Repo) upsertCanonicalItem(ctx context.Context, tx pgx.Tx, sourceID string, it zotero.CanonicalItem) error {
	// A parent item has no parent: store NULL (not an empty string) so
	// parent_key IS NULL predicates select parents correctly.
	var parentKey any
	if it.ParentKey != "" {
		parentKey = it.ParentKey
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			zotero_version = GREATEST(zotero_items.zotero_version, EXCLUDED.zotero_version),
			item_type = EXCLUDED.item_type,
			parent_key = EXCLUDED.parent_key,
			raw_envelope = EXCLUDED.raw_envelope,
			raw_data = EXCLUDED.raw_data,
			deleted = false,
			synced_at = now(),
			updated_at = now()
		WHERE EXCLUDED.zotero_version >= zotero_items.zotero_version
	`, sourceID, it.Key, it.Version, it.ItemType, parentKey, it.Envelope, it.Data)
	if err != nil {
		return fmt.Errorf("upsert canonical item %s: %w", it.Key, err)
	}
	return nil
}

func (r *Repo) markCanonicalItemsMissing(ctx context.Context, tx pgx.Tx, sourceID string, presentKeys []string) error {
	if presentKeys == nil {
		presentKeys = []string{}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE zotero_items
		SET deleted = true, updated_at = now()
		WHERE source_id = $1 AND deleted = false
		  AND (cardinality($2::text[]) = 0 OR zotero_key <> ALL($2::text[]))
	`, sourceID, presentKeys); err != nil {
		return fmt.Errorf("mark canonical items missing: %w", err)
	}
	return nil
}

func (r *Repo) upsertCanonicalCollection(ctx context.Context, tx pgx.Tx, sourceID string, c zotero.CanonicalCollection) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO zotero_collections (source_id, zotero_key, name, parent_key, raw_envelope)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			name = EXCLUDED.name,
			parent_key = EXCLUDED.parent_key,
			raw_envelope = EXCLUDED.raw_envelope,
			deleted = false,
			synced_at = now(),
			updated_at = now()
	`, sourceID, c.Key, c.Name, c.ParentKey, c.Envelope)
	if err != nil {
		return fmt.Errorf("upsert canonical collection %s: %w", c.Key, err)
	}
	return nil
}

func (r *Repo) markCanonicalCollectionsMissing(ctx context.Context, tx pgx.Tx, sourceID string, presentKeys []string) error {
	if presentKeys == nil {
		presentKeys = []string{}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE zotero_collections
		SET deleted = true, updated_at = now()
		WHERE source_id = $1 AND deleted = false
		  AND (cardinality($2::text[]) = 0 OR zotero_key <> ALL($2::text[]))
	`, sourceID, presentKeys); err != nil {
		return fmt.Errorf("mark canonical collections missing: %w", err)
	}
	return nil
}
