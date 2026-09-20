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
//     non-deleted attachment, plus (#294) every document's newest job BY
//     THAT SAME KEY regardless of age, attachment liveness or snapshot
//     linkage — a prune can never flip a document's derived outcome, and
//     a document can never lose its last job row to age alone;
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
// phase's planned removal count from the initial plan (never smaller
// than done). artifactPaths carries the durable-artifact files of the
// tranche's DELETED rows — the caller unlinks them right after the
// commit, so an aborted run cannot leak the bytes of tranches it
// already committed (#290 review: end-of-run unlink loses them on every
// abort path).
type RetentionProgress func(phase string, done, total int, artifactPaths []string)

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
		// SnapshotDocsNoJobRow (#294) is the informational reconciliation
		// line: documents that hold an ACTIVE snapshot but no job row at
		// all. The retention bug's production footprint (90 fully processed
		// documents re-enqueued). Read-only signal for the operator — the
		// sync-side snapshot defense keeps such documents served without
		// pointless reprocessing; the count does not gate anything here.
		SnapshotDocsNoJobRow int `json:"snapshot_docs_without_job_row"`
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
// age, and NOT the outcome truth. The outcome truth is layered:
//
//  1. the NEWEST job row (updated_at DESC, id DESC — exactly the
//     selection read model's lateral join) of the PREFERRED, non-deleted
//     attachment;
//  2. (#294) the newest job row BY THAT SAME KEY of the DOCUMENT,
//     across ALL its attachments, REGARDLESS of age, preferred/deleted
//     state or snapshot linkage — "latest job" IS the outcome-truth
//     record (#252 semantics); age only prunes OLDER attempts.
//
// #294 production evidence: the previous sibling guard keyed on
// enqueued_at ("prune only when a newer-enqueued attempt exists"), which
// protects the newest ENQUEUE, not the read model's outcome row — a
// completed job with a late updated_at plus a later-enqueued, fast-failed
// sibling lost its row (and with NULL-hash failed rows left over, the
// sync's (attachment_id, content_hash) dedup found nothing to conflict
// with → mass requeue of fully processed documents). The guard now uses
// the read model's exact key on BOTH layers, so the row the outcome API
// reads is structurally the row that survives.
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
	                AND (j2.updated_at, j2.id) > (j.updated_at, j.id))
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
		{&rep.Jobs.SnapshotDocsNoJobRow, false, `
	FROM processing_snapshots s
	WHERE s.active
	  AND NOT EXISTS (SELECT 1 FROM ingest_jobs j
	                  JOIN zotero_attachments a ON a.id = j.attachment_id
	                  WHERE a.document_id = s.document_id)`},
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
// of cascade, and every DELETE re-evaluates the keep-guards itself — a
// row that de-qualifies between a tranche's SELECT and its DELETE
// survives, preserving the #281 protection level. Idempotent by
// construction — every candidate re-qualifies through the same guards on
// the next run, so an interruption at any tranche boundary leaves
// committed partial progress that a re-run resumes naturally. batchSize
// <= 0 means RetentionBatchDefault.
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
	// #294 pre-state for the post-apply invariant: every document that
	// holds an active snapshot AND at least one job row. The apply must
	// never leave any of these with zero job rows ("latest job IS the
	// outcome-truth record" — losing it flips the document back to
	// never-processed for the sync).
	invBefore, err := r.snapshotDocsWithJobs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("retention invariant pre-state: %w", err)
	}
	rem := &RetentionRemovals{}
	// Tranche loop: re-select the candidate set every iteration — deleted
	// rows are gone, so each tranche picks the next batch without OFFSET
	// bookkeeping. The DELETE re-checks the keep-guards itself (#290
	// review: a row that flips between SELECT and DELETE — reactivated
	// snapshot, new outbox/repair link — must survive, exactly as it did
	// pre-#290 when the guard lived inside the one DELETE). A tranche that
	// SELECTED candidates but deleted none therefore does NOT end the
	// phase: the picks were raced away or de-qualified — re-select. Only a
	// candidate-free SELECT ends the phase, and ctx is checked at every
	// boundary so an abort lands BETWEEN transactions: everything
	// committed stays committed (the resume contract), nothing half-open.
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("retention apply interrupted after %d snapshot(s) — committed tranches are safe, re-run resumes: %w", rem.Snapshots, err)
		}
		picked, deleted, paths, err := r.deleteSnapshotTranche(ctx, batchSize)
		if err != nil {
			return nil, nil, err
		}
		if picked == 0 {
			break
		}
		if deleted > 0 {
			rem.Snapshots += deleted
			rem.ArtifactPaths = append(rem.ArtifactPaths, paths...)
			total := plan.Snapshots.Remove
			if rem.Snapshots > total {
				total = rem.Snapshots // concurrent drift must never print X>Y
			}
			if progress != nil {
				progress("snapshots", rem.Snapshots, total, paths)
			}
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("retention apply interrupted after %d job attempt(s) — committed tranches are safe, re-run resumes: %w", rem.Jobs, err)
		}
		picked, deleted, err := r.deleteJobTranche(ctx, batchSize, jobMinAge)
		if err != nil {
			return nil, nil, err
		}
		if picked == 0 {
			break
		}
		if deleted > 0 {
			rem.Jobs += deleted
			total := plan.Jobs.Remove
			if rem.Jobs > total {
				total = rem.Jobs
			}
			if progress != nil {
				progress("jobs", rem.Jobs, total, nil)
			}
		}
	}
	// #294 post-apply invariant (self-check): no document with an active
	// snapshot lost its LAST job row. Committed tranches stay committed
	// (the resume contract) — a violation is a loud error naming the docs,
	// never a silent flip to never-processed.
	if err := r.checkSnapshotDocInvariant(ctx, invBefore); err != nil {
		return rem, plan, err
	}
	return rem, plan, nil
}

