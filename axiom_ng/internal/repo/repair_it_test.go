package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// seedRepairCase drives one attachment through create→queue→claim and
// returns the case id. attempts pre-sets the attachment's loop-guard counter
// (zotero_attachments.repair_attempts); claim=false leaves the case queued.
func seedRepairCase(t *testing.T, lr *leaseRepo, attKey string, attempts int, claim bool) string {
	t.Helper()
	ctx := context.Background()
	// unique (base_url, library_id) per fixture — the sources table has a
	// unique constraint and seed() plain-INSERTs (review B1)
	lib := "lib-" + attKey
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: lib,
		docKey: "DOC-" + attKey, attKey: attKey}, "completed", 1)
	if _, err := lr.pool.Exec(ctx, `UPDATE zotero_attachments SET repair_attempts=$2 WHERE id=$1`, attID, attempts); err != nil {
		t.Fatalf("set repair_attempts: %v", err)
	}
	c, _, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %+v", err, c)
	}
	if c.Status != RepairRejected {
		t.Fatalf("fresh case status = %s, want rejected", c.Status)
	}
	if err := lr.rep.QueueRepairCase(ctx, c.ID, "reparierbar", []byte(`{"folio":[1]}`)); err != nil {
		t.Fatalf("QueueRepairCase: %v", err)
	}
	if !claim {
		return c.ID
	}
	got, err := lr.rep.ClaimRepairCase(ctx, c.ID)
	if err != nil {
		t.Fatalf("ClaimRepairCase: %v", err)
	}
	if got.Status != RepairInRepair {
		t.Fatalf("claimed status = %s, want in_repair", got.Status)
	}
	return c.ID
}

// TestRepairLoopGuardIT pins design nail 1: two burned attempts per
// attachment block the third claim — the case flips to blocked_for_dudu
// with reason 'loop-guard' and NEVER enters in_repair (and the refused
// claim burns no attempt). Below the limit the claim succeeds and
// increments the attachment counter.
func TestRepairLoopGuardIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// attempts=2 (RepairMaxAttempts) → claim must refuse.
	caseID := seedRepairCase(t, lr, "ATT-LG", RepairMaxAttempts, false)
	_, err := lr.rep.ClaimRepairCase(ctx, caseID)
	if err == nil || !strings.Contains(err.Error(), "loop-guard") {
		t.Fatalf("third claim must hit the loop guard, got err=%v", err)
	}
	var status, reason string
	var attAttempts int
	if err := lr.pool.QueryRow(ctx,
		`SELECT status::text, blocked_reason FROM repair_cases WHERE id=$1`, caseID).
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "blocked_for_dudu" || reason != "loop-guard" {
		t.Fatalf("guard must block: got %s/%s, want blocked_for_dudu/loop-guard", status, reason)
	}
	if err := lr.pool.QueryRow(ctx,
		`SELECT repair_attempts FROM zotero_attachments WHERE id=(SELECT attachment_id FROM repair_cases WHERE id=$1)`,
		caseID).Scan(&attAttempts); err != nil {
		t.Fatal(err)
	}
	if attAttempts != RepairMaxAttempts {
		t.Fatalf("refused claim must not burn an attempt: repair_attempts=%d, want %d", attAttempts, RepairMaxAttempts)
	}

	// attempts=1 → claim succeeds and increments to 2.
	caseID2 := seedRepairCase(t, lr, "ATT-OK", 1, true)
	if err := lr.pool.QueryRow(ctx,
		`SELECT repair_attempts FROM zotero_attachments WHERE id=(SELECT attachment_id FROM repair_cases WHERE id=$1)`,
		caseID2).Scan(&attAttempts); err != nil {
		t.Fatal(err)
	}
	if attAttempts != 2 {
		t.Fatalf("successful claim must increment repair_attempts to 2, got %d", attAttempts)
	}
}

