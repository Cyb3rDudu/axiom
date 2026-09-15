package repo

// #259 IT: first-class force-rebuild re-ingest after ARTIFACTS_EXPIRED.
// Runs only against a DEDICATED test database: AXIOM_TEST_DATABASE_URL must
// point at a *_test database (same harness as lease_integration_test.go).
//
// DoD evidence: job completed → EnqueueForceRebuild → the claim of the NEW
// job derives a fresh snapshot + idempotency key that the runner's durable
// dedup has never seen — the precondition for "runner executes fresh, no
// 409/ARTIFACTS_EXPIRED" (the runner keys dedup on the idempotency key).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// docIDOf resolves the document behind a seeded attachment.
func (lr *leaseRepo) docIDOf(t *testing.T, attID string) string {
	t.Helper()
	var docID string
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT document_id::text FROM zotero_attachments WHERE id=$1`, attID).Scan(&docID); err != nil {
		t.Fatalf("docIDOf: %v", err)
	}
	return docID
}

// forceFlagOf reads force_rebuild for a job row.
func (lr *leaseRepo) forceFlagOf(t *testing.T, jobID string) bool {
	t.Helper()
	var force bool
	if err := lr.pool.QueryRow(context.Background(),
		`SELECT force_rebuild FROM ingest_jobs WHERE id=$1`, jobID).Scan(&force); err != nil {
		t.Fatalf("forceFlagOf: %v", err)
	}
	return force
}

func TestEnqueueForceRebuildAfterAckedJob(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	attID, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:259", libraryID: "users/0",
		docKey: "R1", attKey: "R1", contentHash: h("sha256:r1"), preferred: true,
	}, "pending", 3)
	docID := lr.docIDOf(t, attID)

	// 1. The original job runs to completion (claim freezes snapshot+key,
	//    then the job settles completed — the "runner acknowledged" state).
	cj1 := lr.claim(t, defaultClaim("worker-a"))
	if cj1 == nil || cj1.JobID != jobID {
		t.Fatalf("expected claim of %s, got %+v", jobID, cj1)
	}
	if _, err := lr.pool.Exec(ctx,
		`UPDATE ingest_jobs SET status='completed', completed_at=now() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("complete job: %v", err)
	}

	// 2. Operator re-ingest via the API path (design a): a NEW pending
	//    force job next to the completed non-force job.
	job2, err := lr.rep.EnqueueForceRebuild(ctx, docID)
	if err != nil {
		t.Fatalf("EnqueueForceRebuild: %v", err)
	}
	if job2.Status != "pending" {
		t.Fatalf("new job status = %s, want pending", job2.Status)
	}
	if !job2.ForceRebuild {
		t.Fatal("returned job must carry ForceRebuild=true")
	}
	if job2.EnqueuedAt == "" {
		t.Fatal("returned job must carry the enqueue timestamp")
	}
	if !lr.forceFlagOf(t, job2.ID) {
		t.Fatal("new job must carry force_rebuild=true")
	}
	// List views must report the flag honestly (regression pin: the lists
	// used to serialize ForceRebuild=false even for force rows).
	byAtt, err := lr.rep.ListJobsByAttachment(ctx, attID)
	if err != nil {
		t.Fatal(err)
	}
	foundForce := false
	for _, j := range byAtt {
		if j.ID == job2.ID {
			foundForce = j.ForceRebuild
		}
	}
	if !foundForce {
		t.Fatal("ListJobsByAttachment must report ForceRebuild=true for the force job")
	}
	active, err := lr.rep.ActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range active {
		if j.ID == job2.ID && !j.ForceRebuild {
			t.Fatal("ActiveJobs must report ForceRebuild=true for the force job")
		}
	}
	// The completed non-force job is untouched (history stays).
	if row := lr.rowOf(t, jobID); row.status != "completed" {
		t.Fatalf("old job status = %s, want completed", row.status)
	}

	// 3. The dispatcher claims the force job: fresh snapshot, fresh key.
	cj2 := lr.claim(t, defaultClaim("worker-b"))
	if cj2 == nil || cj2.JobID != job2.ID {
		t.Fatalf("expected claim of the force job %s, got %+v", job2.ID, cj2)
	}
	if cj2.IdempotencyKey == nil || cj1.IdempotencyKey == nil {
		t.Fatal("idempotency keys missing")
	}
	if *cj2.IdempotencyKey == *cj1.IdempotencyKey {
		t.Fatalf("force rebuild reuses the old idempotency key %s — the runner dedup would answer ARTIFACTS_EXPIRED", *cj2.IdempotencyKey)
	}
	if !strings.Contains(*cj2.IdempotencyKey, "force-") {
		t.Fatalf("force idempotency key lacks force marker: %s", *cj2.IdempotencyKey)
	}
	if !cj2.ForceRebuild {
		t.Fatal("claimed job must carry force_rebuild=true")
	}

	// 4. Guard: a second enqueue while the force job is claimed refuses.
	if _, err := lr.rep.EnqueueForceRebuild(ctx, docID); !errors.Is(err, ErrRebuildInFlight) {
		t.Fatalf("second enqueue while in flight: err = %v, want ErrRebuildInFlight", err)
	}
}

