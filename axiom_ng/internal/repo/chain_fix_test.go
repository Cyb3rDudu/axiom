// Regression tests for the over-review findings F1 (completion serialized
// against the source advisory lock) and F2 (FK membership chain validated at
// claim / completion). Run against the isolated axiom_ng_repo_test database.
package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestMarkCompletedSerializesAgainstSync proves that MarkCompletedTx takes the
// source advisory lock: while an independent transaction (as a sync would) holds
// that lock for the job's source, completion cannot proceed until it is released.
func TestMarkCompletedSerializesAgainstSync(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:50", libraryID: "users/0",
		docKey: "Z1", attKey: "Z1", contentHash: h("sha256:z1"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(ctx, cj.LeaseRef); err != nil {
		t.Fatal(err)
	}

	// An independent transaction simulates the canonical sync holding the source
	// advisory lock for the whole run.
	conn, err := lr.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	syncTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var srcID string
	if err := lr.pool.QueryRow(ctx, `SELECT source_id::text FROM ingest_jobs WHERE id=$1`, jobID).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if _, err := syncTx.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey(srcID)); err != nil {
		t.Fatal(err)
	}

	// Start completion in a goroutine; it must block waiting for the advisory lock.
	type res struct {
		err error
	}
	done := make(chan res, 1)
	go func() {
		tx, err := lr.pool.Begin(ctx)
		if err != nil {
			done <- res{err: err}
			return
		}
		err = lr.rep.MarkCompletedTx(ctx, tx, cj.LeaseRef, "p", "1", "snap")
		_ = tx.Rollback(ctx)
		done <- res{err: err}
	}()

	select {
	case r := <-done:
		t.Fatalf("completion proceeded while source advisory lock held (should block): %v", r.err)
	case <-time.After(250 * time.Millisecond):
		// still blocked -> good
	}

	// Release the advisory lock; completion must then finish.
	if _, err := syncTx.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey(srcID)); err != nil {
		t.Fatal(err)
	}
	if err := syncTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("completion after lock release failed: %v", r.err)
	}
}

// TestCrossSourceJobSkipped proves a job whose attachment belongs to a different
// source than the job is skipped at claim (FK membership chain), not claimed.
func TestCrossSourceJobSkipped(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	// Two distinct sources each with a document + attachment (no ingest jobs).
	// We only enqueue a BROKEN cross-source job so it is the queue head.
	var srcA1, docA1 string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('http://localhost:51','users/0') RETURNING id::text`).Scan(&srcA1); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'A1',1,'book','Doc A1') RETURNING id::text`, srcA1).Scan(&docA1); err != nil {
		t.Fatal(err)
	}
	var srcA2 string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('http://localhost:52','users/1') RETURNING id::text`).Scan(&srcA2); err != nil {
		t.Fatal(err)
	}
	var docA2, attA2 string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1,'A2',1,'book','Doc A2') RETURNING id::text`, srcA2).Scan(&docA2); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version, parent_zotero_key, link_mode, content_type, filename, content_hash, preferred)
		VALUES ($1,$2,'A2',1,'A2','imported_file','application/pdf','a2.pdf','sha256:a2',true) RETURNING id::text`, srcA2, docA2).Scan(&attA2); err != nil {
		t.Fatal(err)
	}

	// BROKEN job: S1's document but S2's attachment (FK-valid, semantically
	// cross-source).
	var brokeID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, max_attempts)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'sha256:x', 'pending', 3) RETURNING id::text`,
		srcA1, docA1, attA2).Scan(&brokeID); err != nil {
		t.Fatalf("insert broken cross-source job: %v", err)
	}

	// Claim must skip it (PARENT_REMOVED), never claim with B/C rows frozen.
	if cjt := lr.claim(t, defaultClaim("worker-a")); cjt != nil {
		t.Fatalf("cross-source job must be skipped, got claimed %v", cjt)
	}
	if r := lr.rowOf(t, brokeID); r.status != "skipped" || r.errorMessage == nil || *r.errorMessage != "PARENT_REMOVED" {
		t.Fatalf("cross-source job = %s/%v, want skipped/PARENT_REMOVED", r.status, r.errorMessage)
	}
}

