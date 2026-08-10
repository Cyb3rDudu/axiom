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

// lockKey derives a stable bigint advisory-lock key from a source UUID so two
// concurrent syncs of the same source are serialised.
func lockKey(sourceID string) int64 {
	// Use the last 8 bytes of the UUID string's hash-like value; good enough
	// for a per-source lock and avoids any collision-sensitive hashing lib.
	var acc int64
	for i := 0; i < len(sourceID); i++ {
		acc = acc*31 + int64(sourceID[i])
	}
	return acc & 0x7FFFFFFFFFFFFFFF
}

// AcquireSourceLock acquires a session-level advisory lock for a source on a
// dedicated connection and returns a release function. The lock serialises a
// whole sync (cursor read, reconciliation, cursor commit) per source_id across
// pool connections, so a slow stale delta cannot overwrite a newer
// reconciliation. Release closes the dedicated connection, dropping the lock.
func (r *Repo) AcquireSourceLock(ctx context.Context, sourceID string) (func(), error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock conn: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey(sourceID)); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock source: %w", err)
	}
	return func() { conn.Release() }, nil
}

// LockSource acquires a session-level advisory lock for a source. It must be
// paired with UnlockSource. The lock serialises the whole sync per source_id so
// a slow stale delta cannot overwrite a newer reconciliation.
func (r *Repo) LockSource(ctx context.Context, sourceID string) error {
	_, err := r.pool.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey(sourceID))
	if err != nil {
		return fmt.Errorf("lock source: %w", err)
	}
	return nil
}

