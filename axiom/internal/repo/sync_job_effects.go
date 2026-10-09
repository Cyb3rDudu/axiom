// sync_job_effects.go — the sync's STORE-side effects (#358): the mirror
// apply commits on the Library database; these methods run in the sync's
// separate store-effect transaction on the Store database (projection
// writes, revision intake, failed-file records, snapshot reconciliation).
// The projection (store_documents) is the Store's only rendition truth —
// the reconcile reads it, never the Library's mirror.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// reconcileAttachmentSnapshotsTx (formerly retireDeletedAttachmentsTx — it
// RETIRES and RESTORES) deactivates active snapshots whose Zotero
// attachment is deleted (source-heal swaps delete the old storage key and
// create a NEW attachment row; the old row's snapshot used to keep serving
// zombie chunks next to the healed rechunk — #176 delta, 2× live snapshots
// on one document). Mirrors deactivateSiblingsTx + outbox tombstones (#127)
// so OpenSearch stops serving them in the same transaction.
//
// Restore mirror (review C1): a deleted-then-RESTORED attachment whose
// completed snapshot was retired has no other reactivation path — the
// pending-job insert dedups against the completed job (idempotency index
// has no status predicate), so persistTx never runs again and the document
// would silently stay unserved. Reactivate its latest completed snapshot
// (one per attachment — the partial unique index is respected by picking a
// single winner) and re-materialize it via an index outbox op.
//
// Known window: a job mid-flight on an attachment that gets deleted commits
// AFTER this step and reactivates/activates its snapshot; the NEXT sync's
// pass here re-retires it. Bounded by one sync interval, drainer guards
// keep OpenSearch convergent.
func (r *Repo) ReconcileAttachmentSnapshotsTx(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		UPDATE processing_snapshots s SET active=false, updated_at=now()
		FROM store_documents p
		WHERE p.attachment_id = s.attachment_id AND p.deleted = true AND s.active = true
		RETURNING s.id::text, s.document_id::text, s.attachment_id::text`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type retired struct{ snap, doc, att string }
	var hits []retired
	for rows.Next() {
		var h retired
		if err := rows.Scan(&h.snap, &h.doc, &h.att); err != nil {
			return err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, h := range hits {
		if err := enqueueOutboxTx(ctx, tx, h.snap, OutboxOpDelete,
			jobIdentity{documentID: h.doc, attachmentID: h.att}); err != nil {
			return fmt.Errorf("tombstone deleted-attachment snapshot %s: %w", h.snap, err)
		}
	}
	return reactivateRestoredAttachmentsTx(ctx, tx)
}

func reactivateRestoredAttachmentsTx(ctx context.Context, tx pgx.Tx) error {
	// Latest completed snapshot of a LIVE attachment that currently serves
	// nothing. "Latest" = highest created_at (tie: max id) — deterministic.
	// #228 review (C1): DISTINCT ON (s.document_id) — the 0019 invariant is
	// one active snapshot per DOCUMENT, so a document whose PDF and EPUB
	// attachments BOTH hold retired snapshots (restored twins) revives
	// exactly ONE winner; the previous per-attachment pick selected both,
	// the second activation hit the 0019 partial unique index and rolled
	// back the ENTIRE canonical sync transaction — a permanent crash loop
	// (stuck cursor, ingestion blocked for every document). The winner is
	// the document's latest snapshot across its live attachments
	// (created_at DESC, tie: max id — deterministic).
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (s.document_id)
			s.id::text, s.document_id::text, s.attachment_id::text
		FROM processing_snapshots s
		JOIN store_documents p ON p.attachment_id = s.attachment_id
		WHERE p.deleted = false
		  AND s.active = false
		  AND NOT EXISTS (
			-- #228: document scope — the 0019 invariant is one active per
			-- DOCUMENT; reviving a second format's snapshot next to the
			-- document's current canonical view would violate it.
			SELECT 1 FROM processing_snapshots x
			WHERE x.document_id = s.document_id AND x.active
		  )
		ORDER BY s.document_id, s.created_at DESC, s.id DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type back struct{ snap, doc, att string }
	var revives []back
	for rows.Next() {
		var b back
		if err := rows.Scan(&b.snap, &b.doc, &b.att); err != nil {
			return err
		}
		revives = append(revives, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range revives {
		// Belt-and-braces: the activation re-checks the document-scoped guard
		// so a stale candidate picked between the SELECT above and this UPDATE
		// (or a future regression of the query) can never land a second active
		// snapshot and fail the whole sync on the 0019 index.
		tag, err := tx.Exec(ctx, `
			UPDATE processing_snapshots SET active=true, updated_at=now()
			WHERE id=$1 AND NOT EXISTS (
				SELECT 1 FROM processing_snapshots x
				WHERE x.document_id = (SELECT document_id FROM processing_snapshots WHERE id=$1)
					AND x.active AND x.id<>$1
			)`, b.snap)
		if err != nil {
			return fmt.Errorf("reactivate restored-attachment snapshot %s: %w", b.snap, err)
		}
		if tag.RowsAffected() == 0 {
			continue // another candidate won the document — nothing to re-index
		}
		if err := enqueueOutboxTx(ctx, tx, b.snap, OutboxOpIndex,
			jobIdentity{documentID: b.doc, attachmentID: b.att}); err != nil {
			return fmt.Errorf("reindex restored-attachment snapshot %s: %w", b.snap, err)
		}
	}
	return nil
}

// AttachmentProjectionDeleted reports whether the rendition's projection
// row is currently deleted — the dispatcher's preflight-time zombie guard
// (#365): a job claimed just before its rendition got replaced must not
// mint a defect verdict + repair case for a file that no longer exists;// the replacement's own job is the live truth.
func (r *Repo) AttachmentProjectionDeleted(ctx context.Context, attachmentID string) (bool, error) {
	var deleted bool
	err := r.pool.QueryRow(ctx,
		`SELECT deleted FROM store_documents WHERE attachment_id=$1::uuid`, attachmentID).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // no row = not known-deleted; the claim's guards own it
	}
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// UnservedRendition is one live preferred projection row that serves
// nothing and has no work in flight — the sync's self-heal offer set
// (#365: the changed-set alone can never re-offer these).
type UnservedRendition struct {
	DocumentID    string
	SourceID      string
	RecordKey     string
	RenditionKey  string
	Version       int64
	Hash          string
	Title         string
	Creators      []byte // JSON string array (the projection's flattened form)
	Year          *int
	Publisher     string
	Language      string
	Tags          []byte // JSON string array
	CitationClass string
	ContentType   string
}

// UnservedPreferredRenditionsTx returns the source's live preferred
// renditions that serve nothing (no active snapshot) and have no work in
// flight (no pending/claimed/processing job under their revision
// identity). The sync's self-heal sweep mints their intake — a terminally
// dead job or a missed mint must not strand a document until an operator
// force-rebuild (#365).
func (r *Repo) UnservedPreferredRenditionsTx(ctx context.Context, tx pgx.Tx, sourceID string) ([]UnservedRendition, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.document_id::text, p.source_id::text, p.record_key, p.rendition_key, p.source_version,
		       COALESCE(p.content_hash,''), p.title, p.creators, p.publication_year,
		       p.publisher, p.language, p.tags, p.citation_class, p.content_type
		FROM store_documents p
		WHERE p.source_id::text = $1 AND p.deleted = false AND p.preferred = true
		  AND p.content_hash IS NOT NULL AND p.content_hash <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM processing_snapshots s
			WHERE s.attachment_id = p.attachment_id AND s.active)
		  AND NOT EXISTS (
			SELECT 1 FROM ingest_jobs j
			WHERE j.revision_source_id = p.source_id::text
			  AND j.revision_rendition_id = p.rendition_key
			  AND j.status IN ('pending','claimed','processing'))
		  AND NOT EXISTS (
			-- the attempt ceiling is a verdict, not a suggestion: a rendition
			-- whose jobs burned all attempts at this content stays dead —
			-- force-rebuild remains the operator escape (no per-sync thrash)
			SELECT 1 FROM ingest_jobs x
			WHERE x.revision_source_id = p.source_id::text
			  AND x.revision_rendition_id = p.rendition_key
			  AND x.content_hash = p.content_hash
			  AND x.status = 'failed' AND x.attempt >= x.max_attempts)`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnservedRendition
	for rows.Next() {
		var u UnservedRendition
		if err := rows.Scan(&u.DocumentID, &u.SourceID, &u.RecordKey, &u.RenditionKey, &u.Version,
			&u.Hash, &u.Title, &u.Creators, &u.Year, &u.Publisher, &u.Language,
			&u.Tags, &u.CitationClass, &u.ContentType); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RetireDeletedRenditionJobsTx (#365) obsoletes the non-terminal jobs of
// renditions deleted THIS sync — in the same transaction as the projection
// delete marks. A job minted while the rendition was live must not survive
// as a zombie: claimed later (the lane may queue it for hours), it would
// preflight a file that no longer exists and mint a repair case for a
// rendition that was simply REPLACED (the production case: a wave-queued
// job for a deleted attachment failed preflight "defekt/DRM" while its
// replacement never processed). Identity match is by revision keys —
// unclaimed revision jobs carry NULL attachment FKs until their first
// claim. Idempotent: terminal rows are untouched.
func (r *Repo) RetireDeletedRenditionJobsTx(ctx context.Context, tx pgx.Tx, attachmentIDs []string) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE ingest_jobs j SET
			status='skipped', error_code='SKIPPED',
			error_message='rendition deleted (replaced or removed)',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			next_attempt_at=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		FROM store_documents p
		WHERE p.attachment_id = ANY($1::uuid[])
		  AND p.rendition_key = j.revision_rendition_id
		  AND p.source_id::text = j.revision_source_id
		  AND j.status IN ('pending','claimed','processing')`, attachmentIDs)
	return err
}

// WriteFailedJobsTx records file-resolution failures as terminal failed
// ingest rows (the sync's store-effect phase). The pending-lane mint died
// with the legacy lane (#358): new work flows through revision intake
// (EnqueueRevisionIntakeTx); these rows are listing signals only —
// status='failed' at birth, never claimed. Idempotent only against an
// UNRESOLVED identical failure; a historical (resolved) failure must not
// suppress a new error event.
func (r *Repo) WriteFailedJobsTx(ctx context.Context, tx pgx.Tx, failed []FailedJob) (int, error) {
	failedWritten := 0
	for _, f := range failed {
		code := f.ErrorCode
		if code == "" {
			code = "IO_ERROR"
		}
		maxAt := 0
		if f.Retryable {
			maxAt = 3
		}
		var existing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM ingest_jobs WHERE attachment_id=$1 AND status='failed' AND error_code=$2 AND resolved_at IS NULL
		)`, f.AttachmentID, code).Scan(&existing); err != nil {
			return 0, err
		}
		if existing {
			continue
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, status, error_code, error_message, max_attempts)
			VALUES ($1,$2,$3,'failed',$4,$5,$6)
		`, f.SourceID, f.DocumentID, f.AttachmentID, code, f.ErrorMessage, maxAt)
		if err != nil {
			return 0, err
		}
		failedWritten += int(tag.RowsAffected())
	}
	return failedWritten, nil
}

// ResolveAttachmentFailuresTx closes prior unresolved failed rows for a
// rendition once fresh work is minted again (the intake-mint successor of
// the legacy pending-insert resolution): a stale FILE_NOT_FOUND must not
// mask or outrank the new attempt. Like its predecessor it never touches
// updated_at — bookkeeping must not re-rank the outcome read model.
func (r *Repo) ResolveAttachmentFailuresTx(ctx context.Context, tx pgx.Tx, attachmentID string) error {
	_, err := tx.Exec(ctx, `UPDATE ingest_jobs
		SET resolved_at=now()
		WHERE attachment_id=$1::uuid AND status='failed' AND resolved_at IS NULL`, attachmentID)
	return err
}