// TestCompletionRejectsCrossDocumentChain proves MarkCompletedTx refuses to
// complete when the attachment no longer belongs to the job's document (chain
// broken after claim), even under the source lock.
func TestCompletionRejectsCrossDocumentChain(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:53", libraryID: "users/0",
		docKey: "B1", attKey: "B1", contentHash: h("sha256:b1"), preferred: true,
	}, "pending", 3)
	_ = jobID
	cj := lr.claim(t, defaultClaim("worker-a"))
	if err := lr.rep.MarkProcessing(ctx, cj.LeaseRef); err != nil {
		t.Fatal(err)
	}

	// Break the chain: reparent the attachment to a different document; and give
	// the new document a different source so the membership check fails.
	var newSrcID, newDocID string
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_sources (base_url, library_id) VALUES ('http://localhost:54','users/1') RETURNING id::text`).Scan(&newSrcID); err != nil {
		t.Fatal(err)
	}
	if err := lr.pool.QueryRow(ctx, `
		INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
		VALUES ($1::uuid,'OTHER','1','book','Other') RETURNING id::text`, newSrcID).Scan(&newDocID); err != nil {
		t.Fatal(err)
	}
	if _, err := lr.pool.Exec(ctx, `
		UPDATE zotero_attachments SET document_id=$1::uuid, source_id=$2::uuid, parent_zotero_key='OTHER' WHERE zotero_key='B1'`, newDocID, newSrcID); err != nil {
		t.Fatal(err)
	}

	tx, err := lr.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = lr.rep.MarkCompletedTx(ctx, tx, cj.LeaseRef, "p", "1", "snap")
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("completion with broken chain = %v, want ErrLostLease", err)
	}
}

// TestImmediateCancelClearsAllLeaseFields verifies the immediate (pending)
// cancellation path clears every lease/scheduling field.
func TestImmediateCancelClearsAllLeaseFields(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:55", libraryID: "users/0",
		docKey: "C1", attKey: "C1", contentHash: h("sha256:c1"), preferred: true,
	}, "pending", 3)
	// Set lease/scheduling fields as if it had been claimed then reset to pending.
	if _, err := lr.pool.Exec(ctx, `
		UPDATE ingest_jobs SET status='pending', claimed_by='w', lease_token=gen_random_uuid(),
			lease_until=now()+interval '60 seconds', last_heartbeat_at=now(),
			next_attempt_at=now()+interval '1 minute' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := lr.rep.RequestCancellation(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	r := lr.rowOf(t, jobID)
	if r.status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", r.status)
	}
	if r.claimedBy != nil || r.leaseToken != nil || r.leaseUntil != nil || r.lastHeartbeat != nil || r.nextAttemptAt != nil {
		t.Fatalf("immediate cancel left lease/scheduling fields: owner=%v token=%v until=%v hb=%v next=%v",
			r.claimedBy, r.leaseToken, r.leaseUntil, r.lastHeartbeat, r.nextAttemptAt)
	}
	if r.completedAt == nil {
		t.Fatal("immediate cancel must set completed_at")
	}
	if r.cancelRequested == nil {
		t.Fatal("immediate cancel must set cancel_requested_at")
	}
}

// TestLeaseDurationValidated proves claim and renew reject sub-second / non-positive
// durations rather than silently producing an already-expired lease.
func TestLeaseDurationValidated(t *testing.T) {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	ctx := context.Background()

	for _, d := range []time.Duration{0, -time.Second, 500 * time.Millisecond} {
		opts := defaultClaim("worker-a")
		opts.LeaseDuration = d
		if _, err := lr.rep.ClaimNextJob(ctx, opts); err == nil {
			t.Fatalf("claim with duration %v must error", d)
		} else if !strings.Contains(err.Error(), "lease") {
			t.Fatalf("claim duration err = %v, want lease-related", err)
		}
	}

	// Renew also rejects bad durations.
	_, jobID := lr.seed(t, seedSpec{
		sourceBaseURL: "http://localhost:56", libraryID: "users/0",
		docKey: "D1", attKey: "D1", contentHash: h("sha256:d1"), preferred: true,
	}, "pending", 3)
	cj := lr.claim(t, defaultClaim("worker-a"))
	if cj == nil || cj.JobID != jobID {
		t.Fatalf("expected claim %s, got %v", jobID, cj)
	}
	if err := lr.rep.RenewLease(ctx, cj.LeaseRef, time.Second-1); err == nil {
		t.Fatalf("renew with sub-second duration must error")
	}
}
