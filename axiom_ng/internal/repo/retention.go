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
//   - the LATEST job per document (it IS the outcome truth);
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
// age, NOT the document's latest job (a strictly NEWER sibling exists),
// NOT repair-linked, NOT producing an active snapshot.
const prunableJobSQL = `
	FROM ingest_jobs j
	JOIN zotero_attachments a ON a.id = j.attachment_id
	WHERE j.status IN ('completed','failed','cancelled','skipped')
	  AND j.enqueued_at < now() - make_interval(secs => $1)
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
	  AND NOT EXISTS (SELECT 1 FROM ingest_jobs j2
	                  JOIN zotero_attachments a2 ON a2.id = j2.attachment_id
	                  WHERE a2.document_id = a.document_id
	                    AND j2.enqueued_at > j.enqueued_at)
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
}

// ApplyRetention executes the plan in ONE transaction: superseded snapshots
// first (their cascade frees chunks/embeddings/relationships/artifacts and
// done outbox rows), then the stale job attempts. Idempotent by
// construction — a second run finds nothing left in the candidate sets.
func (r *Repo) ApplyRetention(ctx context.Context, jobMinAge time.Duration) (*RetentionRemovals, *RetentionReport, error) {
	if jobMinAge <= 0 {
		jobMinAge = RetentionJobMinAgeDefault
	}
	plan, err := r.RetentionPlan(ctx, jobMinAge)
	if err != nil {
		return nil, nil, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	var snapN, jobN int
	tag, err := tx.Exec(ctx, `
		DELETE FROM processing_snapshots s
		WHERE `+supersededSnapshotGuard)
	if err != nil {
		return nil, nil, fmt.Errorf("delete superseded snapshots: %w", err)
	}
	snapN = int(tag.RowsAffected())
	tag, err = tx.Exec(ctx, `
		DELETE FROM ingest_jobs j
		WHERE j.id IN (SELECT j.id`+prunableJobSQL+`)`, jobMinAge.Seconds())
	if err != nil {
		return nil, nil, fmt.Errorf("delete stale job attempts: %w", err)
	}
	jobN = int(tag.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return &RetentionRemovals{Snapshots: snapN, Jobs: jobN}, plan, nil
}
