// Lease protocol repository: atomic claim, fenced post-claim mutations and
// retry scheduling for ingest_jobs (Gate 1 of the dispatcher work package).
// axiom-ng owns all durable state; the processor only computes.
//
// Claim, obsolete-terminalization, lease-exhaustion, cancellation-terminalization
// and frozen-input freeze all happen in one short transaction. Every post-claim
// mutation is fenced by job_id + worker_id + lease_token and returns
// ErrLostLease when the caller no longer owns the lease.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxObsoleteSkips bounds how many obsolete/skipped queue-heads one ClaimNextJob
// invocation may drain before yielding, so a burst of stale rows cannot stall
// the poll loop indefinitely. The next poll continues where the bound left off.
const maxObsoleteSkips = 100

// maxRetryDelaySeconds is the documented upper bound for ScheduleRetry backoff;
// larger or negative requests are clamped into [0, maxRetryDelaySeconds].
const maxRetryDelaySeconds = 86400 // 24h

// LeaseRef identifies a claimed job for a fenced mutation. All post-claim
// operations require these three fields.
type LeaseRef struct {
	JobID      string
	WorkerID   string
	LeaseToken string
}

// ErrLostLease is returned when a fenced mutation affects zero rows because the
// caller no longer owns the lease (reclaimed, expired, terminal). The caller
// must stop local state mutation and never persist/acknowledge a processor
// result in response to the original work.
var ErrLostLease = errors.New("lease lost or job not in expected state")

// ClaimOptions supplies the dispatcher-engine parts of a claim: the worker's
// stable id, the lease duration and the processing-profile identity to freeze
// for this job. The immutable input snapshot is built inside the claim
// transaction from the locked source/document/attachment state.
type ClaimOptions struct {
	WorkerID       string
	LeaseDuration  time.Duration
	Profile        json.RawMessage
	ProfileHash    string
	IdempotencyKey string
}

// ClaimedJob is the immutable claim payload returned to a dispatcher so it can
// build a processor request entirely from the frozen snapshot.
type ClaimedJob struct {
	LeaseRef
	Status         string
	Attempt        int
	MaxAttempts    int
	ContentHash    *string
	ForceRebuild   bool
	SourceID       string
	DocumentID     string
	AttachmentID   string
	InputSnapshot  json.RawMessage
	Profile        json.RawMessage
	ProfileHash    *string
	IdempotencyKey *string
}

// execer is the minimal SQL executor shared by pgxpool.Pool and pgx.Tx so the
// fenced-mutation choke point works against both the pool (post-claim autocommit
// transitions) and a caller-owned transaction (MarkCompletedTx).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DefaultLeaseDuration is used when a call omits ClaimOptions.LeaseDuration;
// conservative and replaced by real config in the dispatcher (Gate 2).
const DefaultLeaseDuration = 120 * time.Second

