package repo

// #281 retention IT (real DB, fixture-built state mirroring the situation
// AFTER the strand's changes): superseded snapshots with derived rows,
// stale terminal job attempts, the never-delete list (latest job per
// document, repair-linked jobs, active-snapshot producers, pending work,
// snapshots with pending outbox rows), dry-run exactness, apply effects,
// and idempotence (second run reports zero).

import (
	"context"
	"testing"
	"time"
)

type retEnv struct {
	*leaseRepo
}

func openRetDB(t *testing.T) *retEnv {
	lr := openLeaseDB(t)
	lr.truncateFixtures(t)
	return &retEnv{lr}
}

// seedSnapshot inserts one snapshot row (active flag, job link) and
// N chunks (+ dense embedding each). Returns the snapshot id.
func (e *retEnv) seedSnapshot(t *testing.T, active bool, jobID *string, chunks int, pendingOutbox bool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	var job any
	if jobID != nil {
		job = *jobID
	}
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active, ingest_job_id)
		SELECT a.id, 'hash-' || gen_random_uuid()::text, 'p', 'v1', 'ph', a.document_id, '{}', $1,
		       CASE WHEN $2::text IS NULL THEN NULL ELSE $2::uuid END
		FROM zotero_attachments a LIMIT 1
		RETURNING id::text`, active, job).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < chunks; i++ {
		var chunkID string
		if err := e.pool.QueryRow(ctx, `
			INSERT INTO processing_chunks (snapshot_id, chunk_index, text)
			VALUES ($1::uuid, $2, $3) RETURNING id::text`, id, i, "chunk text").Scan(&chunkID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO processing_chunk_dense_embeddings (chunk_id, model, dimensions, vector)
			VALUES ($1::uuid, 'm', 3, '[1,2,3]')`, chunkID); err != nil {
			t.Fatal(err)
		}
	}
	if pendingOutbox {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO opensearch_outbox (snapshot_id, operation, payload)
			VALUES ($1::uuid, 'delete', '{}')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// seedJobForAttachment inserts a job row with a fixed age; returns its id.
func (e *retEnv) seedJobForAttachment(t *testing.T, status string, age time.Duration) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO ingest_jobs (status, attachment_id, content_hash, enqueued_at, error_code, error_message)
		SELECT $1, a.id, 'h-' || gen_random_uuid()::text, now() - make_interval(secs => $2), 'X', 'old attempt'
		FROM zotero_attachments a LIMIT 1 RETURNING id::text`, status, age.Seconds()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *retEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionIT(t *testing.T) {
	e := openRetDB(t)
	ctx := context.Background()

	// ── fixture: one document + two attachments (generations) ──────────
	docID, _ := e.seed(t, seedSpec{sourceBaseURL: "https://zotero.ret", libraryID: "lib-ret",
		docKey: "RETDOC", attKey: "RETATT1", contentHash: nil}, "completed", 1)
	_ = docID

	// latest job (recent) + a STALE attempt on the same document: the
	// stale one is prunable (older than age, not latest, no repair link)
	latestJob := e.seedJobForAttachment(t, "completed", time.Hour)
	e.seedJobForAttachment(t, "failed", 20*24*time.Hour)

	// repair-linked stale job (different attachment of the same doc): NEVER
	att2 := e.seedAttachmentIfAbsent(t, "RETATT2")
	repairJob := e.seedJobForAttachment(t, "skipped", 20*24*time.Hour)
	if _, err := e.pool.Exec(ctx, `
		UPDATE ingest_jobs SET attachment_id = $1::uuid WHERE id = $2`, att2, repairJob); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class, analysis)
		VALUES ($1::uuid, NULL, 'healed', '🔴 reparierbar', '{}')`, att2); err != nil {
		t.Fatal(err)
	}

	// pending job (never pruned) + active-snapshot producer (never pruned)
	pendingJob := e.seedJobForAttachment(t, "pending", 20*24*time.Hour)
	producerJob := e.seedJobForAttachment(t, "completed", 20*24*time.Hour)

	// snapshots: active (kept), superseded with done outbox (removed),
	// superseded with PENDING outbox (kept — the OS delete must drain)
	activeSnap := e.seedSnapshot(t, true, &producerJob, 2, false)
	_ = activeSnap
	supSnap := e.seedSnapshot(t, false, nil, 3, false)
	pendSnap := e.seedSnapshot(t, false, nil, 2, true)

	age := 14 * 24 * time.Hour

	// ── dry-run: exact counts, nothing deleted ──────────────────────────
	plan, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshots.Remove != 1 {
		t.Fatalf("snapshots to remove = %d, want 1 (only the pending-outbox-free superseded one)", plan.Snapshots.Remove)
	}
	if plan.Snapshots.KeepPendingOutbox != 1 {
		t.Fatalf("pending-outbox keeps = %d, want 1", plan.Snapshots.KeepPendingOutbox)
	}
	if plan.Snapshots.Chunks != 3 || plan.Snapshots.DenseEmbeddings != 3 {
		t.Fatalf("derived removal counts = chunks %d / dense %d, want 3/3", plan.Snapshots.Chunks, plan.Snapshots.DenseEmbeddings)
	}
	if plan.Jobs.Remove != 1 {
		t.Fatalf("jobs to remove = %d, want 1 (only the stale non-latest unlinked attempt)", plan.Jobs.Remove)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs`); n != 6 {
		t.Fatalf("dry-run must not delete: jobs = %d, want 6", n)
	}

	// ── apply ────────────────────────────────────────────────────────────
	rem, _, err := e.rep.ApplyRetention(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if rem.Snapshots != 1 || rem.Jobs != 1 {
		t.Fatalf("removals = %d/%d, want 1/1", rem.Snapshots, rem.Jobs)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots`); n != 2 {
		t.Fatalf("snapshots left = %d, want 2 (active + pending-outbox)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id WHERE s.id = $1::uuid`, supSnap); n != 0 {
		t.Fatalf("superseded snapshot's chunks must cascade away, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id WHERE s.id = $1::uuid`, pendSnap); n != 2 {
		t.Fatalf("pending-outbox snapshot keeps its chunks, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs`); n != 5 {
		t.Fatalf("jobs left = %d, want 5 (seed-job, latest, repair-linked, pending, producer)", n)
	}
	for _, keep := range []string{latestJob, repairJob, pendingJob, producerJob} {
		if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, keep); n != 1 {
			t.Fatalf("never-delete job %s was pruned", keep)
		}
	}

	// ── outcome truth: latest job per document is untouched (no flip) ────
	var latestStatus string
	if err := e.pool.QueryRow(ctx, `
		SELECT j.status FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id = j.attachment_id
		WHERE j.id = $1::uuid`, latestJob).Scan(&latestStatus); err != nil {
		t.Fatal(err)
	}
	if latestStatus != "completed" {
		t.Fatalf("latest job flipped to %q — outcome truth must be stable", latestStatus)
	}

	// ── idempotence: second run reports zero removals ───────────────────
	plan2, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Snapshots.Remove != 0 || plan2.Jobs.Remove != 0 {
		t.Fatalf("second run must report zero: got snapshots=%d jobs=%d", plan2.Snapshots.Remove, plan2.Jobs.Remove)
	}
	rem2, _, err := e.rep.ApplyRetention(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if rem2.Snapshots != 0 || rem2.Jobs != 0 {
		t.Fatalf("second apply must remove zero: got %d/%d", rem2.Snapshots, rem2.Jobs)
	}
}

// seedAttachmentIfAbsent adds a second attachment under the harness document.
func (e *retEnv) seedAttachmentIfAbsent(t *testing.T, key string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path)
		SELECT source_id, document_id, $1, 1, 'RETDOC', 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf'
		FROM zotero_attachments WHERE zotero_key = 'RETATT1'
		RETURNING id::text`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
