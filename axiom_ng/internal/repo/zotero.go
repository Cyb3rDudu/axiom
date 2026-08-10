// Package repo provides database access for the axiom-ng orchestrator: the
// Zotero mirror tables and the ingest queue.
package repo

import (
	"context"
	"encoding/json"
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

// SetSourceVersion stores the last known library version after a sync.
func (r *Repo) SetSourceVersion(ctx context.Context, sourceID string, version int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE zotero_sources
		SET last_modified_version = $2, last_sync_at = now(), updated_at = now()
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
	err := tx.QueryRow(ctx, `
		INSERT INTO zotero_documents (
			source_id, zotero_key, zotero_version, item_type, title, creators,
			publication_year, url, tags, collections, metadata
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, '{}'::jsonb)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			zotero_version = EXCLUDED.zotero_version,
			item_type = EXCLUDED.item_type,
			title = EXCLUDED.title,
			creators = EXCLUDED.creators,
			publication_year = EXCLUDED.publication_year,
			url = EXCLUDED.url,
			tags = EXCLUDED.tags,
			collections = EXCLUDED.collections,
			deleted = false,
			updated_at = now()
		RETURNING id
	`,
		sourceID, item.Key, item.Version, item.ItemType, item.Title, creators,
		year, item.URL, tags, cols,
	).Scan(&docID)
	if err != nil {
		return fmt.Errorf("upsert document %s: %w", item.Key, err)
	}

	for _, att := range item.Attachments {
		if err := r.upsertAttachment(ctx, tx, sourceID, docID, item.Key, att); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) upsertAttachment(ctx context.Context, tx pgx.Tx, sourceID, docID, parentKey string, att zotero.Attachment) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO zotero_attachments (
			source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
			link_mode, content_type, filename, file_uri, local_path
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (source_id, zotero_key)
		DO UPDATE SET
			document_id = EXCLUDED.document_id,
			zotero_version = EXCLUDED.zotero_version,
			link_mode = EXCLUDED.link_mode,
			content_type = EXCLUDED.content_type,
			filename = EXCLUDED.filename,
			file_uri = EXCLUDED.file_uri,
			local_path = EXCLUDED.local_path,
			deleted = false,
			updated_at = now()
	`,
		sourceID, docID, att.Key, att.Version, parentKey, att.LinkMode,
		att.ContentType, att.Filename, att.LocalPath, att.LocalPath,
	)
	if err != nil {
		return fmt.Errorf("upsert attachment %s: %w", att.Key, err)
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