// TestEnqueueForceRebuildBlockedByLockHeldElsewhere is the DETERMINISTIC
// serialization pin: a second connection holds the advisory lock on the
// attachment while EnqueueForceRebuild runs — the enqueue must BLOCK until
// the lock is released (proof it takes the same lock). Mutation-checked:
// removing pg_advisory_xact_lock from EnqueueForceRebuild makes the enqueue
// complete while the lock is held → red. (The 16-goroutine test above is a
// soak, not a discriminator — its race window is too thin to catch the
// mutation reliably.)
func TestEnqueueForceRebuildBlockedByLockHeldElsewhere(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	attID, _ := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:259y", libraryID: "users/0",
		docKey: "L1", attKey: "L1", contentHash: h("sha256:l1"), preferred: true,
	}, "completed", 3)
	docID := lr.docIDOf(t, attID)

	// A second pool/connection holds the advisory lock in an open tx.
	other, err := pgxpool.New(ctx, lr.dsn)
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	holder, err := other.Begin(ctx)
	if err != nil {
		t.Fatalf("holder tx: %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, attID); err != nil {
		t.Fatalf("acquire lock: %v", err)
	}

	type outcome struct {
		job *Job
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		job, err := lr.rep.EnqueueForceRebuild(context.WithoutCancel(ctx), docID)
		done <- outcome{job, err}
	}()

	// While the lock is held the enqueue must NOT complete.
	blocked := true
	select {
	case <-done:
		blocked = false
	case <-time.After(750 * time.Millisecond):
		// still blocked: the enqueue waits on the same lock — exactly right
	}
	// ALWAYS release the lock before asserting, so a failing run cannot
	// leak the open holder transaction into pool.Close (which would wait
	// for the connection until the test timeout).
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	if !blocked {
		t.Fatalf("enqueue completed while the advisory lock was held elsewhere — serialization broken")
	}

	// Releasing the lock lets the enqueue through.
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("enqueue after lock release: %v", got.err)
		}
		if got.job == nil || !got.job.ForceRebuild {
			t.Fatalf("enqueue after lock release: %+v", got.job)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("enqueue never completed after the lock was released")
	}
}

// TestEnqueueForceRebuildConcurrentSingleWinner is the soak: 16 concurrent
// enqueues admit exactly one winner. A bare INSERT…SELECT…WHERE NOT EXISTS
// is NOT mutual exclusion under READ COMMITTED (each statement evaluates
// the predicate against the other's pre-commit snapshot, and no constraint
// covers force rows) — the advisory lock in EnqueueForceRebuild must admit
// exactly ONE winner per attachment. NOTE: this is not a discriminating
// mutation pin (the window is too thin); the deterministic pin is
// TestEnqueueForceRebuildBlockedByLockHeldElsewhere above.
func TestEnqueueForceRebuildConcurrentSingleWinner(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	attID, _ := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:259x", libraryID: "users/0",
		docKey: "C1", attKey: "C1", contentHash: h("sha256:c1"), preferred: true,
	}, "completed", 3)
	docID := lr.docIDOf(t, attID)

	const n = 16
	start := make(chan struct{})
	res := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, err := lr.rep.EnqueueForceRebuild(ctx, docID)
			res <- err
		}()
	}
	close(start)
	wins, refused := 0, 0
	for i := 0; i < n; i++ {
		if err := <-res; err == nil {
			wins++
		} else if errors.Is(err, ErrRebuildInFlight) {
			refused++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || refused != n-1 {
		t.Fatalf("concurrent enqueues: %d winners, %d refused — want exactly 1/%d (serialization broken)", wins, refused, n-1)
	}
	// Exactly one pending force row exists.
	var pending int
	if err := lr.pool.QueryRow(ctx, `
		SELECT count(*) FROM ingest_jobs
		WHERE attachment_id=$1 AND force_rebuild AND status='pending'`, attID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending force rows = %d, want 1", pending)
	}
}

func TestEnqueueForceRebuildRefusals(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// Unknown document → ErrNoPreferredAttachment.
	if _, err := lr.rep.EnqueueForceRebuild(ctx, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, ErrNoPreferredAttachment) {
		t.Fatalf("unknown document: err = %v, want ErrNoPreferredAttachment", err)
	}

	// Attachment without any hash anywhere → ErrNoContentHash.
	attID, _ := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:259b", libraryID: "users/0",
		docKey: "R2", attKey: "R2", contentHash: nil, preferred: true,
	}, "completed", 3)
	docID := lr.docIDOf(t, attID)
	if _, err := lr.rep.EnqueueForceRebuild(ctx, docID); !errors.Is(err, ErrNoContentHash) {
		t.Fatalf("no hash: err = %v, want ErrNoContentHash", err)
	}

	// Attachment hash NULL but a prior job carries one → the job's hash is
	// used (the generation being rebuilt).
	attID3, _ := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:259c", libraryID: "users/0",
		docKey: "R3", attKey: "R3", contentHash: h("sha256:r3-job"), preferred: true,
	}, "completed", 3)
	if _, err := lr.pool.Exec(ctx, `UPDATE zotero_attachments SET content_hash=NULL WHERE id=$1`, attID3); err != nil {
		t.Fatal(err)
	}
	docID3 := lr.docIDOf(t, attID3)
	job3, err := lr.rep.EnqueueForceRebuild(ctx, docID3)
	if err != nil {
		t.Fatalf("hash-fallback enqueue: %v", err)
	}
	if job3.ContentHash == nil || *job3.ContentHash != "sha256:r3-job" {
		t.Fatalf("fallback hash = %v, want the prior job's sha256:r3-job", job3.ContentHash)
	}
}
