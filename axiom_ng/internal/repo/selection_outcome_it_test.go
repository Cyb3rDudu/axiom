// #252 outcome-truth IT: the SQL wiring behind ZoteroDocumentState.Outcome —
// quality_state.pagination_state from the newest job, the newest repair case,
// and the selection gate all flow through ListZoteroDocuments into the derived
// per-document outcome. (The DeriveOutcome switch itself is unit-pinned in
// selection_outcome_test.go.)
package repo

import (
	"context"
	"testing"
)

func TestOutcomeProjectionIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-252"
	attID, jobID := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.252", libraryID: "lib-1",
		docKey: "OUTC1", attKey: "OUTATT1", contentHash: &ch, preferred: true}, "skipped", 3)

	// The unpaginiert dead end: job skipped with the #254 pagination_state in
	// its quality_state, plus the rejected repair case the dispatcher opens.
	if _, err := lr.pool.Exec(ctx, `
		UPDATE ingest_jobs SET error_code='SKIPPED', error_message='preflight:unpaginiert',
			quality_state='{"pagination_state":"needs_ocr"}' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class)
		VALUES ($1, (SELECT document_id FROM zotero_attachments WHERE id=$1), 'rejected', '🔴 unpaginiert')`,
		attID); err != nil {
		t.Fatal(err)
	}

	find := func() ZoteroDocumentState {
		t.Helper()
		docs, err := lr.rep.ListZoteroDocuments(ctx, "")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(docs) != 1 {
			t.Fatalf("want 1 document, got %d", len(docs))
		}
		return docs[0]
	}

	d := find()
	if d.Outcome != "needs_ocr" {
		t.Fatalf("outcome = %q, want needs_ocr (pagination_state must beat the rejected repair case)", d.Outcome)
	}
	if d.RepairStatus != "rejected" {
		t.Fatalf("repair_status = %q, want rejected (live from repair_cases)", d.RepairStatus)
	}

	// Live flip: case queued by the fix-service track → in_repair wins as
	// soon as the dead end is gone (repaired pagination_state absent).
	if _, err := lr.pool.Exec(ctx, `
		UPDATE ingest_jobs SET quality_state='{}' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE repair_cases SET status='in_repair' WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	d = find()
	if d.Outcome != "in_repair" || d.OutcomeReason != "repair case: in_repair" {
		t.Fatalf("outcome = %q (%q), want in_repair (repair case: in_repair)", d.Outcome, d.OutcomeReason)
	}

	// Terminal: repair healed, doc reprocessed → completed.
	if _, err := lr.pool.Exec(ctx, `UPDATE repair_cases SET status='healed' WHERE attachment_id=$1`, attID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE ingest_jobs SET status='completed' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	d = find()
	if d.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", d.Outcome)
	}

	// Selection gate: an excluded row beats even the completed job — the
	// client deliberately held this doc, that is the live truth.
	if _, err := lr.pool.Exec(ctx, `
		INSERT INTO zotero_selections (document_id, mode)
		VALUES ((SELECT document_id FROM zotero_attachments WHERE id=$1), 'excluded')`, attID); err != nil {
		t.Fatal(err)
	}
	d = find()
	if d.Outcome != "excluded" || d.OutcomeReason != "selection-excluded" {
		t.Fatalf("outcome = %q (%q), want excluded (selection-excluded)", d.Outcome, d.OutcomeReason)
	}
}
