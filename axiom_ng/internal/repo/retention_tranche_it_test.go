package repo

// #290 tranche ITs (real DB): the apply must delete in TRANCHES — one
// committed transaction per tranche, progress callback per tranche — and
// an interruption at a tranche boundary must leave committed partial
// progress that a re-run resumes (idempotence of the candidate guards).
//
// Mutation pins (part of the issue's acceptance):
//   - revert the per-tranche commit (single big transaction again) →
//     TestRetentionTrancheIT's callback sequence collapses (one callback
//     per phase instead of one per tranche) and the interrupt test finds
//     either everything or nothing persisted → RED;
//   - drop the progress callback invocation → both tests' sequence
//     assertions fail → RED.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// waitForLockWaiter polls until some OTHER backend is blocked on a row
// lock with a query matching ANY of the patterns — the retention tranche
// blocked either in its locking SELECT (current shape) or in its DELETE
// (a mutant without FOR UPDATE) — bounded by maxWait.
func (e *retEnv) waitForLockWaiter(t *testing.T, maxWait time.Duration, patterns ...string) bool {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		var n int
		if err := e.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock'
			  AND datname = current_database()
			  AND (query LIKE $1 OR query LIKE $2)`, patterns[0], patterns[len(patterns)-1]).Scan(&n); err == nil && n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestRetentionGuardRecheckIT (#290 review blocker pin): the keep-guards
// must live INSIDE the tranche DELETE, not only in the SELECT. Choreography:
// the test holds a row lock on the OLDER of two candidates; the apply's
// tranche SELECT still sees it as a candidate (plain reads don't block),
// its DELETE blocks on the lock; the test then flips the row's guard
// input (active=true, the snapshot-persist replay path) and commits; the
// DELETE wakes and Postgres re-checks the qual against the updated row
// (EvalPlanQual) — the reactivated snapshot must SURVIVE, the run must
// continue with the still-qualified candidate. Red probe: drop the guard
// from the DELETE (id = ANY(...) only) → the reactivated snapshot is
// deleted → "must survive" fails. Same for the job leg via status flip.
func TestRetentionGuardRecheckIT(t *testing.T) {
	e := openRetDB(t)
	ctx := context.Background()
	age := 14 * 24 * time.Hour

	// ── snapshot leg: reactivated snapshot survives ──────────────────
	e.seedTrancheFixture(t, 0, 0)                     // fixture doc/attachment + outcome job
	victim := e.seedSnapshot(t, false, nil, 1, false) // older: first pick
	time.Sleep(10 * time.Millisecond)                 // strict created_at order
	other := e.seedSnapshot(t, false, nil, 1, false)  // younger: second pick
	_ = other

	lockTx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(ctx)
	if _, err := lockTx.Exec(ctx, `SELECT id FROM processing_snapshots WHERE id=$1::uuid FOR UPDATE`, victim); err != nil {
		t.Fatal(err)
	}
	type res struct {
		err error
	}
	done := make(chan res, 1)
	go func() {
		_, _, err := e.rep.ApplyRetention(ctx, age, 1, nil)
		done <- res{err}
	}()
	if !e.waitForLockWaiter(t, 5*time.Second, "%processing_snapshots%FOR UPDATE%", "%DELETE FROM processing_snapshots%") {
		t.Fatal("tranche never blocked on the victim row lock — choreography broken")
	}
	// flip the guard input WHILE the DELETE waits, then release the lock
	if _, err := lockTx.Exec(ctx, `UPDATE processing_snapshots SET active=true WHERE id=$1::uuid`, victim); err != nil {
		t.Fatal(err)
	}
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("apply with a concurrently reactivated candidate must succeed, got %v", r.err)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots WHERE id=$1::uuid AND active`, victim); n != 1 {
		t.Fatalf("reactivated snapshot was deleted by the tranche — the keep-guard left the DELETE (TOCTOU)")
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots WHERE id=$1::uuid`, other); n != 0 {
		t.Fatalf("still-qualified candidate must be removed in the same run, got %d", n)
	}

	// ── job leg: pending-flipped job survives ─────────────────────
	e.seedJobForAttachment(t, "TRTATT", "completed", time.Hour, 0) // outcome truth + sibling
	victimJob := e.seedJobForAttachment(t, "TRTATT", "failed", 20*24*time.Hour, 20*24*time.Hour)
	time.Sleep(10 * time.Millisecond)
	otherJob := e.seedJobForAttachment(t, "TRTATT", "failed", 20*24*time.Hour, 20*24*time.Hour)
	_ = otherJob

	jlockTx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer jlockTx.Rollback(ctx)
	if _, err := jlockTx.Exec(ctx, `SELECT id FROM ingest_jobs WHERE id=$1::uuid FOR UPDATE`, victimJob); err != nil {
		t.Fatal(err)
	}
	jdone := make(chan res, 1)
	go func() {
		_, _, err := e.rep.ApplyRetention(ctx, age, 1, nil)
		jdone <- res{err}
	}()
	if !e.waitForLockWaiter(t, 5*time.Second, "%FROM ingest_jobs%FOR UPDATE%", "%DELETE FROM ingest_jobs%") {
		t.Fatal("job tranche never blocked on the victim row lock — choreography broken")
	}
	if _, err := jlockTx.Exec(ctx, `UPDATE ingest_jobs SET status='pending' WHERE id=$1::uuid`, victimJob); err != nil {
		t.Fatal(err)
	}
	if err := jlockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	jr := <-jdone
	if jr.err != nil {
		t.Fatalf("apply with a concurrently re-queued candidate must succeed, got %v", jr.err)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, victimJob); n != 1 {
		t.Fatalf("pending-flipped job was deleted by the tranche — the keep-guard left the DELETE (TOCTOU)")
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, otherJob); n != 0 {
		t.Fatalf("still-qualified job must be removed in the same run, got %d", n)
	}
}

// seedTrancheFixture seeds one preferred attachment with a young completed
// outcome-truth job plus nStale prunable terminal attempts (newer sibling
// exists, not the preferred-newest, no repair link, no snapshot), and
// nSnaps superseded outbox-free snapshots. Returns nothing — every
// assertion goes through counts.
func (e *retEnv) seedTrancheFixture(t *testing.T, nStale, nSnaps int) {
	t.Helper()
	e.seedDocWithAttachment(t, "TRTDOC", "TRTATT")
	e.seedJobForAttachment(t, "TRTATT", "completed", time.Hour, 0) // outcome truth + newer sibling
	for i := 0; i < nStale; i++ {
		e.seedJobForAttachment(t, "TRTATT", "failed", 20*24*time.Hour, 20*24*time.Hour)
	}
	for i := 0; i < nSnaps; i++ {
		e.seedSnapshot(t, false, nil, 1, false)
	}
}

// TestRetentionTrancheIT: with batch=1, three deletable snapshots and three
// deletable jobs must produce SIX progress callbacks — one per COMMITTED
// tranche, each advancing done by exactly the batch size — and the final
// state must be fully removed. The callback sequence is the commit pin: a
// revert to one monolithic transaction fires one callback per phase.
func TestRetentionTrancheIT(t *testing.T) {
	e := openRetDB(t)
	ctx := context.Background()
	e.seedTrancheFixture(t, 3, 3)
	age := 14 * 24 * time.Hour

	plan, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshots.Remove != 3 || plan.Jobs.Remove != 3 {
		t.Fatalf("fixture must offer 3/3 removals, got snapshots=%d jobs=%d", plan.Snapshots.Remove, plan.Jobs.Remove)
	}

	var calls []string
	rem, _, err := e.rep.ApplyRetention(ctx, age, 1, func(phase string, done, total int, _ []string) {
		calls = append(calls, fmt.Sprintf("%s %d/%d", phase, done, total))
	})
	if err != nil {
		t.Fatal(err)
	}
	if rem.Snapshots != 3 || rem.Jobs != 3 {
		t.Fatalf("removals = %d/%d, want 3/3", rem.Snapshots, rem.Jobs)
	}
	want := []string{
		"snapshots 1/3", "snapshots 2/3", "snapshots 3/3",
		"jobs 1/3", "jobs 2/3", "jobs 3/3",
	}
	if len(calls) != len(want) {
		t.Fatalf("progress callbacks = %v, want one per committed tranche %v (a monolithic single-transaction revert collapses this)", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("progress callback %d = %q, want %q (full sequence: %v)", i, calls[i], want[i], calls)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots WHERE NOT active`); n != 0 {
		t.Fatalf("superseded snapshots left = %d, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE status='failed'`); n != 0 {
		t.Fatalf("stale job attempts left = %d, want 0", n)
	}
}

