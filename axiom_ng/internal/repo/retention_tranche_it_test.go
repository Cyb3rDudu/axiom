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
	rem, _, err := e.rep.ApplyRetention(ctx, age, 1, func(phase string, done, total int) {
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
	_, _, err := e.rep.ApplyRetention(ctx, age, 1, func(phase string, done, total int) {
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
