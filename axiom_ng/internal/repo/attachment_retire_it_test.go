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
	if err := retireDeletedAttachmentsTx(ctx, tx); err != nil {
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
	if err := retireDeletedAttachmentsTx(ctx, tx); err != nil {
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
