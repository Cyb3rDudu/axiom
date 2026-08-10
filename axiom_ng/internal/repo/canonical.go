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

// SetCanonicalCursor advances the canonical sync cursor (monotonic).
func (r *Repo) SetCanonicalCursor(ctx context.Context, sourceID string, version int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE zotero_sources
		SET canonical_last_modified_version = GREATEST(canonical_last_modified_version, $2)
		WHERE id = $1
	`, sourceID, version)
	if err != nil {
		return fmt.Errorf("set canonical cursor: %w", err)
	}
	return nil
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

// ApplyCanonical writes the canonical items + collections atomically with a
// version guard and derives the normalized document/attachment projections. It
// marks items/collections that disappeared from the current listing as deleted
// and returns a flag per bibliographic parent whose preferred processable
// attachment should be enqueued. Runs on the caller-provided transaction.
func (r *Repo) ApplyCanonical(ctx context.Context, tx pgx.Tx, sourceID string, items []zotero.CanonicalItem, collections []zotero.CanonicalCollection) ([]CanonicalDocFlag, error) {
	presentItemKeys := make([]string, 0, len(items))
	for i := range items {
		if err := r.upsertCanonicalItem(ctx, tx, sourceID, items[i]); err != nil {
			return nil, err
		}
		presentItemKeys = append(presentItemKeys, items[i].Key)
	}
	if err := r.markCanonicalItemsMissing(ctx, tx, sourceID, presentItemKeys); err != nil {
		return nil, err
	}
	presentColKeys := make([]string, 0, len(collections))
	for _, c := range collections {
		if err := r.upsertCanonicalCollection(ctx, tx, sourceID, c); err != nil {
			return nil, err
		}
		presentColKeys = append(presentColKeys, c.Key)
	}
	if err := r.markCanonicalCollectionsMissing(ctx, tx, sourceID, presentColKeys); err != nil {
		return nil, err
	}
	return r.deriveDocumentProjections(ctx, tx, sourceID, items)
}

func (r *Repo) upsertCanonicalItem(ctx context.Context, tx pgx.Tx, sourceID string, it zotero.CanonicalItem) error {
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
	`, sourceID, it.Key, it.Version, it.ItemType, it.ParentKey, it.Envelope, it.Data)
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
