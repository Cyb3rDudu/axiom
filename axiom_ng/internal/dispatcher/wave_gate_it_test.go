package dispatcher

// #282 wave-gate integration tests (real DB, same proviso as the dispatcher
// suite): the repair-included wave semantics — dry-run → repair if needed →
// sync → NEXT document; skip only when unrepairable. These pin the claim
// gate: while a repair loop-back drains (queued/in_repair, or healed-but-
// not-yet-enqueued), no new document is claimed. Terminal parks never gate.

import (
	"context"
	"testing"
	"time"
)

// seedRepairCaseForJob opens a repair case for the given job's attachment in
// the requested status (updated_at=now).
func (h *dispatchHarness) seedRepairCaseForJob(t *testing.T, jobID, status string) string {
	t.Helper()
	ctx := context.Background()
	var attID string
	if err := h.pool.QueryRow(ctx,
		`SELECT attachment_id::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	var caseID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class, analysis)
		SELECT a.id, a.document_id, $2::repair_status, '🔴 reparierbar', '{}'
		FROM ingest_jobs j JOIN zotero_attachments a ON a.id = j.attachment_id
		WHERE j.id = $1
		RETURNING id::text`, jobID, status).Scan(&caseID); err != nil {
		t.Fatal(err)
	}
	return caseID
}

// enqueueJobForDocument inserts a pending job for the document of the given
// case's attachment (the shape the post-heal sync produces).
func (h *dispatchHarness) enqueueJobForDocument(t *testing.T, caseID string) {
	t.Helper()
	ctx := context.Background()
	tag, err := h.pool.Exec(ctx, `
		INSERT INTO ingest_jobs (status, attachment_id, content_hash)
		SELECT 'pending', a.id, 'healed-hash'
		FROM repair_cases c JOIN zotero_attachments a ON a.id = c.attachment_id
		WHERE c.id = $1`, caseID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("enqueue for document: %d rows", tag.RowsAffected())
	}
}

// Wave ordering pin (#282 DoD): while a repair case is open (queued — the
// fixer heal is pending), the next document is NOT claimed. Once the case
// is terminal (unrepairable park), the wave continues — the skip is
// documented in the case, never silent.
func TestWaveGateDefersClaimWhileRepairOpen(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "W1", 3)
	// a job on a SECOND, unrelated document: the gate is INSTANCE-wide by
	// design (the owner-specified wave semantics serialize the whole
	// queue, not per document) — pinning the scope so a per-document
	// narrowing cannot pass silently.
	otherJob := h.seedJob(t, "W1B", 3)

	// an OPEN repair case (queued heal) holds the gate
	h.seedRepairCaseForJob(t, jobID, "queued")

	fp := newFakeProcessor(t)
	fp.statuses = []string{"completed"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`
	d := newDispatcher(t, h, fp, Config{})
	runFor(t, d, context.Background(), 700*time.Millisecond)

	if got := h.jobStatus(t, jobID); got != "pending" {
		t.Fatalf("status = %q, want pending — the wave gate must defer the claim while a heal is queued (#282)", got)
	}
	if got := h.jobStatus(t, otherJob); got != "pending" {
		t.Fatalf("second document's status = %q, want pending — the gate scope is instance-wide (owner semantics)", got)
	}
	held, reason, err := h.rep.WaveRepairGate(context.Background())
	if err != nil || !held {
		t.Fatalf("WaveRepairGate = %v/%q err=%v, want held", held, reason, err)
	}

	// terminal park (unrepairable): gate opens, the wave moves on.
	// #298: Run is single-shot (ready/stopped close exactly once), so the
	// post-release claim runs on a FRESH dispatcher — same harness and fake
	// processor, same claim+completion proof.
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE repair_cases SET status='failed', blocked_reason='no-healable-defect-evidenced: IT' WHERE status='queued'`); err != nil {
		t.Fatal(err)
	}
	d2 := newDispatcher(t, h, fp, Config{})
	runFor(t, d2, context.Background(), 3*time.Second)
	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed after the terminal park released the gate", got)
	}
}

