// sync_job_effects.go — the legacy sync lane's writes into STORE-owned
// tables, exported for internal/library/mirror (F09 #303: the Zotero
// mirror persistence moved to the Library side; these job/snapshot effects
// stay Store-owned and are called by the mirror's canonical apply inside
// the sync transaction — the documented dual-write of the transition,
// abated when revision intake replaces the legacy lane / F12 splits
// persistence).
package repo

import (
	"context"
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
		FROM zotero_attachments a
		WHERE a.id = s.attachment_id AND a.deleted = true AND s.active = true
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
		JOIN zotero_attachments a ON a.id = s.attachment_id
		WHERE a.deleted = false
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
// writeJobsTx writes pending and failed ingest jobs within the given
// transaction (ON CONFLICT DO NOTHING dedup for pending; failed jobs are
// inserted). Returns (enqueued new jobs, failed jobs written).
// #294 defense in depth: a pending insert is suppressed when the
// attachment already has an ACTIVE snapshot for the SAME content hash —
// the snapshot is the proof of processing (served chunks/index), so a
// document whose job rows were pruned (retention) or lost any other way
// is NOT re-enqueued as "never processed" while its content is unchanged.
// A changed hash matches no active snapshot → the job enqueues normally;
// explicit force rebuilds take a different path (force_rebuild=true) and
// are never suppressed here.
func (r *Repo) WriteSyncJobsTx(ctx context.Context, tx pgx.Tx, sourceID string, pending []PendingJob, failed []FailedJob) (int, int, error) {
	inserted := 0
	for _, p := range pending {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
			SELECT $1,$2,$3,$4,'pending',false
			WHERE NOT EXISTS (
				-- #294: active snapshot for the SAME content = processed & served
				SELECT 1 FROM processing_snapshots s
				WHERE s.attachment_id = $3 AND s.content_hash = $4 AND s.active)
			ON CONFLICT (attachment_id, content_hash) WHERE force_rebuild=false DO NOTHING
		`, p.SourceID, p.DocumentID, p.AttachmentID, p.ContentHash)
		if err != nil {
			return 0, 0, err
		}
		inserted += int(tag.RowsAffected())
		// A pending job means the file is processable again: any prior failed job
		// for this attachment is now resolved, so a later real failure can create
		// a fresh failed job instead of being masked by a stale one.
		// #294 review (MAJOR 2): ONLY on an ACTUAL enqueue — a suppressed insert
		// (snapshot served) processes nothing, so it resolves nothing. And the
		// resolution sets resolved_at WITHOUT touching updated_at: the outcome
		// read model orders by (updated_at, id), and bookkeeping must never
		// re-rank the outcome row (the old bump let a stale failed row outrank
		// the completed anchor → retention pruned the anchor while the sync
		// defense kept the doc served at outcome=failed forever).
		// Accepted residual (#294 review): the ON CONFLICT path (a pending
		// row already exists for this attachment_id+content_hash) also skips
		// resolution — that only leaves a stale error message on the failed
		// row and never re-ranks the outcome key.
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `UPDATE ingest_jobs
				SET resolved_at=now()
				WHERE attachment_id=$1 AND status='failed' AND resolved_at IS NULL`, p.AttachmentID); err != nil {
				return 0, 0, err
			}
		}
	}
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
		// Idempotent only against an UNRESOLVED identical failure; a historical
		// (resolved) failure must not suppress a new error event.
		var existing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM ingest_jobs WHERE attachment_id=$1 AND status='failed' AND error_code=$2 AND resolved_at IS NULL
		)`, f.AttachmentID, code).Scan(&existing); err != nil {
			return 0, 0, err
		}
		if existing {
			continue
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, status, error_code, error_message, max_attempts)
			VALUES ($1,$2,$3,'failed',$4,$5,$6)
		`, f.SourceID, f.DocumentID, f.AttachmentID, code, f.ErrorMessage, maxAt)
		if err != nil {
			return 0, 0, err
		}
		failedWritten += int(tag.RowsAffected())
	}
	return inserted, failedWritten, nil
}
