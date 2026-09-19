package repo

// retention.go — #281: the retention/GC maintenance path.
//
// An operator (or a schedule) runs it; nothing here ever deletes without
// an explicit invocation, and the default is the DRY RUN (report only).
// Three problem classes from the production evidence (2026-09-15/16):
//
//  1. superseded snapshots keep their chunks/embeddings/relationships
//     forever — dead weight and an analysis trap (a measurement summed
//     over ALL snapshots instead of the latest: 252 = 127 stale + 125
//     current);
//  2. historical job attempts accumulate per document — only the latest
//     job per document carries decision-relevant state;
//  3. derived outcome numbers alarm over stale attachment states.
//
// Never-delete list (pinned by IT):
//   - the OUTCOME-TRUTH job: the newest job row (updated_at DESC, id DESC
//     — the selection read model's exact key) of the PREFERRED,
//     non-deleted attachment, plus every document's newest job as defense
//     in depth — a prune can never flip a document's derived outcome;
//   - every job whose attachment has ANY repair case (heal forensics);
//   - every job that produced an ACTIVE snapshot;
//   - every non-terminal job (pending/claimed/processing);
//   - superseded snapshots with PENDING outbox rows (their OpenSearch
//     delete must still drain — the outbox is the only place the stale
//     index docs get removed from).

import (
	"context"
	"fmt"
	"time"
)

// RetentionJobMinAgeDefault is the default age beyond which terminal job
// attempts become prunable (the issue's example: 14 days).
const RetentionJobMinAgeDefault = 14 * 24 * time.Hour

// RetentionBatchDefault is the default tranche size for ApplyRetention:
// snapshots (and job attempts) deleted per transaction, with a commit
// between tranches (#290: the first production apply's single 40-minute
// DELETE was black-holed by the host<->VM port-forward and rolled back
// silently). Sized from that evidence — ~13 s per snapshot cascade → 25
// ≈ 5 min per transaction, the "no transaction longer than a few
// minutes" bound; override per run via --batch / AXIOM_RETENTION_BATCH.
const RetentionBatchDefault = 25

// RetentionProgress is called once per committed tranche so long runs are
// observable from the outside (#290: WAL-delta forensics must never again
// be the only way to tell a healthy apply from a hung one). phase is
// "snapshots" or "jobs"; done is the phase total so far, total the
// phase's planned removal count from the initial plan.
type RetentionProgress func(phase string, done, total int)

// RetentionReport is the dry-run result: exact counts of what a real run
// WOULD remove (and what it never touches).
type RetentionReport struct {
	JobMinAge time.Duration `json:"job_min_age"`
	Snapshots struct {
		Remove              int `json:"remove"`
		KeepActive          int `json:"keep_active"`
		KeepOutboxHeld      int `json:"keep_outbox_held"`
		Chunks              int `json:"chunks_remove"`
		DenseEmbeddings     int `json:"dense_embeddings_remove"`
		SparseEmbeddings    int `json:"sparse_embeddings_remove"`
		Entities            int `json:"entities_remove"`
		EntityMentions      int `json:"entity_mentions_remove"`
		ChunkRelationships  int `json:"chunk_relationships_remove"`
		EntityRelationships int `json:"entity_relationships_remove"`
		Artifacts           int `json:"artifacts_remove"`
	} `json:"snapshots"`
	Jobs struct {
		Remove           int `json:"remove"`
		KeepLatest       int `json:"keep_latest_per_document"`
		KeepRepairLinked int `json:"keep_repair_linked"`
		KeepActiveSnap   int `json:"keep_active_snapshot_ref"`
		KeepNonTerminal  int `json:"keep_non_terminal"`
	} `json:"jobs"`
	// Attachments carries the informational counter reconciliation: open
	// cases per attachment counter state. Report-only (the attempts
	// counter is loop-guard truth and is never rewritten here).
	Attachments struct {
		WithRepairAttempts int `json:"with_repair_attempts"`
		OpenRepairCases    int `json:"open_repair_cases"`
	} `json:"attachments"`
}

