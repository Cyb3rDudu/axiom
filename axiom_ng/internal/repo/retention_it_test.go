package repo

// #281 retention IT (real DB, fixture-built state mirroring the situation
// AFTER the strand's changes): superseded snapshots with derived rows,
// stale terminal job attempts, the never-delete list (latest job per
// document, repair-linked jobs, active-snapshot producers, pending work,
// snapshots with pending outbox rows), dry-run exactness, apply effects,
// and idempotence (second run reports zero).

import (
	"context"
	"fmt"
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
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO processing_chunk_sparse_embeddings (chunk_id, model, values)
			VALUES ($1::uuid, 'm', '{"k": 1.0}')`, chunkID); err != nil {
			t.Fatal(err)
		}
	}
	// entity + mention + one chunk relationship per snapshot (derived-count probes)
	var entID string
	if err := e.pool.QueryRow(ctx, `
			INSERT INTO processing_entities (snapshot_id, ref, text, canonical_form, type)
			VALUES ($1::uuid, 'e1', 'Testentität', 'Testentität', 'CONCEPT') RETURNING id::text`, id).Scan(&entID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
			INSERT INTO processing_entity_mentions (entity_id, chunk_id, start_char, end_char)
			SELECT $1::uuid, c.id, 0, 4 FROM processing_chunks c WHERE c.snapshot_id=$2::uuid LIMIT 1`, entID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
			INSERT INTO processing_chunk_relationships (snapshot_id, source_chunk_id, target_chunk_id, type)
			SELECT $1::uuid, c1.id, c2.id, 'sequential' FROM processing_chunks c1, processing_chunks c2
			WHERE c1.snapshot_id=$1::uuid AND c2.snapshot_id=$1::uuid AND c1.chunk_index=0 AND c2.chunk_index=1`, id); err != nil {
		t.Fatal(err)
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

// seedJobForAttachment inserts a job row with a fixed age on the named
// attachment (deterministic — LIMIT 1 made the fixture non-reproducible);
// returns its id. updAge optionally pins updated_at (the outcome key).
func (e *retEnv) seedJobForAttachment(t *testing.T, attKey, status string, age, updAge time.Duration) string {
	t.Helper()
	var id string
	upd := "now()"
	if updAge > 0 {
		upd = "now() - make_interval(secs => " + fmt.Sprintf("%.0f", updAge.Seconds()) + ")"
	}
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO ingest_jobs (status, attachment_id, content_hash, enqueued_at, updated_at, error_code, error_message)
		SELECT $1, a.id, 'h-' || gen_random_uuid()::text, now() - make_interval(secs => $2), `+upd+`, 'X', 'old attempt'
		FROM zotero_attachments a WHERE a.zotero_key = $3 RETURNING id::text`, status, age.Seconds(), attKey).Scan(&id); err != nil {
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
		docKey: "RETDOC", attKey: "RETATT1", contentHash: nil, preferred: true}, "completed", 1)
	_ = docID

	// deterministic layout on RETATT1 (preferred):
	//   latest  — completed, 1h young (age filter keeps it anyway)
	//   stale   — failed, 20d: THE prunable row (newer sibling exists, not
	//             the preferred-newest, no repair link, no active snapshot)
	//   pending — non-terminal, never pruned
	//   producer— produced the active snapshot, never pruned (old updated_at
	//             so it is NOT the preferred-newest — proves the guards are
	//             independent)
	//   repA    — reviewer repro A (below)
	latestJob := e.seedJobForAttachment(t, "RETATT1", "completed", time.Hour, 0)
	staleJob := e.seedJobForAttachment(t, "RETATT1", "failed", 20*24*time.Hour, 20*24*time.Hour)
	_ = staleJob // asserted via the total prunable count + survival list
	pendingJob := e.seedJobForAttachment(t, "RETATT1", "pending", 20*24*time.Hour, 0)
	producerJob := e.seedJobForAttachment(t, "RETATT1", "completed", 20*24*time.Hour, 25*24*time.Hour)

	// repair-linked stale job (different attachment of the same doc): NEVER
	att2 := e.seedAttachmentIfAbsent(t, "RETATT2")
	repairJob := e.seedJobForAttachment(t, "RETATT2", "skipped", 20*24*time.Hour, 0)
	// newer-enqueued sibling on the NON-preferred attachment (drives the
	// sibling axis for repro A without touching RETATT1's outcome key)
	sib := e.seedJobForAttachment(t, "RETATT2", "completed", time.Hour, 0)
	_ = sib // young sibling; covered by total counts
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO repair_cases (attachment_id, document_id, status, suspicion_class, analysis)
		VALUES ($1::uuid, NULL, 'healed', '🔴 reparierbar', '{}')`, att2); err != nil {
		t.Fatal(err)
	}

	// independent document whose ONLY job is OLD and terminal (no newer
	// sibling): it IS that document's latest job — never prunable (review
	// fix: the keep-latest leg needs an age-eligible probe; a young latest
	// job is masked by the age filter alone)
	var oldLatest string
	if err := e.pool.QueryRow(ctx, `
		WITH s AS (INSERT INTO zotero_sources (base_url, library_id, server_id)
			VALUES ('https://zotero.ret2', 'lib-ret2', 'srv2') RETURNING id),
		d AS (INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title, creators, publication_year)
			SELECT id, 'RETDOC2', 1, 'book', 'Retention Old Latest', '[{"first":"A","last":"Autor"}]', 2024 FROM s RETURNING id),
		a AS (INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred)
			SELECT s.id, d.id, 'RETATT3', 1, 'RETDOC2', 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', true
			FROM s, d RETURNING id)
		INSERT INTO ingest_jobs (status, attachment_id, content_hash, enqueued_at)
		SELECT 'completed', a.id, 'old-latest-hash', now() - interval '20 days' FROM a
		RETURNING id::text`).Scan(&oldLatest); err != nil {
		t.Fatal(err)
	}

	// ── reviewer repro A (own document RETDOC5/RETATT5 preferred +
	// RETATT6 non-preferred): the preferred attachment's OLD completed job
	// (updated 5h — its newest by updated_at) with a newer-enqueued sibling
	// on the NON-preferred attachment. The old keep rule (enqueued_at
	// across all attachments) pruned exactly this row → the document's
	// outcome flipped completed → pending/never-enqueued. The outcome-key
	// guard must keep it. (Own document: young jobs on a shared attachment
	// would outrank the repro row on the outcome key.)
	_ = e.seedDocWithAttachment(t, "RETDOC5", "RETATT5")
	_ = e.seedAttachmentForDoc(t, "RETDOC5", "RETATT6", false)
	repA := e.seedJobForAttachment(t, "RETATT5", "completed", 20*24*time.Hour, 5*time.Hour)
	_ = e.seedJobForAttachment(t, "RETATT6", "completed", time.Hour, 0) // the newer-enqueued sibling

	// ── reviewer repro B (own document RETDOC3/RETATT4, preferred):
	// updated_at/enqueued_at inversion — invOLD completed (enq 20d, upd 1h)
	// vs invNEW failed (enq 19d, upd 2h). Outcome reads invOLD (updated_at
	// newest); it must survive even though invNEW is the newer-enqueued
	// sibling. #294 sharpens the sibling guard to the same key: invNEW is
	// now an OLDER attempt by the read-model key → prunable (the apply
	// count below includes it).
	_ = e.seedDocWithAttachment(t, "RETDOC3", "RETATT4")
	repB := e.seedJobForAttachment(t, "RETATT4", "completed", 20*24*time.Hour, time.Hour)
	invNew := e.seedJobForAttachment(t, "RETATT4", "failed", 19*24*time.Hour, 2*time.Hour)
	_ = invNew

	// snapshots: active (kept), superseded with done outbox (removed),
	// superseded with PENDING outbox (kept — the OS delete must drain),
	// superseded with FAILED outbox (kept — recoverable delete-op; review
	// mutation pin: IN ('pending','failed') -> IN ('pending') must go red)
	activeSnap := e.seedSnapshot(t, true, &producerJob, 2, false)
	_ = activeSnap
	supSnap := e.seedSnapshot(t, false, nil, 3, false)
	pendSnap := e.seedSnapshot(t, false, nil, 2, true)
	failSnap := e.seedSnapshot(t, false, nil, 1, false)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO opensearch_outbox (snapshot_id, operation, payload, status, last_error)
		VALUES ($1::uuid, 'delete', '{}', 'failed', 'os down (IT)')`, failSnap); err != nil {
		t.Fatal(err)
	}
	// drained (done) outbox row on the DELETABLE snapshot — the cascade
	// claim ("plus their drained outbox rows") gets its pin here
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO opensearch_outbox (snapshot_id, operation, payload, status)
		VALUES ($1::uuid, 'index', '{}', 'done')`, supSnap); err != nil {
		t.Fatal(err)
	}
	// artifact ROW with a durable storage_path — ApplyRetention must hand
	// the path to the caller for the post-commit byte unlink (#270 review)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO processing_artifacts (snapshot_id, ref, kind, media_type, sha256, size_bytes, storage_path)
		VALUES ($1::uuid, 'fig1', 'image', 'image/png', 'x', 1, '/tmp/retention-artifact-probe.png')`, supSnap); err != nil {
		t.Fatal(err)
	}

	age := 14 * 24 * time.Hour

	// ── dry-run: exact counts, nothing deleted ──────────────────────────
	plan, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Snapshots.Remove != 1 {
		t.Fatalf("snapshots to remove = %d, want 1 (only the outbox-free superseded one)", plan.Snapshots.Remove)
	}
	if plan.Snapshots.KeepOutboxHeld != 2 {
		t.Fatalf("outbox-held keeps = %d, want 2 (pending + failed)", plan.Snapshots.KeepOutboxHeld)
	}
	if plan.Snapshots.Chunks != 3 || plan.Snapshots.DenseEmbeddings != 3 {
		t.Fatalf("derived removal counts = chunks %d / dense %d, want 3/3", plan.Snapshots.Chunks, plan.Snapshots.DenseEmbeddings)
	}
	if plan.Snapshots.Artifacts != 1 {
		t.Fatalf("plan must count the artifact row for removal, got %d", plan.Snapshots.Artifacts)
	}
	if plan.Snapshots.SparseEmbeddings != 3 || plan.Snapshots.Entities != 1 ||
		plan.Snapshots.EntityMentions != 1 || plan.Snapshots.ChunkRelationships != 1 {
		t.Fatalf("derived removal counts: sparse=%d entities=%d mentions=%d chunkrels=%d, want 3/1/1/1",
			plan.Snapshots.SparseEmbeddings, plan.Snapshots.Entities, plan.Snapshots.EntityMentions, plan.Snapshots.ChunkRelationships)
	}
	if plan.Jobs.Remove != 2 {
		t.Fatalf("jobs to remove = %d, want 2 (the stale row + repro B's older-by-key invNEW)", plan.Jobs.Remove)
	}
	// outcome-truth keeps: oldLatest, repA, repB (the preferred attachments'
	// newest-by-updated_at jobs); latestJob/producerJob are age-young and
	// not counted here
	if plan.Jobs.KeepLatest != 3 {
		t.Fatalf("outcome-truth keep count = %d, want 3 (oldLatest, repA, repB)", plan.Jobs.KeepLatest)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs`); n != 12 {
		t.Fatalf("dry-run must not delete: jobs = %d, want 12", n)
	}

	// ── apply ────────────────────────────────────────────────────────────
	rem, _, err := e.rep.ApplyRetention(ctx, age, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rem.Snapshots != 1 || rem.Jobs != 2 {
		t.Fatalf("removals = %d/%d, want 1/2", rem.Snapshots, rem.Jobs)
	}
	if len(rem.ArtifactPaths) != 1 || rem.ArtifactPaths[0] != "/tmp/retention-artifact-probe.png" {
		t.Fatalf("ApplyRetention must return artifact paths for the post-commit unlink, got %v", rem.ArtifactPaths)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots`); n != 3 {
		t.Fatalf("snapshots left = %d, want 3 (active + pending-outbox + failed-outbox)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_snapshots WHERE id=$1::uuid`, failSnap); n != 1 {
		t.Fatalf("failed-outbox snapshot must survive (recoverable delete-op), got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id WHERE s.id = $1::uuid`, supSnap); n != 0 {
		t.Fatalf("superseded snapshot's chunks must cascade away, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM processing_chunks c
		JOIN processing_snapshots s ON s.id = c.snapshot_id WHERE s.id = $1::uuid`, pendSnap); n != 2 {
		t.Fatalf("pending-outbox snapshot keeps its chunks, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs`); n != 10 {
		t.Fatalf("jobs left = %d, want 10 (all but the stale row and invNEW)", n)
	}
	for _, keep := range []string{latestJob, repairJob, pendingJob, producerJob, oldLatest, repA, repB} {
		if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, keep); n != 1 {
			t.Fatalf("never-delete job %s was pruned", keep)
		}
	}
	// done outbox rows of the deleted snapshot cascade away with it
	if n := e.count(t, `SELECT count(*) FROM opensearch_outbox WHERE snapshot_id=$1::uuid`, supSnap); n != 0 {
		t.Fatalf("done outbox rows must cascade with the deleted snapshot, got %d", n)
	}
	// ── idempotence: second run reports zero removals ───────────────────
	plan2, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Snapshots.Remove != 0 || plan2.Jobs.Remove != 0 {
		t.Fatalf("second run must report zero: got snapshots=%d jobs=%d", plan2.Snapshots.Remove, plan2.Jobs.Remove)
	}
	rem2, _, err := e.rep.ApplyRetention(ctx, age, 0, nil)
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

// seedDocWithAttachment creates a minimal source/document/preferred-attachment
// fixture and returns the attachment id.
func (e *retEnv) seedDocWithAttachment(t *testing.T, docKey, attKey string) string {
	t.Helper()
	var attID string
	if err := e.pool.QueryRow(context.Background(), `
		WITH s AS (INSERT INTO zotero_sources (base_url, library_id, server_id)
			VALUES ('https://zotero.' || $1, 'lib-' || $1, 'srv-' || $1) RETURNING id),
		d AS (INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
			SELECT id, $1, 1, 'book', 'Retention ' || $1 FROM s RETURNING id)
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred)
		SELECT s.id, d.id, $2, 1, $1, 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', true
		FROM s, d RETURNING id::text`, docKey, attKey).Scan(&attID); err != nil {
		t.Fatal(err)
	}
	return attID
}

// seedAttachmentForDoc adds another attachment under an existing document
// (by document zotero_key), preferred flag optional.
func (e *retEnv) seedAttachmentForDoc(t *testing.T, docKey, attKey string, preferred bool) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
			parent_zotero_key, link_mode, content_type, filename, local_path, preferred)
		SELECT source_id, id, $2, 1, $1, 'imported_file', 'application/pdf', 'x.pdf', '/tmp/x.pdf', $3
		FROM zotero_documents WHERE zotero_key = $1 RETURNING id::text`, docKey, attKey, preferred).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRetention294IT pins the #294 invariants: the retention keep-rule
// keys on the selection read model's outcome key (updated_at DESC, id
// DESC) PER DOCUMENT, so the row the outcome API reads structurally
// survives regardless of age, attachment liveness or snapshot linkage;
// the plan reports documents with an active snapshot but no job row; and
// the post-apply invariant fires when a snapshot document loses its last
// job row.
//
// Red probe (pre-#294 SQL): the production shape — a NON-preferred
// attachment (preferred moved to a healed replacement) whose doc-latest
// completed job J_A has a later-enqueued, fast-failed sibling J_B and an
// UNLINKED active snapshot (ingest_job_id NULL). The old sibling guard
// (enqueued_at) pruned exactly J_A; the doc flipped to never-processed
// and the sync's (attachment_id, content_hash) dedup found nothing to
// conflict against (J_B, a failed row, carries no hash) → mass requeue.
func TestRetention294IT(t *testing.T) {
	e := openRetDB(t)
	ctx := context.Background()
	age := 14 * 24 * time.Hour

	// ── repro C: the production shape ────────────────────────────────
	// RETDOC7: RETATT7 non-preferred (job history + active snapshot),
	// RETATT8 preferred + jobless (the healed replacement).
	att7 := e.seedDocWithAttachment(t, "RETDOC7", "RETATT7")
	_ = e.seedAttachmentForDoc(t, "RETDOC7", "RETATT8", true)
	var doc7 string
	if err := e.pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE zotero_key='RETDOC7'`).Scan(&doc7); err != nil {
		t.Fatal(err)
	}
	// J_A: completed, enqueued 480h ago, updated 456h ago (long run —
	// the doc's latest by the read-model key). J_B: failed fast,
	// enqueued 457h ago (LATER enqueue), updated 457h ago (EARLIER
	// update) — the newer-enqueued older-attempt sibling, content_hash
	// NULL (failed rows carry no hash — the production shape that left
	// the sync's dedup with nothing to conflict against).
	jA := e.seedJobForAttachment(t, "RETATT7", "completed", 480*time.Hour, 456*time.Hour)
	jB := e.seedFailedJobNullHash(t, "RETATT7", 457*time.Hour)
	// active snapshot on RETATT7, UNLINKED (ingest_job_id NULL) — the
	// prod rows whose producer link never existed
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'h-prod', 'p', 'v1', 'ph', $2::uuid, '{}', true)`, att7, doc7); err != nil {
		t.Fatal(err)
	}
	// informational line probe: a SECOND document with an active snapshot
	// and NO job rows at all — the production damage shape (90 docs)
	att9 := e.seedDocWithAttachment(t, "RETDOC9", "RETATT9")
	var doc9 string
	if err := e.pool.QueryRow(ctx,
		`SELECT id::text FROM zotero_documents WHERE zotero_key='RETDOC9'`).Scan(&doc9); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO processing_snapshots (attachment_id, content_hash, processor_name, processor_version,
			profile_hash, document_id, profile, active)
		VALUES ($1::uuid, 'h-9', 'p', 'v1', 'ph', $2::uuid, '{}', true)`, att9, doc9); err != nil {
		t.Fatal(err)
	}

	// pre-state of the invariant: both snapshot docs carry jobs (RETDOC9
	// does NOT — it must never be reported as a violation by the apply;
	// it lost nothing)
	before, err := e.rep.snapshotDocsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("invariant pre-state = %v, want exactly RETDOC7 (RETDOC9 has no job row to lose)", before)
	}

	// dry-run: exactly J_B is prunable; J_A (doc-latest by the outcome
	// key, on a non-preferred attachment, aged, snapshot unlinked) is
	// NOT; the informational line counts RETDOC9.
	plan, err := e.rep.RetentionPlan(ctx, age)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Jobs.Remove != 1 {
		t.Fatalf("jobs to remove = %d, want 1 (only J_B, the older attempt by the outcome key)", plan.Jobs.Remove)
	}
	if plan.Jobs.SnapshotDocsNoJobRow != 1 {
		t.Fatalf("snapshot docs without job row = %d, want 1 (RETDOC9)", plan.Jobs.SnapshotDocsNoJobRow)
	}

	// apply: J_B goes, J_A survives — the doc with a snapshot keeps its
	// latest job row; the invariant check passes inside the apply.
	rem, _, err := e.rep.ApplyRetention(ctx, age, 0, nil)
	if err != nil {
		t.Fatalf("apply must hold the invariant (J_A survives): %v", err)
	}
	if rem.Jobs != 1 {
		t.Fatalf("removals = %d, want 1", rem.Jobs)
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, jA); n != 1 {
		t.Fatalf("doc-latest job J_A was pruned — outcome truth lost")
	}
	if n := e.count(t, `SELECT count(*) FROM ingest_jobs WHERE id=$1::uuid`, jB); n != 0 {
		t.Fatalf("older attempt J_B must be pruned")
	}

	// ── firing path: hand-built violation ───────────────────────────
	// the pre-state said RETDOC7 has a job row; remove it out-of-band
	// and the checker must fire loudly.
	if _, err := e.pool.Exec(ctx, `DELETE FROM ingest_jobs WHERE id=$1::uuid`, jA); err != nil {
		t.Fatal(err)
	}
	if err := e.rep.checkSnapshotDocInvariant(ctx, before); err == nil {
		t.Fatal("invariant check must fire when a snapshot document lost its last job row")
	}
}

// seedFailedJobNullHash inserts a terminal failed job row with content_hash
// NULL — the writeJobsTx failed-path shape (failed rows carry no hash), the
// reason the sync's (attachment_id, content_hash) dedup found nothing to
// conflict against in the #294 production storm.
func (e *retEnv) seedFailedJobNullHash(t *testing.T, attKey string, age time.Duration) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO ingest_jobs (status, attachment_id, content_hash, enqueued_at, updated_at, error_code, error_message)
		SELECT 'failed', a.id, NULL, now() - make_interval(secs => $1), now() - make_interval(secs => $1), 'X', 'failed fast'
		FROM zotero_attachments a WHERE a.zotero_key = $2 RETURNING id::text`, age.Seconds(), attKey).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
