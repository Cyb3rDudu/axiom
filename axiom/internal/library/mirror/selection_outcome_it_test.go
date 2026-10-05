// #252/#356/#358 outcome-truth IT: the merged listing behind
// ZoteroDocumentState.Outcome — quality_state.pagination_state from the
// newest job (Store), the newest repair case (Library), the selection
// gate (Library) and the active-snapshot truth (Store) all flow through
// ListDocumentsMirror + DocumentListing into the derived per-document
// outcome. (The DeriveOutcome switch itself is unit-pinned in
// selection_outcome_test.go.)
package mirror

import (
	"context"
	"strings"
	"testing"
)

func TestOutcomeProjectionIT(t *testing.T) {
	lr := openMirrorDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "hash-252"
	attID, jobID := lr.seed(t, mirrorSeedSpec{sourceBaseURL: "https://zotero.252", libraryID: "lib-1",
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

	listing := func() ZoteroDocumentState {
		t.Helper()
		rows, err := lr.rep.ListDocumentsMirror(ctx)
		if err != nil {
			t.Fatalf("mirror listing: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("want 1 document, got %d", len(rows))
		}
		var attIDs, docIDs []string
		for _, z := range rows {
			attIDs = append(attIDs, z.AttachmentID)
			docIDs = append(docIDs, z.DocumentID)
		}
		jobs, serving, err := lr.store.DocumentJobStates(ctx, attIDs, docIDs)
		if err != nil {
			t.Fatalf("store job states: %v", err)
		}
		docs := DocumentListing(rows, jobs, serving, "")
		if len(docs) != 1 {
			t.Fatalf("want 1 merged document, got %d", len(docs))
		}
		return docs[0]
	}

	d := listing()
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
	d = listing()
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
	d = listing()
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
	d = listing()
	if d.Outcome != "excluded" || d.OutcomeReason != "selection-excluded" {
		t.Fatalf("outcome = %q (%q), want excluded (selection-excluded)", d.Outcome, d.OutcomeReason)
	}
}

// TestOutcomeServingCohortIT — the #356 acceptance probe on the MERGED
// listing: administrative closures (cancelled waves, cleanup failures,
// skips) over documents with a SURVIVING active snapshot derive
// `serving` with the closure named; the same closures WITHOUT a snapshot
// stay `failed`. The 126-document production cohort is the first shape;
// the second is the synthetic no-snapshot probe from the issue.
func TestOutcomeServingCohortIT(t *testing.T) {
	lr := openMirrorDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// Two documents: one with an active snapshot (the cohort shape), one
	// without (the honest failure).
	year := 2026
	_ = year
	seedDoc := func(key, jobStatus, errCode, errMsg string, withSnapshot bool) (docID, attID, jobID string) {
		attID, jobID = lr.seed(t, mirrorSeedSpec{
			sourceBaseURL: "https://zotero.356/" + key, libraryID: "lib-" + key,
			docKey: key, attKey: key + "ATT", preferred: true,
			contentHash: strPtr("sha256:" + key),
		}, jobStatus, 3)
		if err := lr.pool.QueryRow(ctx,
			`SELECT document_id::text FROM store_documents WHERE attachment_id=$1::uuid`, attID).Scan(&docID); err != nil {
			t.Fatal(err)
		}
		if withSnapshot {
			if _, err := lr.pool.Exec(ctx, `
				INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name,
					processor_version, profile_hash, document_id, profile, active)
				VALUES ($1::uuid, $2, 'p', 'v', 'ph', $3::uuid, '{}', true)`, attID, "sha256:"+key, docID); err != nil {
				t.Fatal(err)
			}
		}
		if errCode != "" {
			if _, err := lr.pool.Exec(ctx,
				`UPDATE ingest_jobs SET error_code=$2, error_message=$3 WHERE id=$1`, jobID, errCode, errMsg); err != nil {
				t.Fatal(err)
			}
		}
		return docID, attID, jobID
	}

	_, _, _ = seedDoc("CANCELWAVE", "cancelled", "", "", true)                                      // 91-cohort shape
	_, _, _ = seedDoc("NIGHTCLEAN", "failed", "NIGHT_CLEANUP", "Redundant (Snapshot exists)", true) // 34-cohort shape
	_, _, _ = seedDoc("REALFAIL", "failed", "RETRY_EXHAUSTED", "lease expired", false)              // honest failure

	merged := func() map[string]ZoteroDocumentState {
		rows, err := lr.rep.ListDocumentsMirror(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var attIDs, docIDs []string
		for _, z := range rows {
			if z.AttachmentID != "" {
				attIDs = append(attIDs, z.AttachmentID)
			}
			docIDs = append(docIDs, z.DocumentID)
		}
		jobs, serving, err := lr.store.DocumentJobStates(ctx, attIDs, docIDs)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]ZoteroDocumentState{}
		for _, z := range DocumentListing(rows, jobs, serving, "") {
			out[z.ZoteroKey] = z
		}
		return out
	}()

	if z := merged["CANCELWAVE"]; z.Outcome != "serving" {
		t.Fatalf("cancelled-with-snapshot must derive serving, got %q (%q) — #356", z.Outcome, z.OutcomeReason)
	}
	if z := merged["NIGHTCLEAN"]; z.Outcome != "serving" {
		t.Fatalf("cleanup-closure-with-snapshot must derive serving, got %q (%q) — #356", z.Outcome, z.OutcomeReason)
	}
	if z := merged["NIGHTCLEAN"]; z.OutcomeReason == "" || !contains(z.OutcomeReason, "NIGHT_CLEANUP") {
		t.Fatalf("the serving reason must name the closure truth, got %q", z.OutcomeReason)
	}
	if z := merged["REALFAIL"]; z.Outcome != "failed" {
		t.Fatalf("no-snapshot failure must STAY failed, got %q (%q) — the honest failure probe", z.Outcome, z.OutcomeReason)
	}
	// The cohort's coarse state: served documents are synced, not held.
	if z := merged["CANCELWAVE"]; z.SyncState != "synced" {
		t.Fatalf("served cancelled doc must list synced, got %q", z.SyncState)
	}
}

func strPtr(s string) *string { return &s }

func contains(s, sub string) bool { return strings.Contains(s, sub) }