// ClaimNextJob atomically claims the oldest eligible ingest job for the worker,
// terminalizing stale jobs first, all in one transaction.
//
// The one transaction:
//  1. Terminalizes cancel-requested pending jobs (->cancelled) and expired
//     claimed/processing jobs at attempt exhaustion (->failed/LEASE_EXHAUSTED)
//     and expired cancel-requested jobs (->cancelled).
//  2. Scans for a claimable candidate with FOR UPDATE SKIP LOCKED (FIFO),
//     skipping obsolete candidates (deleted/unpreferred/hash-stale) by marking
//     them skipped in-tx.
//  3. Freezes the immutable input snapshot (built from the locked current
//     source/document/attachment state) plus the processing profile and
//     idempotency key if not already frozen (reclaims keep stored values).
//  4. Increments attempt, assigns a fresh lease_token and owner, and commits.
//
// Returns (nil, nil) when nothing is claimable now. Errors are transactional:
// any failure rolls back the whole pass including all skips/terminalizations.
func (r *Repo) ClaimNextJob(ctx context.Context, opts ClaimOptions) (*ClaimedJob, error) {
	if opts.WorkerID == "" {
		return nil, errors.New("claim: worker id is required")
	}
	leaseDur := opts.LeaseDuration
	if leaseDur <= 0 {
		leaseDur = DefaultLeaseDuration
	}
	leaseSec := int(leaseDur.Seconds())

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := r.terminalizeStale(ctx, tx); err != nil {
		return nil, err
	}

	skipped := 0
	var excludedID *string
	for {
		cand, err := claimCandidate(ctx, tx, excludedID)
		if err != nil {
			return nil, err
		}
		if cand == nil {
			break // nothing more claimable this pass
		}

		// Obsolete: terminalize with a stable reason and continue scanning the
		// queue; when the run hits the skip bound, commit the batch and yield so
		// the next poll continues draining.
		if reason := obsoleteReason(cand); reason != "" {
			if err := r.markObsolete(ctx, tx, cand.id, reason); err != nil {
				return nil, err
			}
			skipped++
			id := cand.id
			excludedID = &id
			if skipped >= maxObsoleteSkips {
				break
			}
			continue
		}

		// Build the immutable snapshot from the locked candidate state and
		// claim, freezing it (COALESCE) so a reclaim never overwrites stored
		// input.
		snapshot := buildSnapshot(cand)
		var newAttempt int
		var tsToken string
		err = tx.QueryRow(ctx, `
			UPDATE ingest_jobs SET
				status            = 'claimed',
				claimed_by        = $2,
				lease_token       = gen_random_uuid(),
				attempt           = attempt + 1,
				lease_until       = now() + make_interval(secs => $3),
				last_heartbeat_at = now(),
				started_at        = COALESCE(started_at, now()),
				input_snapshot    = COALESCE(input_snapshot, $4::jsonb),
				processing_profile= COALESCE(processing_profile, $5::jsonb),
				profile_hash      = COALESCE(profile_hash, $6),
				idempotency_key   = COALESCE(idempotency_key, $7),
				updated_at        = now()
			WHERE id = $1
			RETURNING attempt, lease_token::text
		`, cand.id, opts.WorkerID, leaseSec, snapshot, opts.Profile, opts.ProfileHash, opts.IdempotencyKey).
			Scan(&newAttempt, &tsToken)
		if err != nil {
			return nil, fmt.Errorf("claim update: %w", err)
		}

		inputSnapshot, profile, profileHash, idemKey, err := frozenFields(ctx, tx, cand.id)
		if err != nil {
			return nil, err
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &ClaimedJob{
			LeaseRef:       LeaseRef{JobID: cand.id, WorkerID: opts.WorkerID, LeaseToken: tsToken},
			Status:         "claimed",
			Attempt:        newAttempt,
			MaxAttempts:    cand.maxAttempts,
			ContentHash:    cand.contentHash,
			ForceRebuild:   cand.forceRebuild,
			SourceID:       cand.sourceID,
			DocumentID:     cand.documentID,
			AttachmentID:   cand.attachmentID,
			InputSnapshot:  inputSnapshot,
			Profile:        profile,
			ProfileHash:    profileHash,
			IdempotencyKey: idemKey,
		}, nil
	}

	// Nothing claimable: commit the batch of terminalizations/skips done in this
	// pass so they are durable, then signal the caller to poll again.
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

// terminalizeStale marks jobs that cannot proceed as terminal, exactly once, in
// the caller's transaction:
//   - pending + cancel-requested            -> cancelled
//   - claimed/processing + cancel-requested + lease expired -> cancelled
//   - claimed/processing + lease expired + attempt >= max    -> failed LEASE_EXHAUSTED
//
// These rows are only ever terminal here; a concurrent renewed/reclaimed job
// still has a future lease_until, so the expiry filter keeps it untouched.
func (r *Repo) terminalizeStale(ctx context.Context, tx pgx.Tx) error {
	stmts := []string{
		`UPDATE ingest_jobs SET status='cancelled', claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=now(), updated_at=now()
		 WHERE status='pending' AND cancel_requested_at IS NOT NULL`,
		`UPDATE ingest_jobs SET status='cancelled', claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status IN ('claimed','processing') AND cancel_requested_at IS NOT NULL
		   AND lease_until IS NOT NULL AND lease_until <= now()`,
		`UPDATE ingest_jobs SET status='failed', error_code='LEASE_EXHAUSTED',
		          error_message='attempt limit reached',
		          claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status IN ('claimed','processing') AND cancel_requested_at IS NULL
		   AND lease_until IS NOT NULL AND lease_until <= now()
		   AND attempt >= max_attempts`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("terminalize stale: %w", err)
		}
	}
	return nil
}

// claimCandidate locks and returns the oldest still-claimable job, excluding a
// just-skipped id so a burst of obsolete jobs is drained in one pass. It uses
// FOR UPDATE SKIP LOCKED so concurrent claimers never double-grant. Returns
// (nil, nil) when nothing is claimable.
func claimCandidate(ctx context.Context, tx pgx.Tx, excludedID *string) (*candidate, error) {
	// The exclusion clause is assembled only when a just-skipped id exists. This
	// keeps the first (no-exclusion) claim free of a uuid-typed parameter, which
	// pgx otherwise has trouble binding on the very first prepared execution.
	filter := ""
	args := []any{}
	if excludedID != nil {
		filter = ` AND j.id <> $1`
		args = append(args, *excludedID)
	}
	sql := `
		SELECT j.id, j.attempt, j.max_attempts, j.content_hash, j.force_rebuild,
		       j.source_id::text, j.document_id::text, j.attachment_id::text,
		       COALESCE(a.deleted, true)            AS att_deleted,
		       COALESCE(a.preferred, false)         AS att_preferred,
		       COALESCE(d.deleted, true)            AS doc_deleted,
		       COALESCE(s.id::text, '')             AS src_id,
		       a.content_type, a.filename, a.local_path, a.content_hash AS att_hash,
		       COALESCE(a.file_size,0), COALESCE(a.mtime_ms,0),
		       a.zotero_key AS att_key, d.zotero_key AS doc_key, d.title,
		       CASE WHEN j.force_rebuild THEN NULL ELSE a.content_hash END AS cur_hash
		FROM ingest_jobs j
		LEFT JOIN zotero_attachments a ON a.id = j.attachment_id
		LEFT JOIN zotero_documents d ON d.id = j.document_id
		LEFT JOIN zotero_sources s ON s.id = j.source_id
		WHERE j.status IN ('pending','claimed','processing')
		  AND j.cancel_requested_at IS NULL
		  AND j.attempt < j.max_attempts
		  AND (
		        (j.status='pending' AND (j.next_attempt_at IS NULL OR j.next_attempt_at<=now()))
		     OR (j.status IN ('claimed','processing') AND j.lease_until IS NOT NULL AND j.lease_until<=now())
		  )` + filter + `
		ORDER BY j.enqueued_at ASC
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1`

	c, err := scanCandidate(tx.QueryRow(ctx, sql, args...))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim candidate: %w", err)
	}
	return c, nil
}

