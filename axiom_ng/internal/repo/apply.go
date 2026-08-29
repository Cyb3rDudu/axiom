// Atomic canonical apply: in ONE transaction writes canonical rows, applies
// deletions, derives projections from the full active zotero_items state,
// updates memberships, writes pending/failed jobs and the cursor.
package repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zotero"
	"github.com/jackc/pgx/v5"
)

// AttachmentFileInfo holds pre-computed (pre-transaction) file facts for an
// attachment, used to write preferred/hash/stats and craft ingest jobs. Ids are
// resolved inside the transaction. When a file cannot be processed, ErrCode
// distinguishes FILE_NOT_FOUND (absent) from a retryable IO_ERROR.
type AttachmentFileInfo struct {
	LocalPath string
	Exists    bool
	Hash      string
	FileSize  int64
	MtimeMS   int64
	ErrCode   string
	ErrMsg    string
	Retryable bool
}

// CanonicalApplyResult summarises an atomic canonical apply.
type CanonicalApplyResult struct {
	Flags               []CanonicalDocFlag
	Enqueued            int
	FailedJobs          int
	DocumentProjections int
}

// ApplyCanonicalBatch atomically applies a canonical batch. markMissing of
// absent items only happens when batch.FullSnapshot is true (since==0);
// incremental batches never infer deletions from absence. Explicit
// deleteEvents are applied by resolving each key against documents or
// attachments. Projections are derived from the complete active state of
// zotero_items (not the delta). Pending and failed jobs plus the cursor are
// written in the same transaction via the caller-provided tx.
// The selection map gates job creation (#166): entries "excluded" suppress
// both pending and failed jobs for that document; nil = no gate (everything
// is selected — today's behavior). Projections stay a FULL mirror regardless.
func (r *Repo) ApplyCanonicalBatch(ctx context.Context, tx pgx.Tx, sourceID string, batch zotero.CanonicalBatch, collections []zotero.CanonicalCollection, files map[string]AttachmentFileInfo, selection map[string]string) (CanonicalApplyResult, error) {
	var res CanonicalApplyResult

	// 1. Upsert canonical items (version guarded).
	presentKeys := make([]string, 0, len(batch.Items))
	for _, it := range batch.Items {
		if err := r.upsertCanonicalItem(ctx, tx, sourceID, it); err != nil {
			return res, err
		}
		presentKeys = append(presentKeys, it.Key)
	}
	// 2. Mark missing items only on a full snapshot.
	if batch.FullSnapshot {
		if err := r.markCanonicalItemsMissing(ctx, tx, sourceID, presentKeys); err != nil {
			return res, err
		}
	}
	// 3. Absent from a full snapshot's canonical items are deleted; for a delta
	// only the deleteEvents are applied.
	if err := r.applyCanonicalDeleteEvents(ctx, tx, sourceID, batch.DeleteEvents); err != nil {
		return res, err
	}

	// 4. Collections. ListCanonicalCollections always returns a complete
	// snapshot, so missing collections are reconciled (marked deleted) on every
	// run — regardless of whether the item batch was a full snapshot.
	presentCols := make([]string, 0, len(collections))
	for _, c := range collections {
		if err := r.upsertCanonicalCollection(ctx, tx, sourceID, c); err != nil {
			return res, err
		}
		presentCols = append(presentCols, c.Key)
	}
	if err := r.markCanonicalCollectionsMissing(ctx, tx, sourceID, presentCols); err != nil {
		return res, err
	}

	// 5. Memberships for collections referenced by active items.
	if err := r.rebuildMemberships(ctx, tx, sourceID); err != nil {
		return res, err
	}

	// 6. Derive projections from the full active state.
	proj, err := r.deriveFullProjections(ctx, tx, sourceID, files)
	if err != nil {
		return res, err
	}
	res.Flags = proj.flags
	res.DocumentProjections = len(proj.flags)

	// 6b. Deleted attachments must stop serving: retire their active
	// snapshots (+ OS tombstones) in the same sync transaction. This runs
	// AFTER the projections — THEY write zotero_attachments.deleted, and a
	// retire placed earlier sees only the PREVIOUS sync's flag (the Mullins
	// zombie: the one sync that projected the fix-service deletion retired
	// nothing; every earlier heal survived only because follow-up syncs
	// ran). Same-tx same-sync is the contract the Durchpfad IT pins.
	if err := reconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		return res, fmt.Errorf("reconcile attachment snapshots: %w", err)
	}

	// 7. Pending + failed jobs in the same transaction, gated by the
	// selection (#166): excluded documents get no jobs; everything else
	// keeps the ON CONFLICT (attachment_id, content_hash) dedup — a
	// re-selected document with no existing job row gets one WITHOUT any
	// Zotero-side change (the acceptance case: the full derivation offers
	// every doc on every sync; only the gate held it back).
	if selection != nil {
		gatedPending := make([]PendingJob, 0, len(proj.pending))
		for _, p := range proj.pending {
			if !JobGated(selection, p.DocumentID) {
				gatedPending = append(gatedPending, p)
			}
		}
		gatedFailed := make([]FailedJob, 0, len(proj.failed))
		for _, f := range proj.failed {
			if !JobGated(selection, f.DocumentID) {
				gatedFailed = append(gatedFailed, f)
			}
		}
		proj.pending, proj.failed = gatedPending, gatedFailed
	}
	enqueued, failed, err := r.writeJobsTx(ctx, tx, sourceID, proj.pending, proj.failed)
	if err != nil {
		return res, err
	}
	res.Enqueued = enqueued
	res.FailedJobs = failed

	return res, nil
}

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
func reconcileAttachmentSnapshotsTx(ctx context.Context, tx pgx.Tx) error {
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

// applyCanonicalDeleteEvents resolves each deleted key against documents or
// attachments: a document deletion removes the parent + attachments; a single
// attachment deletion removes only that file.
func (r *Repo) applyCanonicalDeleteEvents(ctx context.Context, tx pgx.Tx, sourceID string, events []zotero.DeleteEvent) error {
	for _, ev := range events {
		if ev.Key == "" {
			continue
		}
		var isDoc bool
		_ = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM zotero_items WHERE source_id=$1 AND zotero_key=$2 AND deleted=false AND parent_key IS NULL)`,
			sourceID, ev.Key).Scan(&isDoc)
		if isDoc {
			// Parent document deleted: remove doc-level item + its children.
			if _, err := tx.Exec(ctx, `UPDATE zotero_items SET deleted=true, updated_at=now()
				WHERE source_id=$1 AND (zotero_key=$2 OR parent_key=$2)`, sourceID, ev.Key); err != nil {
				return fmt.Errorf("canonical delete parent %s: %w", ev.Key, err)
			}
			continue
		}
		// Single attachment (or note) deletion.
		if _, err := tx.Exec(ctx, `UPDATE zotero_items SET deleted=true, updated_at=now()
			WHERE source_id=$1 AND zotero_key=$2 AND deleted=false`, sourceID, ev.Key); err != nil {
			return fmt.Errorf("canonical delete attachment %s: %w", ev.Key, err)
		}
	}
	return nil
}

// writeJobsTx writes pending and failed ingest jobs within the given
// transaction (ON CONFLICT DO NOTHING dedup for pending; failed jobs are
// inserted). Returns (enqueued new jobs, failed jobs written).
func (r *Repo) writeJobsTx(ctx context.Context, tx pgx.Tx, sourceID string, pending []PendingJob, failed []FailedJob) (int, int, error) {
	inserted := 0
	for _, p := range pending {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
			VALUES ($1,$2,$3,$4,'pending',false)
			ON CONFLICT (attachment_id, content_hash) WHERE force_rebuild=false DO NOTHING
		`, p.SourceID, p.DocumentID, p.AttachmentID, p.ContentHash)
		if err != nil {
			return 0, 0, err
		}
		inserted += int(tag.RowsAffected())
		// A pending job means the file is processable again: any prior failed job
		// for this attachment is now resolved, so a later real failure can create
		// a fresh failed job instead of being masked by a stale one.
		if _, err := tx.Exec(ctx, `UPDATE ingest_jobs
			SET resolved_at=now(), updated_at=now()
			WHERE attachment_id=$1 AND status='failed' AND resolved_at IS NULL`, p.AttachmentID); err != nil {
			return 0, 0, err
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

func itemContentType(data json.RawMessage) string {
	var d struct {
		ContentType string `json:"contentType"`
	}
	_ = json.Unmarshal(data, &d)
	return d.ContentType
}

func itemFilename(data json.RawMessage) string {
	var d struct {
		Filename string `json:"filename"`
	}
	_ = json.Unmarshal(data, &d)
	return d.Filename
}

func itemLocalPath(env json.RawMessage) string {
	var e struct {
		Links struct {
			Enclosure struct {
				Href string `json:"href"`
			} `json:"enclosure"`
		} `json:"links"`
	}
	_ = json.Unmarshal(env, &e)
	return e.Links.Enclosure.Href
}

func itemLinkMode(data json.RawMessage) string {
	var d struct {
		LinkMode string `json:"linkMode"`
	}
	_ = json.Unmarshal(data, &d)
	return d.LinkMode
}

// isProjectable reports whether a canonical item type may become a document
// projection. Rather than maintaining an incomplete allow-list, every top-level
// item type is projectable EXCEPT the known non-document types (which have
// neither bibliographic metadata nor a processable file). Scientific types that
// carry a PDF/EPUB attachment — document, dataset, presentation, computerProgram,
// interview, patent, case, etc. — are therefore projected and enqueued.
func isProjectable(itemType string) bool {
	switch itemType {
	case "note", "attachment", "annotation":
		return false
	default:
		return true
	}
}

// rebuildMemberships (re)writes zotero_item_collections from active items'
// data.collections keys against the canonical collection keys. It loads the
// collection key->id map up front so it does not issue nested queries while a
// result set is open on the same transaction connection.
func (r *Repo) rebuildMemberships(ctx context.Context, tx pgx.Tx, sourceID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM zotero_item_collections
		WHERE item_id IN (SELECT id FROM zotero_items WHERE source_id=$1)`, sourceID); err != nil {
		return fmt.Errorf("clear memberships: %w", err)
	}
	// Collection key -> id map.
	colMap := map[string]string{}
	rows, err := tx.Query(ctx, `SELECT zotero_key, id::text FROM zotero_collections WHERE source_id=$1`, sourceID)
	if err != nil {
		return fmt.Errorf("query collections: %w", err)
	}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			rows.Close()
			return err
		}
		colMap[k] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	parents, err := tx.Query(ctx, `
		SELECT i.id, i.raw_data
		FROM zotero_items i
		WHERE i.source_id=$1 AND i.deleted=false AND (i.parent_key IS NULL OR i.parent_key='')`, sourceID)
	if err != nil {
		return fmt.Errorf("query active parents for memberships: %w", err)
	}
	// Collect parent -> collection refs first (do not run nested queries on the
	// transaction connection while a result set is open).
	var refs []struct{ itemID, colID string }
	for parents.Next() {
		var id string
		var raw []byte
		if err := parents.Scan(&id, &raw); err != nil {
			parents.Close()
			return err
		}
		var d struct {
			Collections []string `json:"collections"`
		}
		_ = json.Unmarshal(raw, &d)
		for _, ck := range d.Collections {
			if colID, ok := colMap[ck]; ok {
				refs = append(refs, struct{ itemID, colID string }{id, colID})
			}
		}
	}
	parents.Close()
	if err := parents.Err(); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO zotero_item_collections (item_id, collection_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, ref.itemID, ref.colID); err != nil {
			return fmt.Errorf("insert membership: %w", err)
		}
	}
	return nil
}