// supersededSnapshotGuard is THE one definition of "superseded and safe to
// delete": not active, and no still-relevant outbox row. PENDING delete-ops
// must survive to drain OpenSearch; terminal FAILED delete-ops are
// operator-recoverable (outbox.go's recovery contract) — deleting their
// snapshot would make the stale index docs permanent. Every plan step and
// the ApplyRetention DELETE build on this fragment so they cannot drift.
const supersededSnapshotGuard = `NOT s.active
	  AND NOT EXISTS (SELECT 1 FROM opensearch_outbox o
	                  WHERE o.snapshot_id = s.id AND o.status IN ('pending','failed'))`

// supersededSnapshotSQL is the candidate set FROM/WHERE for counting.
const supersededSnapshotSQL = "\n\tFROM processing_snapshots s\n\tWHERE " + supersededSnapshotGuard

// prunableJobSQL is the candidate set: terminal, older than the retention
// age, and NOT the outcome truth. The outcome truth is the NEWEST job row
// (updated_at DESC, id DESC — exactly the selection read model's lateral
// join) of the PREFERRED, non-deleted attachment; pruning any other row
// cannot flip a document's derived outcome. A document-level newest-
// sibling guard (strictly newer job on any attachment of the document)
// stays as defense in depth.
const prunableJobSQL = `
	FROM ingest_jobs j
	JOIN zotero_attachments a ON a.id = j.attachment_id
	WHERE j.status IN ('completed','failed','cancelled','skipped')
	  AND j.enqueued_at < now() - make_interval(secs => $1)
	  AND NOT (
			a.preferred AND NOT a.deleted
			AND NOT EXISTS (SELECT 1 FROM ingest_jobs j2
			                WHERE j2.attachment_id = a.id
			                  AND (j2.updated_at, j2.id) > (j.updated_at, j.id)))
	  AND EXISTS (SELECT 1 FROM ingest_jobs j2
	              JOIN zotero_attachments a2 ON a2.id = j2.attachment_id
	              WHERE a2.document_id = a.document_id
	                AND j2.enqueued_at > j.enqueued_at)
	  AND NOT EXISTS (SELECT 1 FROM repair_cases rc WHERE rc.attachment_id = j.attachment_id)
	  AND NOT EXISTS (SELECT 1 FROM processing_snapshots s
	                  WHERE s.ingest_job_id = j.id AND s.active)`