// TestRetentionInterruptIT: cancelling the context right after the FIRST
// committed tranche must (a) fail the run, (b) leave that tranche's removal
// PERSISTED while the rest survives, (c) let a re-run remove the remainder,
// and (d) let a third run report zero — the #290 resume contract, proven at
// a tranche boundary.
func TestRetentionInterruptIT(t *testing.T) {
	e := openRetDB(t)
	e.seedTrancheFixture(t, 0, 3)
	age := 14 * 24 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tranches := 0
	_, _, err := e.rep.ApplyRetention(ctx, age, 1, func(phase string, done, total int, _ []string) {
		tranches++
		cancel() // kill the run exactly at the first tranche boundary
	})
	if err == nil {
		t.Fatal("an interrupted apply must return an error, not success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted apply error = %v, want context.Canceled", err)
	}
	if tranches != 1 {
		t.Fatalf("interrupt must land after the first tranche, saw %d callbacks", tranches)
	}
	// committed partial progress is PERSISTED: one snapshot gone, two remain
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots WHERE NOT active`); n != 2 {
		t.Fatalf("after interruption: superseded snapshots left = %d, want 2 (first tranche's removal must stay committed)", n)
	}
	// re-run with a live context resumes: removes the remaining two
	rem, _, err := e.rep.ApplyRetention(context.Background(), age, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rem.Snapshots != 2 {
		t.Fatalf("resuming run removed %d snapshots, want 2", rem.Snapshots)
	}
	// third run reports zero (#281 idempotence holds across the interrupt)
	rem2, plan2, err := e.rep.ApplyRetention(context.Background(), age, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rem2.Snapshots != 0 || rem2.Jobs != 0 || plan2.Snapshots.Remove != 0 {
		t.Fatalf("third run must report zero, got removals=%d/%d plan=%d", rem2.Snapshots, rem2.Jobs, plan2.Snapshots.Remove)
	}
}