// TestRepairAutoApplyGateIT pins the gate hierarchy (enforced RAG-side, not
// trusted from the fix-service): auto_apply needs score >= 0.95 AND zero
// contradictions; anything else — including unknown verdict strings —
// blocks the case for dudu.
func TestRepairAutoApplyGateIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	cases := []struct {
		name          string
		score         float64
		contradicts   int
		verdict       string
		wantEffective RepairStatus
	}{
		{"score knapp darunter", 0.949, 0, "auto_apply", RepairBlocked},
		{"score genau Schwelle", 0.95, 0, "auto_apply", RepairInRepair},
		{"ein widerspruch", 0.99, 1, "auto_apply", RepairBlocked},
		{"unbekanntes verdict", 1.0, 0, "vielleicht", RepairBlocked},
	}
	for i, tc := range cases {
		caseID := seedRepairCase(t, lr, "ATT-GATE-"+strings.Repeat("G", i+1), 0, true)
		eff, err := lr.rep.SubmitRepairVerdict(ctx, caseID, []byte(`{"p":1}`), 1, tc.score, tc.contradicts, tc.verdict, "")
		if err != nil {
			t.Fatalf("%s: SubmitRepairVerdict: %v", tc.name, err)
		}
		if eff != tc.wantEffective {
			t.Errorf("%s: effective = %s, want %s", tc.name, eff, tc.wantEffective)
		}
		if tc.wantEffective == RepairBlocked {
			var reason string
			if err := lr.pool.QueryRow(ctx, `SELECT blocked_reason FROM repair_cases WHERE id=$1`, caseID).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason == "" {
				t.Errorf("%s: blocked case must carry a reason", tc.name)
			}
		}
	}
}

