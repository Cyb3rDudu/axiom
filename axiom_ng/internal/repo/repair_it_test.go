package repo

import (
	"context"
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
	c, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
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
	first, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
	if err != nil || first == nil {
		t.Fatalf("first CreateRepairCase: %v %+v", err, first)
	}
	second, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", []byte(`{}`))
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
