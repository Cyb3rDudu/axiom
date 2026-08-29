package repo

import (
	"context"
	"testing"
)

// TestAttachmentRetireAndRestoreIT pins the #176 zombie-retirement and its
// restore mirror: a Zotero-deleted attachment's active snapshot goes inactive
// with an outbox tombstone in the same transaction; undeleting the attachment
// reactivates its latest completed snapshot with an index outbox op — without
// the mirror, a restored attachment had NO reactivation path (pending-job
// insert dedups against the completed job, persistTx never runs again) and
// the document silently stayed unserved.
func TestAttachmentRetireAndRestoreIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "retire-hash-1"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-1",
		docKey: "RETIREDOC", attKey: "RETIREATT", contentHash: &ch}, "completed", 1)

	var snapID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active)
		VALUES ($1, $2, 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', true)
		RETURNING id::text`, attID, ch).Scan(&snapID); err != nil {
		t.Fatalf("seed active snapshot: %v", err)
	}

	// 1. delete the attachment -> retirement flips the snapshot inactive and
	// plans an outbox tombstone.
	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE zotero_attachments SET deleted=true WHERE id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if err := reconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var active bool
	if err := lr.pool.QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE id=$1`, snapID).Scan(&active); err != nil || active {
		t.Fatalf("deleted attachment's snapshot must be inactive: active=%v err=%v", active, err)
	}
	var ops []string
	rows, err := lr.pool.Query(ctx, `SELECT operation FROM opensearch_outbox WHERE snapshot_id=$1 ORDER BY created_at`, snapID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var op string
		if err := rows.Scan(&op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	rows.Close()
	if len(ops) == 0 || ops[len(ops)-1] != OutboxOpDelete {
		t.Fatalf("expected a delete tombstone planned, got %v", ops)
	}

	// 2. restore the attachment -> the mirror reactivates the latest
	// completed snapshot and plans an index op.
	tx, err = lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE zotero_attachments SET deleted=false WHERE id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if err := reconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		t.Fatalf("reconcile after restore: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `SELECT active FROM processing_snapshots WHERE id=$1`, snapID).Scan(&active); err != nil || !active {
		t.Fatalf("restored attachment's snapshot must be active again: active=%v err=%v", active, err)
	}
	rows, err = lr.pool.Query(ctx, `SELECT operation FROM opensearch_outbox WHERE snapshot_id=$1 ORDER BY created_at DESC LIMIT 1`, snapID)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	if rows.Next() {
		if err := rows.Scan(&last); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	if last != OutboxOpIndex {
		t.Fatalf("restore must plan an index op, got %q", last)
	}
}

// TestRestoreTwinAttachmentsReviveOneIT pins the #228 review C1 fix: a
// document with TWO live attachments (PDF + EPUB twins) whose snapshots are
// BOTH retired and no active snapshot on the document revives EXACTLY ONE
// winner. The pre-fix per-attachment DISTINCT selected both, the second
// activation hit the 0019 partial unique index and the error rolled back
// the ENTIRE canonical sync transaction — a permanent crash loop.
func TestRestoreTwinAttachmentsReviveOneIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch1 := "sha256:restore-pdf"
	att1, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.twins", libraryID: "lib-1",
		docKey: "TWINDOC", attKey: "TWINPDF", contentHash: &ch1}, "completed", 1)

	// second live attachment on the SAME document (the EPUB twin)
	var att2 string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments
		  (source_id, document_id, zotero_key, zotero_version, parent_zotero_key,
		   link_mode, content_type, filename, local_path, content_hash, preferred)
		SELECT source_id, document_id, 'TWINEPUB', 1, parent_zotero_key,
		       'linked_file', 'application/epub+zip', 'book.epub', '/tmp/book.epub', 'sha256:restore-epub', true
		FROM zotero_attachments WHERE id=$1
		RETURNING id::text`, att1).Scan(&att2); err != nil {
		t.Fatalf("twin attachment: %v", err)
	}

	// both snapshots retired; the epub one is LATER (created_at) so it must win
	var snap1, snap2 string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active, created_at)
		VALUES ($1, $2, 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', false, now() - interval '2 hours')
		RETURNING id::text`, att1, ch1).Scan(&snap1); err != nil {
		t.Fatalf("seed retired pdf snapshot: %v", err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
			processor_version, profile_hash, document_id, profile, active, created_at)
		VALUES ($1, 'sha256:restore-epub', 'p', 'v', 'ph', (SELECT document_id FROM zotero_attachments WHERE id=$1), '{}', false, now())
		RETURNING id::text`, att2).Scan(&snap2); err != nil {
		t.Fatalf("seed retired epub snapshot: %v", err)
	}

	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileAttachmentSnapshotsTx(ctx, tx); err != nil {
		t.Fatalf("restore reconcile must not error on twin attachments: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// exactly ONE active snapshot on the document — the LATEST (epub twin)
	var activeID string
	if err := lr.pool.QueryRow(ctx, `
		SELECT s.id::text FROM processing_snapshots s
		WHERE s.document_id = (SELECT document_id FROM zotero_attachments WHERE id=$1) AND s.active`,
		att1).Scan(&activeID); err != nil {
		t.Fatalf("exactly one active snapshot expected: %v", err)
	}
	if activeID != snap2 {
		t.Fatalf("revive winner must be the latest snapshot %s, got %s", snap2, activeID)
	}
	var ops int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM opensearch_outbox WHERE snapshot_id=$1 AND operation=$2`,
		snap2, OutboxOpIndex).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 1 {
		t.Fatalf("revived winner must plan exactly one index op, got %d", ops)
	}
}