type candidate struct {
	id           string
	attempt      int
	maxAttempts  int
	contentHash  *string
	forceRebuild bool
	sourceID     string
	documentID   string
	attachmentID string

	attDeleted   bool
	attPreferred bool
	docDeleted   bool
	srcID        string

	contentType string
	filename    string
	localPath   *string
	attHash     *string
	fileSize    int64
	mtimeMS     int64
	attKey      string
	docKey      string
	title       string
	curHash     *string
}

func scanCandidate(row pgx.Row) (*candidate, error) {
	var c candidate
	err := row.Scan(
		&c.id, &c.attempt, &c.maxAttempts, &c.contentHash, &c.forceRebuild,
		&c.sourceID, &c.documentID, &c.attachmentID,
		&c.attDeleted, &c.attPreferred, &c.docDeleted, &c.srcID,
		&c.contentType, &c.filename, &c.localPath, &c.attHash,
		&c.fileSize, &c.mtimeMS, &c.attKey, &c.docKey, &c.title,
		&c.curHash,
	)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// obsoleteReason returns a stable SKIPPED reason when the locked job can no
// longer be processed, or "" when it is claimable. Hash mismatch only applies
// when the job is not a forced rebuild AND a current attachment hash exists (a
// NULL current hash means no hash baseline, so it is not treated as a mismatch —
// a NULL job hash with a real attachment hash is a mismatch).
func obsoleteReason(c *candidate) string {
	if c.attachmentID == "" || c.srcID == "" || c.documentID == "" {
		return "PARENT_REMOVED"
	}
	if c.attDeleted || c.docDeleted {
		return "ATTACHMENT_REMOVED"
	}
	if !c.attPreferred {
		return "ATTACHMENT_NOT_PREFERRED"
	}
	if !c.forceRebuild {
		switch {
		case c.curHash != nil && (c.contentHash == nil || *c.curHash != *c.contentHash):
			return "CONTENT_HASH_CHANGED"
		case c.curHash == nil && c.contentHash != nil:
			return "CONTENT_HASH_MISSING"
		}
	}
	return ""
}

// buildSnapshot produces the immutable input snapshot from the locked current
// attachment/document/source state. In Gate 1 it captures the processing-relevant
// identifiers and file facts; Gate 2 assembles the full contract request from it.
func buildSnapshot(c *candidate) []byte {
	m := map[string]any{
		"attachment_id":  c.attachmentID,
		"document_id":    c.documentID,
		"source_id":      c.srcID,
		"attachment_key": c.attKey,
		"document_key":   c.docKey,
		"content_type":   c.contentType,
		"filename":       c.filename,
		"local_path":     c.localPath,
		"content_hash":   c.attHash,
		"file_size":      c.fileSize,
		"mtime_ms":       c.mtimeMS,
		"title":          c.title,
		"force_rebuild":  c.forceRebuild,
	}
	b, _ := json.Marshal(m)
	return b
}

// frozenFields reads back the COALESCE-resolved frozen input after a claim so
// both first-claim and reclaim return the actual stored values (a reclaim
// returns the earlier freeze, never the fresh opts).
func frozenFields(ctx context.Context, tx pgx.Tx, jobID string) (json.RawMessage, json.RawMessage, *string, *string, error) {
	var snap, prof []byte
	var ph, ik *string
	err := tx.QueryRow(ctx, `
		SELECT input_snapshot::text, COALESCE(processing_profile::text,''), profile_hash, idempotency_key
		FROM ingest_jobs WHERE id=$1`, jobID).Scan(&snap, &prof, &ph, &ik)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read frozen fields: %w", err)
	}
	return json.RawMessage(snap), json.RawMessage(prof), ph, ik, nil
}

