package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRepairStateMachineIT pins the #184 state machine invariants:
//   - unpaginiert never queues (design nail 1)
//   - auto-apply gate blocks below threshold / with contradictions
//     (RAG-side, never trusted from the service)
//   - the blocked transition actually lands in blocked_for_dudu (the
//     original CASE-literal bug stranded cases in in_repair forever)
//   - loop guard: the third claim of the same attachment is impossible
//   - healed closes the case; a new case after a fresh rejection is allowed
//     while attempts accumulate on the attachment
func TestRepairStateMachineIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	ch := "repair-sm-hash"
	attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-1",
		docKey: "SMDOC", attKey: "SMATT", contentHash: &ch}, "completed", 1)

	// open case + unpaginiert gate
	c, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{"x":1}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", c, err)
	}
	if err := lr.rep.QueueRepairCase(ctx, c.ID, "🔴 unpaginiert", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unpaginiert muss QueueRepairCase verweigern")
	}
	if err := lr.rep.QueueRepairCase(ctx, c.ID, "🔴 Unpaginiert (Mixed-Case)", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unpaginiert (mixed case) muss QueueRepairCase verweigern — ToLower-Guard")
	}
	if err := lr.rep.QueueRepairCase(ctx, c.ID, "🔴 reparierbar", json.RawMessage(`{"folio":true}`)); err != nil {
		t.Fatalf("queue: %v", err)
	}

	// claim #1 + gate block (score below threshold)
	if _, err := lr.rep.ClaimRepairCase(ctx, c.ID); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	eff, err := lr.rep.SubmitRepairVerdict(ctx, c.ID, json.RawMessage(`{"labels":[]}`), 1, 0.94, 0, "auto_apply", "")
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if eff != RepairBlocked {
		t.Fatalf("score 0.94 muss blocken, got %s", eff)
	}
	got, _ := lr.rep.getRepairCase(ctx, c.ID)
	if got.Status != RepairBlocked {
		t.Fatalf("blocked muss in DB landen (war der CASE-Bug), got %s", got.Status)
	}
	if !strings.Contains(got.BlockedReason, "auto-apply-gate") {
		t.Fatalf("blocked_reason: %q", got.BlockedReason)
	}

	// contradiction path on a fresh case (closed one frees the attachment)
	ch2 := "repair-sm-hash-2"
	attID2, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-2",
		docKey: "SMDOC2", attKey: "SMATT2", contentHash: &ch2}, "completed", 1)
	c2, err := lr.rep.CreateRepairCase(ctx, attID2, "", "reparierbar", json.RawMessage(`{}`))
	if err != nil || c2 == nil {
		t.Fatalf("CreateRepairCase 2: %v %v", c2, err)
	}
	if err := lr.rep.QueueRepairCase(ctx, c2.ID, "reparierbar", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.rep.ClaimRepairCase(ctx, c2.ID); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	eff, err = lr.rep.SubmitRepairVerdict(ctx, c2.ID, json.RawMessage(`{}`), 1, 1.0, 1, "auto_apply", "")
	if err != nil || eff != RepairBlocked {
		t.Fatalf("1 widerspruch muss blocken: %v %s", err, eff)
	}

	// gate PASS + heal on the SECOND (last allowed) attempt for attID
	// (attempt 1 burned by the gate-blocked c1; a failed attempt counts —
	// partial Zotero mutations are possible, so a free retry is not safe)
	c3, _ := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
	_ = lr.rep.QueueRepairCase(ctx, c3.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := lr.rep.ClaimRepairCase(ctx, c3.ID); err != nil {
		t.Fatalf("claim 2 (letzter erlaubter Versuch): %v", err)
	}
	eff, err = lr.rep.SubmitRepairVerdict(ctx, c3.ID, json.RawMessage(`{"labels":[[1,"1"]]}`), 1, 0.97, 0, "auto_apply", "")
	if err != nil || eff != RepairInRepair {
		t.Fatalf("0.97/0 muss auto-apply: %v %s", err, eff)
	}
	if err := lr.rep.MarkRepairHealed(ctx, c3.ID); err != nil {
		t.Fatalf("healed: %v", err)
	}

	// loop guard: attachment already burned both attempts (claim on c1 and
	// c3 each count — failed attempts too, since partial Zotero mutations
	// are possible) — a third claim is impossible.
	c4, _ := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
	_ = lr.rep.QueueRepairCase(ctx, c4.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := lr.rep.ClaimRepairCase(ctx, c4.ID); err == nil {
		t.Fatal("3. Versuch muss loop-guard auslösen")
	} else if !strings.Contains(err.Error(), "loop-guard") {
		t.Fatalf("loop-guard-fehler erwartet, got %v", err)
	}
	got4, _ := lr.rep.getRepairCase(ctx, c4.ID)
	if got4.Status != RepairBlocked || got4.BlockedReason != "loop-guard" {
		t.Fatalf("loop-guard muss blocked_for_dudu setzen, got %s/%q", got4.Status, got4.BlockedReason)
	}
}

// TestBlockRepairCaseIT (follow-up W1b): BlockRepairCase parks
// queued AND rejected cases as blocked_for_dudu with the given reason
// (queue listing uses it for gone attachments — a silent skip would
// re-serve the case forever), while in_repair refuses the block so a
// mid-flight case can never be parked out from under the fix-service.
func TestBlockRepairCaseIT(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	seed := func(tag string) (string, *RepairCase) {
		ch := "block-" + tag
		attID, _ := lr.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-" + tag,
			docKey: "BD" + tag, attKey: "BA" + tag, contentHash: &ch}, "completed", 1)
		c, err := lr.rep.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
		if err != nil || c == nil {
			t.Fatalf("CreateRepairCase %s: %v %v", tag, c, err)
		}
		return attID, c
	}

	// queued → blocked
	_, c1 := seed("q")
	if err := lr.rep.QueueRepairCase(ctx, c1.ID, "reparierbar", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := lr.rep.BlockRepairCase(ctx, c1.ID, "attachment-gone"); err != nil {
		t.Fatalf("BlockRepairCase (queued): %v", err)
	}
	got, _ := lr.rep.getRepairCase(ctx, c1.ID)
	if got.Status != RepairBlocked || got.BlockedReason != "attachment-gone" {
		t.Fatalf("queued muss blocked_for_dudu('attachment-gone') werden, got %s/%q", got.Status, got.BlockedReason)
	}

	// rejected → blocked (a never-queued case can be parked too)
	_, c2 := seed("r")
	if err := lr.rep.BlockRepairCase(ctx, c2.ID, "attachment-gone"); err != nil {
		t.Fatalf("BlockRepairCase (rejected): %v", err)
	}

	// in_repair refuses the block (guard holds — mid-flight case untouched)
	_, c3 := seed("i")
	_ = lr.rep.QueueRepairCase(ctx, c3.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := lr.rep.ClaimRepairCase(ctx, c3.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := lr.rep.BlockRepairCase(ctx, c3.ID, "attachment-gone"); err == nil {
		t.Fatal("in_repair darf nicht geblockt werden")
	}
	got3, _ := lr.rep.getRepairCase(ctx, c3.ID)
	if got3.Status != RepairInRepair {
		t.Fatalf("in_repair muss bleiben, got %s", got3.Status)
	}

	// blocked cases leave the queue listing (the anti-re-serve nail)
	q, err := lr.rep.ListRepairQueue(ctx)
	if err != nil {
		t.Fatalf("ListRepairQueue: %v", err)
	}
	for _, c := range q {
		if c.ID == c1.ID || c.ID == c2.ID {
			t.Fatalf("geblockter case %s darf nicht mehr gelistet werden", c.ID)
		}
	}
}
