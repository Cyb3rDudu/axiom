package repair

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestRepairStateMachineIT pins the #184 state machine invariants:
//   - the scan class queues like any repairable class (#284 lifted the
//     historical "unpaginiert never queues" refusal — scan_ocr_rebuild
//     heals it; loop safety is the claim guard, not class refusal)
//   - auto-apply gate blocks below threshold / with contradictions
//     (RAG-side, never trusted from the service)
//   - the blocked transition actually lands in blocked_for_dudu (the
//     original CASE-literal bug stranded cases in in_repair forever)
//   - loop guard: the third claim of the same attachment is impossible
//   - healed closes the case; a new case after a fresh rejection is allowed
//     while attempts accumulate on the attachment
func TestRepairStateMachineIT(t *testing.T) {
	e := openStoreDB(t)
	e.truncateFixtures(t)
	ctx := context.Background()

	ch := "repair-sm-hash"
	attID, _ := e.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-1",
		docKey: "SMDOC", attKey: "SMATT", contentHash: &ch}, "completed", 1)

	// open case + scan-class queue admission (#284: the refusal is gone —
	// the pilot books carry exactly this class and must reach the fixer)
	c, _, err := e.store.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{"x":1}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", c, err)
	}
	if err := e.store.QueueRepairCase(ctx, c.ID, "🔴 unpaginiert", json.RawMessage(`{"pagination_state":"needs_ocr"}`)); err != nil {
		t.Fatalf("scan class muss queue-bar sein (#284): %v", err)
	}
	// back to rejected for the flow below (the admission probe consumed the state)
	if _, err := e.store.Pool().Exec(ctx, `UPDATE repair_cases SET status='rejected' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.QueueRepairCase(ctx, c.ID, "🔴 Unpaginiert (Mixed-Case)", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("mixed-case class string muss ebenfalls queue-bar sein (#284): %v", err)
	}
	if _, err := e.store.Pool().Exec(ctx, `UPDATE repair_cases SET status='rejected' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.QueueRepairCase(ctx, c.ID, "🔴 reparierbar", json.RawMessage(`{"folio":true}`)); err != nil {
		t.Fatalf("queue: %v", err)
	}

	// claim #1 + gate block (score below threshold)
	if _, err := e.store.ClaimRepairCase(ctx, c.ID); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	eff, err := e.store.SubmitRepairVerdict(ctx, c.ID, json.RawMessage(`{"labels":[]}`), 1, 0.94, 0, "auto_apply", "")
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if eff != RepairBlocked {
		t.Fatalf("score 0.94 muss blocken, got %s", eff)
	}
	got, _ := e.store.getRepairCase(ctx, c.ID)
	if got.Status != RepairBlocked {
		t.Fatalf("blocked muss in DB landen (war der CASE-Bug), got %s", got.Status)
	}
	if !strings.Contains(got.BlockedReason, "auto-apply-gate") {
		t.Fatalf("blocked_reason: %q", got.BlockedReason)
	}

	// contradiction path on a fresh case (closed one frees the attachment)
	ch2 := "repair-sm-hash-2"
	attID2, _ := e.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-2",
		docKey: "SMDOC2", attKey: "SMATT2", contentHash: &ch2}, "completed", 1)
	c2, _, err := e.store.CreateRepairCase(ctx, attID2, "", "reparierbar", json.RawMessage(`{}`))
	if err != nil || c2 == nil {
		t.Fatalf("CreateRepairCase 2: %v %v", c2, err)
	}
	if err := e.store.QueueRepairCase(ctx, c2.ID, "reparierbar", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ClaimRepairCase(ctx, c2.ID); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	eff, err = e.store.SubmitRepairVerdict(ctx, c2.ID, json.RawMessage(`{}`), 1, 1.0, 1, "auto_apply", "")
	if err != nil || eff != RepairBlocked {
		t.Fatalf("1 widerspruch muss blocken: %v %s", err, eff)
	}

	// gate PASS + heal on the SECOND (last allowed) attempt for attID
	// (attempt 1 burned by the gate-blocked c1; a failed attempt counts —
	// partial Zotero mutations are possible, so a free retry is not safe)
	c3, _, _ := e.store.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
	_ = e.store.QueueRepairCase(ctx, c3.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := e.store.ClaimRepairCase(ctx, c3.ID); err != nil {
		t.Fatalf("claim 2 (letzter erlaubter Versuch): %v", err)
	}
	eff, err = e.store.SubmitRepairVerdict(ctx, c3.ID, json.RawMessage(`{"labels":[[1,"1"]]}`), 1, 0.97, 0, "auto_apply", "")
	if err != nil || eff != RepairInRepair {
		t.Fatalf("0.97/0 muss auto-apply: %v %s", err, eff)
	}
	if err := e.store.MarkRepairHealed(ctx, c3.ID); err != nil {
		t.Fatalf("healed: %v", err)
	}

	// loop guard: attachment already burned both attempts (claim on c1 and
	// c3 each count — failed attempts too, since partial Zotero mutations
	// are possible) — a third claim is impossible.
	c4, _, _ := e.store.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
	_ = e.store.QueueRepairCase(ctx, c4.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := e.store.ClaimRepairCase(ctx, c4.ID); err == nil {
		t.Fatal("3. Versuch muss loop-guard auslösen")
	} else if !strings.Contains(err.Error(), "loop-guard") {
		t.Fatalf("loop-guard-fehler erwartet, got %v", err)
	}
	got4, _ := e.store.getRepairCase(ctx, c4.ID)
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
	e := openStoreDB(t)
	e.truncateFixtures(t)
	ctx := context.Background()

	seed := func(tag string) (string, *RepairCase) {
		ch := "block-" + tag
		attID, _ := e.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-" + tag,
			docKey: "BD" + tag, attKey: "BA" + tag, contentHash: &ch}, "completed", 1)
		c, _, err := e.store.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
		if err != nil || c == nil {
			t.Fatalf("CreateRepairCase %s: %v %v", tag, c, err)
		}
		return attID, c
	}

	// queued → blocked
	_, c1 := seed("q")
	if err := e.store.QueueRepairCase(ctx, c1.ID, "reparierbar", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if err := e.store.BlockRepairCase(ctx, c1.ID, "attachment-gone"); err != nil {
		t.Fatalf("BlockRepairCase (queued): %v", err)
	}
	got, _ := e.store.getRepairCase(ctx, c1.ID)
	if got.Status != RepairBlocked || got.BlockedReason != "attachment-gone" {
		t.Fatalf("queued muss blocked_for_dudu('attachment-gone') werden, got %s/%q", got.Status, got.BlockedReason)
	}

	// rejected → blocked (a never-queued case can be parked too)
	_, c2 := seed("r")
	if err := e.store.BlockRepairCase(ctx, c2.ID, "attachment-gone"); err != nil {
		t.Fatalf("BlockRepairCase (rejected): %v", err)
	}

	// in_repair refuses the block (guard holds — mid-flight case untouched)
	_, c3 := seed("i")
	_ = e.store.QueueRepairCase(ctx, c3.ID, "reparierbar", json.RawMessage(`{}`))
	if _, err := e.store.ClaimRepairCase(ctx, c3.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := e.store.BlockRepairCase(ctx, c3.ID, "attachment-gone"); err == nil {
		t.Fatal("in_repair darf nicht geblockt werden")
	}
	got3, _ := e.store.getRepairCase(ctx, c3.ID)
	if got3.Status != RepairInRepair {
		t.Fatalf("in_repair muss bleiben, got %s", got3.Status)
	}

	// blocked cases leave the queue listing (the anti-re-serve nail)
	q, err := e.store.ListRepairQueue(ctx)
	if err != nil {
		t.Fatalf("ListRepairQueue: %v", err)
	}
	for _, c := range q {
		if c.ID == c1.ID || c.ID == c2.ID {
			t.Fatalf("geblockter case %s darf nicht mehr gelistet werden", c.ID)
		}
	}
}

// TestRequeueOrphanGuardIT — #285: after an ambiguous create (Apply audited
// create_attachment_orphan — item minted in Zotero, upload failed, cleanup
// delete failed too), the requeue refuses until the operator names the
// orphan key in the ack, confirming the EMPTY item was deleted in Zotero.
// A blind re-apply would mint a second sibling attachment — the same hazard
// class the manual custody endpoint guards with its 409.
func TestRequeueOrphanGuardIT(t *testing.T) {
	e := openStoreDB(t)
	e.truncateFixtures(t)
	ctx := context.Background()

	ch := "repair-orphan-hash"
	attID, _ := e.seed(t, seedSpec{sourceBaseURL: "https://zotero.live", libraryID: "lib-o",
		docKey: "ODOC", attKey: "OATT", contentHash: &ch}, "completed", 1)
	c, _, err := e.store.CreateRepairCase(ctx, attID, "", "reparierbar", json.RawMessage(`{}`))
	if err != nil || c == nil {
		t.Fatalf("CreateRepairCase: %v %v", c, err)
	}
	if err := e.store.QueueRepairCase(ctx, c.ID, "reparierbar", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ClaimRepairCase(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	// the ambiguous create: case fails with the orphan audited
	if err := e.store.AuditWrite(ctx, c.ID, attID, "create_attachment_orphan",
		map[string]any{"new_zotero_key": "ORPHAN1", "filename": "Autor - 2020 - Titel.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.MarkRepairFailed(ctx, c.ID, "zotero create: upload 502"); err != nil {
		t.Fatal(err)
	}

	// blind requeue → refused, naming the key (no second sibling mint)
	err = e.store.RequeueRepairCaseWithOrphanAck(ctx, c.ID, "erneut versuchen", json.RawMessage(`{}`), "")
	if err == nil || !strings.Contains(err.Error(), "ORPHAN1") {
		t.Fatalf("blind requeue must refuse naming the orphan key, got %v", err)
	}
	// wrong ack → refused
	if err := e.store.RequeueRepairCaseWithOrphanAck(ctx, c.ID, "falscher Ack", json.RawMessage(`{}`), "WRONG"); err == nil {
		t.Fatal("wrong ack must refuse")
	}
	// correct ack → queued (operator confirmed the Zotero deletion)
	if err := e.store.RequeueRepairCaseWithOrphanAck(ctx, c.ID, "orphan gelöscht", json.RawMessage(`{}`), "ORPHAN1"); err != nil {
		t.Fatalf("correct ack must requeue: %v", err)
	}
	got, _ := e.store.getRepairCase(ctx, c.ID)
	if got.Status != RepairQueued {
		t.Fatalf("status must be queued, got %s", got.Status)
	}
	// resolved: a later requeue (case parked again) needs no ack
	if _, err := e.store.Pool().Exec(ctx, `UPDATE repair_cases SET status='failed' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.RequeueRepairCaseWithOrphanAck(ctx, c.ID, "nach resolve", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("resolved orphan must not block further requeues: %v", err)
	}
}