// TestRepairOneOpenCaseIT pins the partial unique index: a second
// CreateRepairCase for an attachment with an OPEN case returns the EXISTING
// case instead of inserting a row.
func TestRepairOneOpenCaseIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-guardcheck",
		docKey: "DOCREP", attKey: "ATT-ONE"}, "completed", 1)
	first, _, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil || first == nil {
		t.Fatalf("first CreateRepairCase: %v %+v", err, first)
	}
	second, _, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil {
		t.Fatalf("second CreateRepairCase: %v", err)
	}
	if second == nil {
		t.Fatal("second create must return the existing OPEN case, got (nil,nil)")
	}
	if second.ID != first.ID {
		t.Fatalf("second create must return the same case: %s vs %s", second.ID, first.ID)
	}
	var n int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM repair_cases WHERE attachment_id=$1`, attID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("one-open-case violated: %d rows for attachment %s", n, attID)
	}
}

// TestRepairVerdictRequiresInRepairIT: SubmitRepairVerdict on a case that is
// NOT in_repair must error instead of silently mutating a queued case.
func TestRepairVerdictRequiresInRepairIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	caseID := seedRepairCase(t, lr, "ATT-GUARD", 0, false) // stays queued
	if _, err := lr.rep.SubmitRepairVerdict(ctx, caseID, []byte(`{}`), 1, 0.99, 0, "auto_apply", ""); err == nil {
		t.Fatal("verdict on a queued (not in_repair) case must error")
	}
	if err := lr.rep.MarkRepairHealed(ctx, caseID); err == nil {
		t.Fatal("MarkRepairHealed on a queued case must error")
	}
}

// TestRepairCaseItemNullYearIT (#227): a document with publication_year
// NULL must not kill the case item resolution — the scan COALESCEs to 0
// and the item is served (a NULL year used to crash the queue listing and
// the invoker's RepairCaseItem).
func TestRepairCaseItemNullYearIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	caseID := seedRepairCase(t, lr, "ATT-NY", 0, true)
	var attID string
	if err := lr.pool.QueryRow(ctx,
		`SELECT attachment_id::text FROM repair_cases WHERE id=$1`, caseID).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `UPDATE zotero_documents SET publication_year = NULL`); err != nil {
		t.Fatalf("null year: %v", err)
	}
	item, err := lr.rep.RepairCaseItem(ctx, caseID)
	if err != nil {
		t.Fatalf("NULL publication_year must degrade to 0, not fail: %v", err)
	}
	if item.Year != 0 {
		t.Fatalf("year = %d, want 0 (COALESCE)", item.Year)
	}
	if item.AttachmentKey != "ATT-NY" {
		t.Fatalf("item key = %q", item.AttachmentKey)
	}
	_ = attID
}

// TestRepairRequeueIT pins the #278 loop-guard reset route: a parked case
// (blocked_for_dudu/loop-guard or failed) is re-armed WITHOUT DB surgery —
// repair_attempts → 0, case → queued, reason audited. The requeue refuses
// empty reasons (the changed evidence conditions must be documented) and
// refuses non-parked states (mid-flight/healed are never touched).
func TestRepairRequeueIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// Guard-parked case (the production shape: attempts exhausted).
	caseID := seedRepairCase(t, lr, "ATT-RQ", RepairMaxAttempts, false)
	if _, err := lr.rep.ClaimRepairCase(ctx, caseID); err == nil {
		t.Fatal("claim must hit the loop guard first")
	}

	// Empty reason is refused — changed evidence conditions are documented.
	if err := lr.rep.RequeueRepairCase(ctx, caseID, "  ", nil); err == nil {
		t.Fatal("requeue without reason must be refused")
	}

	// Requeue re-arms: guard counter 0, case queued.
	if err := lr.rep.RequeueRepairCase(ctx, caseID,
		"#278 forensics fix — Karte vollständig, retry lohnt", nil); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	var status, reason string
	var attAttempts int
	if err := lr.pool.QueryRow(ctx,
		`SELECT status::text, COALESCE(blocked_reason,'') FROM repair_cases WHERE id=$1`, caseID).
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || reason != "" {
		t.Fatalf("requeue must clear the park: got %s/%q, want queued/\"\"", status, reason)
	}
	if err := lr.pool.QueryRow(ctx,
		`SELECT repair_attempts FROM zotero_attachments WHERE id=(SELECT attachment_id FROM repair_cases WHERE id=$1)`,
		caseID).Scan(&attAttempts); err != nil {
		t.Fatal(err)
	}
	if attAttempts != 0 {
		t.Fatalf("requeue must reset the loop guard, repair_attempts=%d", attAttempts)
	}
	// The previously impossible claim now works and burns exactly one attempt.
	got, err := lr.rep.ClaimRepairCase(ctx, caseID)
	if err != nil || got.Status != RepairInRepair {
		t.Fatalf("claim after requeue: err=%v status=%s", err, got.Status)
	}

	// Audit trail carries the requeue with its reason.
	var n int
	if err := lr.pool.QueryRow(ctx,
		`SELECT count(*) FROM zotero_write_audit WHERE case_id=$1 AND action='repair-requeue'`,
		caseID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("requeue must write exactly one audit row, got %d", n)
	}

	// in_repair (mid-flight) refuses — the same nail as BlockRepairCase.
	if err := lr.rep.RequeueRepairCase(ctx, caseID, "nochmal", nil); err == nil {
		t.Fatal("requeue must refuse in_repair")
	}

	// #284 review: the analysis_patch surface — a parked case re-armed with
	// the OCR override carries it in analysis (the invoker keys budget and
	// --ocr-mode on exactly these fields), and the patch is audited.
	caseID2 := seedRepairCase(t, lr, "ATT-RQ2", 0, false)
	if _, err := lr.pool.Exec(ctx,
		`UPDATE repair_cases SET status='failed', blocked_reason='no-healable-defect-evidenced: IT' WHERE id=$1`, caseID2); err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.RequeueRepairCase(ctx, caseID2, "force-Modus für kaputte Textschicht (#284)",
		json.RawMessage(`{"ocr": {"mode": "force", "lang": "eng"}}`)); err != nil {
		t.Fatalf("requeue with patch: %v", err)
	}
	var analysis string
	if err := lr.pool.QueryRow(ctx, `SELECT analysis::text FROM repair_cases WHERE id=$1`, caseID2).Scan(&analysis); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(analysis, `"mode"`) || !strings.Contains(analysis, `"force"`) {
		t.Fatalf("analysis_patch must merge into the case analysis, got %s", analysis)
	}

	// #284: a REJECTED manual-track case is re-armable too (the operator
	// path for a broken-text-layer case that never auto-queued).
	caseID3 := seedRepairCase(t, lr, "ATT-RQ3", 0, false)
	if _, err := lr.pool.Exec(ctx, `UPDATE repair_cases SET status='rejected' WHERE id=$1`, caseID3); err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.RequeueRepairCase(ctx, caseID3, "manuell gereiht", json.RawMessage(`{"ocr":{"mode":"force"}}`)); err != nil {
		t.Fatalf("requeue from rejected must work since #284: %v", err)
	}
	if err := lr.rep.RequeueRepairCase(ctx, caseID3, "nochmal", nil); err == nil {
		t.Fatal("requeue from queued (not parked) must refuse")
	}
}