// markObsolete transitions a job to skipped with a stable reason in the caller's
// transaction and clears any lease it held. It is always committed with the
// enclosing claim pass (never left dangling by an early return).
func (r *Repo) markObsolete(ctx context.Context, tx pgx.Tx, jobID, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE ingest_jobs SET
			status='skipped', error_code='SKIPPED', error_message=$2,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1
	`, jobID, reason)
	return err
}

// fencedUpdate runs an UPDATE whose $1,$2,$3 are job_id, claimed_by and
// lease_token (the fencing predicate) and returns whether it affected a row.
// Every post-claim mutation routes through this so a single zero-row guard
// catches a lost lease for all callers. The expected-status filter is written
// as SQL literals in each statement. `ex` may be the pool (autocommit) or a
// caller-owned transaction (MarkCompletedTx).
func (r *Repo) fencedUpdate(ctx context.Context, ex execer, sqlStmt string, ref LeaseRef, args ...any) (bool, error) {
	tag, err := ex.Exec(ctx, sqlStmt,
		append([]any{ref.JobID, ref.WorkerID, ref.LeaseToken}, args...)...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RenewLease extends the lease_until and heartbeat for a claimed job using
// database time. Returns a lost-lease error if the job is no longer owned in an
// expected state.
func (r *Repo) RenewLease(ctx context.Context, ref LeaseRef, duration time.Duration) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			lease_until = now() + make_interval(secs => $4),
			last_heartbeat_at = now(),
			updated_at = now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref, int(duration.Seconds()))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("renew lease: %w", ErrLostLease)
	}
	return nil
}

// MarkProcessing advances a claimed job to processing after processor acceptance.
func (r *Repo) MarkProcessing(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET status='processing', updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3 AND status='claimed'
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark processing: %w", ErrLostLease)
	}
	return nil
}

// ScheduleRetry returns a claimed/processing job to pending for a later retry
// with bounded backoff. The delay is clamped into [0, maxRetryDelaySeconds]; the
// DB stores now()+delay so no sleeps happen inside a transaction or while
// holding a lease.
func (r *Repo) ScheduleRetry(ctx context.Context, ref LeaseRef, errorCode, errorMessage string, nextDelaySeconds int) error {
	if nextDelaySeconds < 0 {
		nextDelaySeconds = 0
	}
	if nextDelaySeconds > maxRetryDelaySeconds {
		nextDelaySeconds = maxRetryDelaySeconds
	}
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='pending',
			next_attempt_at = now() + make_interval(secs => $6),
			error_code=$4, error_message=$5,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref, errorCode, errorMessage, nextDelaySeconds)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("schedule retry: %w", ErrLostLease)
	}
	return nil
}

// MarkFailed sets a terminal failed state, clearing the lease.
func (r *Repo) MarkFailed(ctx context.Context, ref LeaseRef, errorCode, errorMessage string) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='failed', error_code=$4, error_message=$5,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref, errorCode, errorMessage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark failed: %w", ErrLostLease)
	}
	return nil
}

// MarkCompletedTx finalises a job as completed inside a CALLER-OWNED
// transaction, alongside the durable processing-snapshot persistence (Gate 4
// persists the snapshot and completes the job in one commit). It re-fences with
// owner+lease and re-validates the current attachment state (not deleted, still
// preferred, hash still matching unless forced rebuild). A rollback of the
// caller's transaction rolls the completion back with it.
func (r *Repo) MarkCompletedTx(ctx context.Context, tx pgx.Tx, ref LeaseRef, processorName, processorVersion, snapshotID string) error {
	ok, err := r.fencedUpdate(ctx, tx, `
		UPDATE ingest_jobs j SET
			status='completed',
			processor_name=$4, processor_version=$5,
			result=COALESCE(result,'{}')::jsonb || jsonb_build_object('snapshot_id',$6::text),
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE j.id=$1 AND j.claimed_by=$2 AND j.lease_token=$3 AND j.status='processing'
		  AND EXISTS (
		        SELECT 1 FROM zotero_attachments a
		        WHERE a.id = j.attachment_id
		          AND a.deleted = false AND a.preferred = true
		          AND (j.force_rebuild
		               OR (j.content_hash IS NOT NULL AND a.content_hash IS NOT NULL
		                   AND a.content_hash = j.content_hash))
		      )
	`, ref, processorName, processorVersion, snapshotID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark completed: %w", ErrLostLease)
	}
	return nil
}

// MarkCancelled sets a terminal cancelled state, clearing the lease.
func (r *Repo) MarkCancelled(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='cancelled',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark cancelled: %w", ErrLostLease)
	}
	return nil
}

// MarkSkipped sets a terminal skipped state with a reason, clearing the lease.
func (r *Repo) MarkSkipped(ctx context.Context, ref LeaseRef, reason string) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			status='skipped', error_code='SKIPPED', error_message=$4,
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref, reason)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("mark skipped: %w", ErrLostLease)
	}
	return nil
}

// RequestCancellation records a cancellation request on a non-terminal job so
// the dispatcher/recovery converges it to cancelled. It is idempotent and does
// not require ownership (an operator may cancel a job any worker holds). Pending
// jobs are terminalized immediately by the next claim pass.
func (r *Repo) RequestCancellation(ctx context.Context, jobID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE ingest_jobs SET cancel_requested_at=COALESCE(cancel_requested_at, now()), updated_at=now()
		WHERE id=$1 AND status NOT IN ('completed','failed','cancelled','skipped')
	`, jobID)
	return err
}

