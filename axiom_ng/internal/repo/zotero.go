// Package repo provides database access for the axiom-ng orchestrator: the
// Zotero mirror tables and the ingest queue.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo wraps the pgx pool with methods for the Zotero mirror and ingest queue.
type Repo struct {
	pool *pgxpool.Pool
}

// New builds a Repo from an existing pool.
func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// EnsureSource returns the id of a zotero_sources row for the given base URL
// and library, creating it if absent (upsert on the unique pair).
func (r *Repo) EnsureSource(ctx context.Context, baseURL, libraryID, serverID string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id, server_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (base_url, library_id)
		DO UPDATE SET server_id = EXCLUDED.server_id, updated_at = now()
		RETURNING id
	`, baseURL, libraryID, serverID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure source: %w", err)
	}
	return id, nil
}

// SetSourceVersion advances the stored library version, never moving it
// backwards. Under concurrent Sync calls a slower goroutine with an older
// version cannot rewind the cursor below what another goroutine already wrote.
func (r *Repo) SetSourceVersion(ctx context.Context, sourceID string, version int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE zotero_sources
		SET last_modified_version = GREATEST(last_modified_version, $2),
		    last_sync_at = now(), updated_at = now()
		WHERE id = $1
	`, sourceID, version)
	if err != nil {
		return fmt.Errorf("set source version: %w", err)
	}
	return nil
}

// SourceVersion returns the stored library version for a source (0 if none).
func (r *Repo) SourceVersion(ctx context.Context, sourceID string) (int64, error) {
	var v int64
	err := r.pool.QueryRow(ctx,
		`SELECT last_modified_version FROM zotero_sources WHERE id = $1`, sourceID).Scan(&v)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("source version: %w", err)
	}
	return v, nil
}