// snapshotDocsWithJobs lists documents that hold an active snapshot and at
// least one job row (any attachment) — the protected set of the #294
// post-apply invariant.
func (r *Repo) snapshotDocsWithJobs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT s.document_id::text
		FROM processing_snapshots s
		WHERE s.active
		  AND EXISTS (SELECT 1 FROM ingest_jobs j
			          JOIN zotero_attachments a ON a.id = j.attachment_id
			          WHERE a.document_id = s.document_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// checkSnapshotDocInvariant asserts that every document from the before
// set still owns at least one job row. A method (not inlined) so the IT
// suite can exercise the firing path against a hand-built violation.
func (r *Repo) checkSnapshotDocInvariant(ctx context.Context, beforeDocs []string) error {
	if len(beforeDocs) == 0 {
		return nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT d::text FROM unnest($1::uuid[]) AS d
		WHERE NOT EXISTS (SELECT 1 FROM ingest_jobs j
			          JOIN zotero_attachments a ON a.id = j.attachment_id
			          WHERE a.document_id = d)`, beforeDocs)
	if err != nil {
		return fmt.Errorf("retention invariant check: %w", err)
	}
	defer rows.Close()
	var lost []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return err
		}
		lost = append(lost, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lost) > 0 {
		shown := lost
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return fmt.Errorf("retention post-apply invariant violated: %d document(s) with an active snapshot lost their last job row (e.g. %v) — committed tranches stay committed; investigate before the next sync", len(lost), shown)
	}
	return nil
}

// deleteSnapshotTranche deletes up to limit superseded snapshots in ONE
// short transaction and commits it. Returns picked (candidates the SELECT
// saw — 0 ends the phase), deleted (rows the DELETE actually removed —
// the keep-guards are re-checked INSIDE the DELETE, so a row that
// qualified at SELECT time but flipped before the DELETE survives, and
// its artifact paths are then excluded) and the artifact paths of exactly
// the DELETED rows, collected inside the same transaction before the
// rows cascade away.
func (r *Repo) deleteSnapshotTranche(ctx context.Context, limit int) (picked, deleted int, artifactPaths []string, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, 0, nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT s.id FROM processing_snapshots s
		WHERE `+supersededSnapshotGuard+`
		ORDER BY s.created_at, s.id
		LIMIT $1
		FOR UPDATE`, limit)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("select snapshot tranche: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	picked = len(ids)
	if picked == 0 {
		return 0, 0, nil, nil
	}
	// artifact paths per candidate snapshot, BEFORE the cascade
	pathsBySnap := make(map[string][]string)
	rows, err = tx.Query(ctx, `
		SELECT ar.snapshot_id::text, ar.storage_path FROM processing_artifacts ar
		WHERE ar.snapshot_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("collect artifact paths: %w", err)
	}
	for rows.Next() {
		var snap, p string
		if err := rows.Scan(&snap, &p); err != nil {
			rows.Close()
			return 0, 0, nil, err
		}
		pathsBySnap[snap] = append(pathsBySnap[snap], p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	// The candidates are locked FOR UPDATE above: a writer flipping a
	// guard input on one of these rows blocks until this transaction ends,
	// so the row cannot de-qualify between selection and DELETE — and a
	// flip committed while we WAITED for the lock is visible to this
	// statement's fresh snapshot (the re-checked qual drops the row from
	// the candidate set entirely). The guard STAYS in the DELETE as well
	// (#290 review blocker): other-table guard inputs — a new outbox or
	// repair row committed after the selection — are caught here, where a
	// blind id-only DELETE would destroy them. RETURNING says WHICH rows
	// went, so survivor artifact paths never reach the caller.
	rows, err = tx.Query(ctx, `
		DELETE FROM processing_snapshots s
		WHERE s.id = ANY($1::uuid[]) AND `+supersededSnapshotGuard+`
		RETURNING s.id::text`, ids)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("delete superseded snapshots: %w", err)
	}
	var deletedIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, nil, err
		}
		deletedIDs = append(deletedIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, nil, err
	}
	for _, id := range deletedIDs {
		artifactPaths = append(artifactPaths, pathsBySnap[id]...)
	}
	return picked, len(deletedIDs), artifactPaths, nil
}

// deleteJobTranche deletes up to limit stale job attempts in ONE short
// transaction and commits it. Same guard-in-DELETE discipline as the
// snapshot tranche: a job that flipped since the SELECT (e.g. status
// pending) survives. Deleting a prunable job never un-prunes a remaining
// one: every candidate's sibling guard is anchored on its document's
// latest job by the read-model key (updated_at, id), which by definition
// has no greater sibling and is therefore never in the prunable set.
func (r *Repo) deleteJobTranche(ctx context.Context, limit int, jobMinAge time.Duration) (picked, deleted int, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	// prunableJobSQL binds the age as $1 (its own numbering); the tranche
	// LIMIT follows it as $2. FOR UPDATE locks the candidates so a guard
	// input cannot flip between selection and DELETE.
	rows, err := tx.Query(ctx, `SELECT j.id`+prunableJobSQL+` ORDER BY j.enqueued_at, j.id LIMIT $2 FOR UPDATE`,
		jobMinAge.Seconds(), limit)
	if err != nil {
		return 0, 0, fmt.Errorf("select job tranche: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	picked = len(ids)
	if picked == 0 {
		return 0, 0, nil
	}
	// the age placeholder inside prunableJobSQL is the FIRST $n in this
	// statement's text, so the id array becomes $2; the guard re-check in
	// the DELETE catches other-table flips (e.g. a repair case) committed
	// after the locked selection
	tag, err := tx.Exec(ctx, `
		DELETE FROM ingest_jobs j
		WHERE j.id IN (SELECT j.id`+prunableJobSQL+`)
		  AND j.id = ANY($2::uuid[])`, jobMinAge.Seconds(), ids)
	if err != nil {
		return 0, 0, fmt.Errorf("delete stale job attempts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return picked, int(tag.RowsAffected()), nil
}