// ReleaseOrExpireLease clears the lease fields for a job owned by ref, returning
// it to pending so it is claimable again. Used by graceful shutdown so in-flight
// work is not lost after restart.
func (r *Repo) ReleaseOrExpireLease(ctx context.Context, ref LeaseRef) error {
	ok, err := r.fencedUpdate(ctx, r.pool, `
		UPDATE ingest_jobs SET
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			status='pending',
			next_attempt_at=now(),
			updated_at=now()
		WHERE id=$1 AND claimed_by=$2 AND lease_token=$3
		  AND status IN ('claimed','processing')
	`, ref)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("release lease: %w", ErrLostLease)
	}
	return nil
}

// GetJob returns the full current row for a job.
func (r *Repo) GetJob(ctx context.Context, jobID string) (*Job, error) {
	var j Job
	err := r.pool.QueryRow(ctx, `
		SELECT id::text, source_id::text, document_id::text, attachment_id::text,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text
		FROM ingest_jobs WHERE id=$1
	`, jobID).Scan(&j.ID, &j.SourceID, &j.DocumentID, &j.AttachmentID,
		&j.Status, &j.ContentHash, &j.Attempt, &j.MaxAttempts,
		&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("get job %s: %w", jobID, pgx.ErrNoRows)
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// RecoverExpiredJobs reclaims expired claimed/processing jobs synchronously and
// atomically, distinguishing the three outcomes in ONE transaction with row
// locking so it never clobbers a parallel renew/reclaim/completion:
//
//   - cancel-requested now (lease expired)     -> cancelled
//   - attempt >= max_attempts now               -> failed LEASE_EXHAUSTED
//   - otherwise (reclaimable)                   -> pending, due now
//
// Because FOR UPDATE SKIP LOCKED pins each row, a concurrent claimer that took
// the row first is simply skipped here; a renewed lease no longer matches the
// expiry filter.
func (r *Repo) RecoverExpiredJobs(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id FROM ingest_jobs
		WHERE status IN ('claimed','processing')
		  AND lease_until IS NOT NULL AND lease_until <= now()
		ORDER BY enqueued_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT $1
	`, limit)
	if err != nil {
		return 0, err
	}
	ids := []string{}
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
		// Nothing to recover; commit the (empty) transaction.
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Per-row classified update. The row is still locked in this transaction, so
	// the three statements below cannot race a concurrent claimer; each one is
	// fully guarded by its own WHERE.
	cancelled := 0
	exhausted := 0
	recovered := 0
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `UPDATE ingest_jobs SET status='cancelled',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NOT NULL`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			cancelled++
			continue
		}
		tag, err = tx.Exec(ctx, `UPDATE ingest_jobs SET status='failed',
			error_code='LEASE_EXHAUSTED', error_message='attempt limit reached',
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			completed_at=COALESCE(completed_at, now()), updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NULL AND attempt >= max_attempts`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			exhausted++
			continue
		}
		tag, err = tx.Exec(ctx, `UPDATE ingest_jobs SET status='pending',
			next_attempt_at=now(),
			claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			updated_at=now()
			WHERE id=$1 AND status IN ('claimed','processing')
			  AND cancel_requested_at IS NULL AND attempt < max_attempts`, id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			recovered++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return recovered, nil
}