// UnlockSource releases the advisory lock acquired by LockSource.
func (r *Repo) UnlockSource(ctx context.Context, sourceID string) error {
	_, err := r.pool.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey(sourceID))
	if err != nil {
		return fmt.Errorf("unlock source: %w", err)
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

// ReconcileReq scopes attachment reconciliation to a subset of the library.
// A full sync sets ReconcileAll to cover every document of the source. An
// incremental sync must set AffectedDocKeys (and optionally DeletedDocKeys) so
// it never touches documents that were not part of this run; an empty delta
// reconciles nothing.
type ReconcileReq struct {
	SourceID             string
	ReconcileAll         bool
	AffectedDocKeys      []string
	DeletedDocKeys       []string
	SeenAttachments      map[string][]string // docKey -> attachment keys still present
	PreferredAttachments map[string]string   // docKey -> preferred attachment key
}

// Reconcile marks attachments that are no longer present as removed and clears
// the preferred flag from files that are no longer the chosen processable one.
// Scope is strictly limited:
//
//   - ReconcileAll true (full sync): every document of the source is reconciled
//     against SeenAttachments.
//   - otherwise: only AffectedDocKeys are reconciled; DeletedDocKeys have their
//     document and all attachments marked removed.
//   - an empty incremental scope changes nothing.
func (r *Repo) Reconcile(ctx context.Context, req ReconcileReq) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Determine the set of documents to reconcile: everything if full sync,
	// otherwise just the affected docs. Deleted docs are handled separately.
	var reconcileDocKeys []string
	if req.ReconcileAll {
		rows, err := tx.Query(ctx,
			`SELECT zotero_key FROM zotero_documents WHERE source_id = $1 AND deleted = false`,
			req.SourceID)
		if err != nil {
			return fmt.Errorf("reconcile list docs: %w", err)
		}
		reconcileDocKeys = make([]string, 0)
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			reconcileDocKeys = append(reconcileDocKeys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	} else if len(req.AffectedDocKeys) > 0 {
		reconcileDocKeys = req.AffectedDocKeys
	}

	// Reconcile each in-scope, non-deleted document's attachments.
	for _, docKey := range reconcileDocKeys {
		if containsReqKey(req.DeletedDocKeys, docKey) {
			continue // handled by the deletion path below
		}
		seen := req.SeenAttachments[docKey] // may be nil/empty
		if seen == nil {
			seen = []string{}
		}
		preferred := req.PreferredAttachments[docKey]
		if _, err := tx.Exec(ctx, `
			UPDATE zotero_attachments
			SET deleted = true, updated_at = now()
			WHERE source_id = $1
			  AND parent_zotero_key = $2
			  AND deleted = false
			  AND NOT (zotero_key = ANY($3::text[]))
		`, req.SourceID, docKey, seen); err != nil {
			return fmt.Errorf("reconcile removed attachments for %s: %w", docKey, err)
		}

		if preferred == "" {
			// No preferred processable file remains for this document: clear the
			// flag on any attachment that still had it.
			if _, err := tx.Exec(ctx, `
				UPDATE zotero_attachments
				SET preferred = false, updated_at = now()
				WHERE source_id = $1
				  AND parent_zotero_key = $2
				  AND preferred = true
			`, req.SourceID, docKey); err != nil {
				return fmt.Errorf("reconcile clear preferred for %s: %w", docKey, err)
			}
		} else if _, err := tx.Exec(ctx, `
			UPDATE zotero_attachments
			SET preferred = false, updated_at = now()
			WHERE source_id = $1
			  AND parent_zotero_key = $2
			  AND preferred = true
			  AND zotero_key <> $3
		`, req.SourceID, docKey, preferred); err != nil {
			return fmt.Errorf("reconcile preferred for %s: %w", docKey, err)
		}
	}

	// Deletion path: a deleted key can be either a parent document or a single
	// attachment. Resolve each generically: if the key is a document (parent),
	// mark the parent and all of its attachments removed; otherwise treat it as
	// an attachment and remove only that file (correcting preferred).
	for _, delKey := range req.DeletedDocKeys {
		var docDeleted bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM zotero_documents
				WHERE source_id = $1 AND zotero_key = $2 AND deleted = false
			)
		`, req.SourceID, delKey).Scan(&docDeleted)
		if err != nil {
			return fmt.Errorf("reconcile resolve deleted %s: %w", delKey, err)
		}

		if docDeleted {
			// Parent deletion: delete the document and all its attachments.
			if _, err := tx.Exec(ctx, `
				UPDATE zotero_attachments
				SET deleted = true, preferred = false, updated_at = now()
				WHERE source_id = $1 AND parent_zotero_key = $2 AND deleted = false
			`, req.SourceID, delKey); err != nil {
				return fmt.Errorf("reconcile delete parent attachments %s: %w", delKey, err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE zotero_documents
				SET deleted = true, updated_at = now()
				WHERE source_id = $1 AND zotero_key = $2
			`, req.SourceID, delKey); err != nil {
				return fmt.Errorf("reconcile delete document %s: %w", delKey, err)
			}
			continue
		}

		// Otherwise treat the key as a single deleted attachment.
		affected := int64(0)
		if err := tx.QueryRow(ctx, `
			UPDATE zotero_attachments SET deleted = true, preferred = false, updated_at = now()
			WHERE source_id = $1 AND zotero_key = $2 AND deleted = false
			RETURNING 1
		`, req.SourceID, delKey).Scan(&affected); err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("reconcile delete attachment %s: %w", delKey, err)
		}
		_ = affected
	}

	return tx.Commit(ctx)
}

// MarkMissingDocumentsDeleted marks documents of a source that were previously
// active but are no longer present in the current Zotero listing as deleted
// (with their attachments), so content removed entirely from Zotero does not
// linger as active in the mirror. Only called on a full sync, where presentKeys
// is the complete set of currently existing document keys.
func (r *Repo) MarkMissingDocumentsDeleted(ctx context.Context, sourceID string, presentKeys []string) error {
	if presentKeys == nil {
		presentKeys = []string{}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	mark, err := tx.Exec(ctx, `
		UPDATE zotero_documents
		SET deleted = true, updated_at = now()
		WHERE source_id = $1
		  AND deleted = false
		  AND (cardinality($2::text[]) = 0 OR zotero_key <> ALL($2::text[]))
	`, sourceID, presentKeys)
	if err != nil {
		return fmt.Errorf("mark missing documents: %w", err)
	}
	if mark.RowsAffected() > 0 {
		// Also remove the attachments of the vanished documents.
		if _, err := tx.Exec(ctx, `
			UPDATE zotero_attachments a
			SET deleted = true, preferred = false, updated_at = now()
			FROM zotero_documents d
			WHERE a.source_id = $1 AND a.document_id = d.id AND d.deleted = true AND a.deleted = false
		`, sourceID); err != nil {
			return fmt.Errorf("mark missing document attachments: %w", err)
		}
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

// containsReqKey reports whether key is present in keys.
func containsReqKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