// RetentionPlan computes the report (dry run). Read-only.
func (r *Repo) RetentionPlan(ctx context.Context, jobMinAge time.Duration) (*RetentionReport, error) {
	if jobMinAge <= 0 {
		jobMinAge = RetentionJobMinAgeDefault
	}
	rep := &RetentionReport{JobMinAge: jobMinAge}
	count := func(dst *int, withAge bool, sql string) error {
		var err error
		if withAge {
			err = r.pool.QueryRow(ctx, "SELECT count(*)"+sql, jobMinAge.Seconds()).Scan(dst)
		} else {
			err = r.pool.QueryRow(ctx, "SELECT count(*)"+sql).Scan(dst)
		}
		if err != nil {
			return fmt.Errorf("count: %w", err)
		}
		return nil
	}
	// withAge marks the job-scoped queries that bound by the retention age.
	steps := []struct {
		dst     *int
		withAge bool
		sql     string
	}{
		{&rep.Snapshots.Remove, false, supersededSnapshotSQL},
		{&rep.Snapshots.KeepActive, false, " FROM processing_snapshots s WHERE s.active"},
		{&rep.Snapshots.KeepOutboxHeld, false, `
	FROM processing_snapshots s
	WHERE NOT s.active
	  AND EXISTS (SELECT 1 FROM opensearch_outbox o
	              WHERE o.snapshot_id = s.id AND o.status IN ('pending','failed'))`},
		{&rep.Snapshots.Chunks, false, `
	FROM processing_chunks c JOIN processing_snapshots s ON s.id = c.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.DenseEmbeddings, false, `
	FROM processing_chunk_dense_embeddings e
	JOIN processing_chunks c ON c.id = e.chunk_id
	JOIN processing_snapshots s ON s.id = c.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.SparseEmbeddings, false, `
	FROM processing_chunk_sparse_embeddings e
	JOIN processing_chunks c ON c.id = e.chunk_id
	JOIN processing_snapshots s ON s.id = c.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.Entities, false, `
	FROM processing_entities e JOIN processing_snapshots s ON s.id = e.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.EntityMentions, false, `
	FROM processing_entity_mentions m
	JOIN processing_entities e ON e.id = m.entity_id
	JOIN processing_snapshots s ON s.id = e.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.ChunkRelationships, false, `
	FROM processing_chunk_relationships cr JOIN processing_snapshots s ON s.id = cr.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.EntityRelationships, false, `
	FROM processing_entity_relationships er JOIN processing_snapshots s ON s.id = er.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Snapshots.Artifacts, false, `
	FROM processing_artifacts ar JOIN processing_snapshots s ON s.id = ar.snapshot_id
	WHERE ` + supersededSnapshotGuard},
		{&rep.Jobs.Remove, true, prunableJobSQL},
		{&rep.Jobs.KeepNonTerminal, false, " FROM ingest_jobs WHERE status IN ('pending','claimed','processing')"},
		{&rep.Jobs.KeepLatest, true, `
	FROM ingest_jobs j
	JOIN zotero_attachments a ON a.id = j.attachment_id
	WHERE j.status IN ('completed','failed','cancelled','skipped')
	  AND j.enqueued_at < now() - make_interval(secs => $1)
	  AND a.preferred AND NOT a.deleted
	  AND NOT EXISTS (SELECT 1 FROM ingest_jobs j2
	                  WHERE j2.attachment_id = a.id
	                    AND (j2.updated_at, j2.id) > (j.updated_at, j.id))
	  AND NOT EXISTS (SELECT 1 FROM repair_cases rc WHERE rc.attachment_id = j.attachment_id)
	  AND NOT EXISTS (SELECT 1 FROM processing_snapshots s
	                  WHERE s.ingest_job_id = j.id AND s.active)`},
		{&rep.Jobs.KeepRepairLinked, true, `
	FROM ingest_jobs j
	WHERE j.status IN ('completed','failed','cancelled','skipped')
	  AND j.enqueued_at < now() - make_interval(secs => $1)
	  AND EXISTS (SELECT 1 FROM repair_cases rc WHERE rc.attachment_id = j.attachment_id)`},
		{&rep.Jobs.KeepActiveSnap, true, `
	FROM ingest_jobs j
	WHERE j.status IN ('completed','failed','cancelled','skipped')
	  AND j.enqueued_at < now() - make_interval(secs => $1)
	  AND EXISTS (SELECT 1 FROM processing_snapshots s
	              WHERE s.ingest_job_id = j.id AND s.active)`},
		{&rep.Attachments.WithRepairAttempts, false, " FROM zotero_attachments WHERE repair_attempts > 0"},
		{&rep.Attachments.OpenRepairCases, false, " FROM repair_cases WHERE status IN ('rejected','queued','in_repair')"},
	}
	for _, st := range steps {
		if err := count(st.dst, st.withAge, st.sql); err != nil {
			return nil, fmt.Errorf("retention plan: %w", err)
		}
	}
	return rep, nil
}

// RetentionRemovals is what one real run deleted.
type RetentionRemovals struct {
	Snapshots int `json:"snapshots_removed"`
	Jobs      int `json:"jobs_removed"`
	// ArtifactPaths are the durable-artifact files of the deleted
	// snapshots (#270 review: rows cascade but the BYTES under
	// AXIOM_ARTIFACT_ROOT would stay forever). Collected INSIDE the
	// transaction; the caller unlinks them AFTER the commit — a missing
	// file must never roll back row deletion (orphaned rows are the worse
	// evil), and a failed unlink leaves an orphaned file, which the next
	// run re-reports as gone-with-its-snapshot.
	ArtifactPaths []string `json:"artifact_paths"`
}

// ApplyRetention executes the plan in TRANCHES (#290): up to batchSize
// superseded snapshots per transaction (their cascade frees chunks/
// embeddings/relationships/artifacts and done outbox rows), commit, then
// the next tranche; stale job attempts afterwards under the same
// discipline. No single transaction runs longer than one tranche's worth
// of cascade. Idempotent by construction — every candidate re-qualifies
// through the same guards on the next run, so an interruption at any
// tranche boundary leaves committed partial progress that a re-run
// resumes naturally. batchSize <= 0 means RetentionBatchDefault.
func (r *Repo) ApplyRetention(ctx context.Context, jobMinAge time.Duration, batchSize int, progress RetentionProgress) (*RetentionRemovals, *RetentionReport, error) {
	if jobMinAge <= 0 {
		jobMinAge = RetentionJobMinAgeDefault
	}
	if batchSize <= 0 {
		batchSize = RetentionBatchDefault
	}
	plan, err := r.RetentionPlan(ctx, jobMinAge)
	if err != nil {
		return nil, nil, err
	}
	rem := &RetentionRemovals{}
	// Tranche loop: re-select the candidate set every iteration — deleted
	// rows are gone, so each tranche picks the next batch without OFFSET
	// bookkeeping. A zero-row tranche ends the phase. ctx is checked at the
	// boundary so a deadline/cancel aborts BETWEEN transactions: everything
	// committed stays committed (the resume contract), nothing half-open.
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("retention apply interrupted after %d snapshot(s) — committed tranches are safe, re-run resumes: %w", rem.Snapshots, err)
		}
		n, paths, err := r.deleteSnapshotTranche(ctx, batchSize)
		if err != nil {
			return nil, nil, err
		}
		if n == 0 {
			break
		}
		rem.Snapshots += n
		rem.ArtifactPaths = append(rem.ArtifactPaths, paths...)
		if progress != nil {
			progress("snapshots", rem.Snapshots, plan.Snapshots.Remove)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("retention apply interrupted after %d job attempt(s) — committed tranches are safe, re-run resumes: %w", rem.Jobs, err)
		}
		n, err := r.deleteJobTranche(ctx, batchSize, jobMinAge)
		if err != nil {
			return nil, nil, err
		}
		if n == 0 {
			break
		}
		rem.Jobs += n
		if progress != nil {
			progress("jobs", rem.Jobs, plan.Jobs.Remove)
		}
	}
	return rem, plan, nil
}

// deleteSnapshotTranche deletes up to limit superseded snapshots in ONE
// short transaction and commits it. Artifact paths are collected inside
// the same transaction, before the rows cascade away.
func (r *Repo) deleteSnapshotTranche(ctx context.Context, limit int) (int, []string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT s.id FROM processing_snapshots s
		WHERE `+supersededSnapshotGuard+`
		ORDER BY s.created_at, s.id
		LIMIT $1`, limit)
	if err != nil {
		return 0, nil, fmt.Errorf("select snapshot tranche: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if len(ids) == 0 {
		return 0, nil, nil
	}
	var paths []string
	rows, err = tx.Query(ctx, `
		SELECT ar.storage_path FROM processing_artifacts ar
		WHERE ar.snapshot_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, nil, fmt.Errorf("collect artifact paths: %w", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM processing_snapshots s
		WHERE s.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, nil, fmt.Errorf("delete superseded snapshots: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return int(tag.RowsAffected()), paths, nil
}

// deleteJobTranche deletes up to limit stale job attempts in ONE short
// transaction and commits it. Deleting a prunable job never un-prunes a
// remaining one: every candidate's "newer sibling" guard is anchored on
// its document's newest job, which by definition has no newer sibling and
// is therefore never in the prunable set.
func (r *Repo) deleteJobTranche(ctx context.Context, limit int, jobMinAge time.Duration) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// prunableJobSQL binds the age as $1 (its own numbering); the tranche
	// LIMIT follows it as $2.
	rows, err := tx.Query(ctx, `SELECT j.id`+prunableJobSQL+` LIMIT $2`,
		jobMinAge.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("select job tranche: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM ingest_jobs WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, fmt.Errorf("delete stale job attempts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