// SyncDocuments mirrors the given Zotero items (and their attachments) into
// the zotero_documents/zotero_attachments tables.
func (r *Repo) SyncDocuments(ctx context.Context, sourceID string, items []zotero.Item) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, item := range items {
		if err := r.upsertDocument(ctx, tx, sourceID, item); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (r *Repo) upsertDocument(ctx context.Context, tx pgx.Tx, sourceID string, item zotero.Item) error {
	creators, _ := json.Marshal(item.Creators)
	tags, _ := json.Marshal(item.Tags)
	cols, _ := json.Marshal(item.Collections)
	year := 0
	if item.PublicationYear != nil {
		year = *item.PublicationYear
	}

	var docID string
	row := tx.QueryRow(ctx, `
		INSERT INTO zotero_documents (
			source_id, zotero_key, zotero_version, item_type, title, creators,
			publication_year, url, tags, collections, metadata
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, '{}'::jsonb)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			zotero_version = GREATEST(zotero_documents.zotero_version, EXCLUDED.zotero_version),
			item_type = EXCLUDED.item_type,
			title = EXCLUDED.title,
			creators = EXCLUDED.creators,
			publication_year = EXCLUDED.publication_year,
			url = EXCLUDED.url,
			tags = EXCLUDED.tags,
			collections = EXCLUDED.collections,
			deleted = false,
			updated_at = now()
		WHERE EXCLUDED.zotero_version >= zotero_documents.zotero_version
		RETURNING id
	`,
		sourceID, item.Key, item.Version, item.ItemType, item.Title, creators,
		year, item.URL, tags, cols,
	)
	if err := row.Scan(&docID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("upsert document %s: %w", item.Key, err)
		}
		// Incoming version is older than the stored one; the DO UPDATE WHERE
		// guard suppressed the write and therefore returned no row. Keep the
		// existing document id so attachments can still reference it.
		if err2 := tx.QueryRow(ctx,
			`SELECT id::text FROM zotero_documents WHERE source_id = $1 AND zotero_key = $2`,
			sourceID, item.Key).Scan(&docID); err2 != nil {
			return fmt.Errorf("lookup document %s: %w", item.Key, err2)
		}
	}

	for _, att := range item.Attachments {
		if err := r.upsertAttachment(ctx, tx, sourceID, docID, item.Key, att); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) upsertAttachment(ctx context.Context, tx pgx.Tx, sourceID, docID, parentKey string, att zotero.Attachment) error {
	// local_path must be a native filesystem path for downstream processors;
	// keep the original Zotero file:// URI separately in file_uri.
	native := zotero.LocalFilePath(att.LocalPath)
	_, err := tx.Exec(ctx, `
		INSERT INTO zotero_attachments (
			source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
			link_mode, content_type, filename, file_uri, local_path
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			document_id = EXCLUDED.document_id,
			zotero_version = GREATEST(zotero_attachments.zotero_version, EXCLUDED.zotero_version),
			link_mode = EXCLUDED.link_mode,
			content_type = EXCLUDED.content_type,
			filename = EXCLUDED.filename,
			file_uri = EXCLUDED.file_uri,
			local_path = EXCLUDED.local_path,
			deleted = false,
			updated_at = now()
		WHERE EXCLUDED.zotero_version >= zotero_attachments.zotero_version
	`,
		sourceID, docID, att.Key, att.Version, parentKey, att.LinkMode,
		att.ContentType, att.Filename, att.LocalPath, native,
	)
	if err != nil {
		return fmt.Errorf("upsert attachment %s: %w", att.Key, err)
	}
	return nil
}

// Reconcile marks attachments that are no longer present in the library as
// removed, and clears the preferred flag from attachments that are no longer
// the chosen processable file so a change of the preferred file (or switch to
// an unsupported type) is reflected. Data already persisted from a later
// removed attachment is thereby marked for pruning rather than staying active.
func (r *Repo) Reconcile(ctx context.Context, sourceID string, seenKeys []string, preferredKeys []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE zotero_attachments
		SET deleted = true, updated_at = now()
		WHERE source_id = $1 AND deleted = false
		  AND (cardinality($2::text[]) = 0 OR zotero_key <> ALL($2::text[]))
	`, sourceID, seenKeys); err != nil {
		return fmt.Errorf("reconcile removed attachments: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE zotero_attachments
		SET preferred = false, updated_at = now()
		WHERE source_id = $1 AND preferred = true
		  AND (cardinality($2::text[]) = 0 OR zotero_key <> ALL($2::text[]))
	`, sourceID, preferredKeys); err != nil {
		return fmt.Errorf("reconcile preferred: %w", err)
	}

	return tx.Commit(ctx)
}

// UpdateAttachmentFileInfo persists the resolved content hash, size, mtime and
// preferred flag for one attachment after its local file has been inspected.
func (r *Repo) UpdateAttachmentFileInfo(ctx context.Context, sourceID, zoteroKey string, hash string, fileSize int64, mtimeMs int64, preferred bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE zotero_attachments
		SET content_hash = $3, file_size = $4, mtime_ms = $5, preferred = $6,
		    updated_at = now()
		WHERE source_id = $1 AND zotero_key = $2
	`, sourceID, zoteroKey, hash, fileSize, mtimeMs, preferred)
	if err != nil {
		return fmt.Errorf("update attachment file info %s: %w", zoteroKey, err)
	}
	return nil
}

// DocumentID returns the mirrored document id for a Zotero item key.
func (r *Repo) DocumentID(ctx context.Context, sourceID, zoteroKey string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE source_id = $1 AND zotero_key = $2`,
		sourceID, zoteroKey).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("document id for %s: %w", zoteroKey, err)
	}
	return id, nil
}

// AttachmentID returns the mirrored attachment id for a Zotero key.
func (r *Repo) AttachmentID(ctx context.Context, sourceID, zoteroKey string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_attachments WHERE source_id = $1 AND zotero_key = $2`,
		sourceID, zoteroKey).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("attachment id for %s: %w", zoteroKey, err)
	}
	return id, nil
}