// The post-heal window pin (#282 DoD): a HEALED case whose sync has not
// enqueued a job yet holds the gate; the enqueue (what the post-heal sync
// does) releases it.
func TestWaveGateHoldsUntilHealedEnqueued(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "W2", 3)
	caseID := h.seedRepairCaseForJob(t, jobID, "healed")

	held, reason, err := h.rep.WaveRepairGate(context.Background())
	if err != nil || !held {
		t.Fatalf("WaveRepairGate = %v/%q err=%v, want held for healed-not-enqueued", held, reason, err)
	}

	// the post-heal sync's effect: a job enqueued for the document after
	// the heal timestamp releases the gate
	h.enqueueJobForDocument(t, caseID)
	held, _, err = h.rep.WaveRepairGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("gate must release once the healed document has a newer-enqueued job")
	}
}

// The stranded-heal window is BOUNDED (#282): a healed case whose sync
// never landed stops gating after the window — a dead invoker mid-sync must
// not stall the wave forever (the strand stays operator-visible in the
// invoker log, not as a silent claim freeze).
func TestWaveGateStrandedHealWindowExpires(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "W3", 3)
	h.seedRepairCaseForJob(t, jobID, "healed")
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE repair_cases SET updated_at = now() - interval '2 hours' WHERE status='healed'`); err != nil {
		t.Fatal(err)
	}
	held, _, err := h.rep.WaveRepairGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("gate must not hold on a heal older than the window (bounded strand)")
	}
}

// Terminal parks never gate (#282 DoD): failed + blocked_for_dudu cases
// (genuinely unrepairable) do not block the wave and produce no sync loop.
func TestWaveGateTerminalParksNeverHold(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "W4", 3)
	h.seedRepairCaseForJob(t, jobID, "failed")
	jobID2 := h.seedJob(t, "W5", 3)
	h.seedRepairCaseForJob(t, jobID2, "blocked_for_dudu")
	// rejected (manual track) also never gates
	jobID3 := h.seedJob(t, "W6", 3)
	h.seedRepairCaseForJob(t, jobID3, "rejected")

	held, reason, err := h.rep.WaveRepairGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatalf("terminal/manual cases must never gate, got held (%s)", reason)
	}
}

// The document-level loop binding (#282): every heal REPLACES the
// attachment (fresh per-attachment repair_attempts), so the
// heal → sync → re-reject → heal cycle across generations is bounded by the
// DOCUMENT's healed count. Beyond RepairMaxAttempts healed cases, a fresh
// preflight reject does not auto-queue — the case stays rejected for the
// operator. This is the mutation probe for the sync-storm DoD: without the
// guard, the healed-skip-heal cycle would run unbounded.
func TestPreflightAutoQueueDocLevelLoopGuard(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "W7", 3)

	// two prior HEALED cases on the document (attachment generations 1+2),
	// OUTSIDE the stranded-heal window so the wave gate itself stays open —
	// this test pins the auto-queue guard, not the gate
	var attID, docID string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT attachment_id::text, document_id::text FROM ingest_jobs WHERE id=$1`, jobID).
		Scan(&attID, &docID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.pool.Exec(context.Background(), `
			INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class, analysis, updated_at)
			VALUES ($1::uuid, $2::uuid, 'healed', '🔴 reparierbar', '{}', now() - interval '2 hours')`, attID, docID); err != nil {
			t.Fatal(err)
		}
	}

	fp := newFakeProcessor(t)
	fp.preflightReport = &map[string]any{
		"ok":      false,
		"finding": "🔴 reparierbar",
		"reason":  "kein Tier-1",
		"details": map[string]any{"pages": 1, "text_layer": true},
	}
	d := newDispatcher(t, h, fp, Config{PreflightEnabled: true})
	runFor(t, d, context.Background(), 3*time.Second)

	if got := h.jobStatus(t, jobID); got != "skipped" {
		t.Fatalf("status = %q, want skipped (preflight red)", got)
	}
	// the fresh case must exist …
	var n int
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(max(status::text),'') FROM repair_cases
		 WHERE attachment_id=$1 AND status IN ('rejected','queued','in_repair')`, attID).Scan(&n, &status); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("open cases = %d, want 1 (fresh case created)", n)
	}
	// … but stay REJECTED — the loop guard refused the auto-queue
	if status != "rejected" {
		t.Fatalf("fresh case status = %q, want rejected (document already healed 2×, #282 loop guard)", status)
	}
}
