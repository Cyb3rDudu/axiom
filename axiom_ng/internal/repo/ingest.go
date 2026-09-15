package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PendingJob describes a processing unit the sync layer wants to enqueue.
type PendingJob struct {
	SourceID     string
	DocumentID   string
	AttachmentID string
	ContentHash  string
	ForceRebuild bool
}

// Job is a row of ingest_jobs. The FK projections are nullable because a job may
// reference a source/document/attachment that no longer resolves (a legacy or
// broken job); nil means the reference is absent in SQL. This keeps GetJob/
// ListJobs and the REST API from failing on a NULL FK.
type Job struct {
	ID           string
	SourceID     *string
	DocumentID   *string
	AttachmentID *string
	Status       string
	ContentHash  *string
	Attempt      int
	MaxAttempts  int
	ErrorCode    *string
	ErrorMessage *string
	ResolvedAt   *string
	EnqueuedAt   string
	// ForceRebuild (#259): true for force-rebuild jobs; populated by
	// EnqueueForceRebuild and the list queries (ListJobs/ActiveJobs/
	// ListJobsByAttachment).
	ForceRebuild bool
	// QualityState (#175): the preflight report JSONB (verdict/finding/
	// text_layer/…), nil when not assessed.
	QualityState json.RawMessage
}

// FailedJob describes a file-resolution failure that should be persisted as a
// failed ingest job so it is not silently dropped.
type FailedJob struct {
	SourceID     string
	DocumentID   string
	AttachmentID string
	ErrorCode    string
	ErrorMessage string
	Retryable    bool
}

// #259 force-rebuild sentinel errors: the API layer maps them to distinct
// HTTP statuses so an operator hears WHY a rebuild was refused.
var (
	// ErrNoPreferredAttachment: no preferred, non-deleted attachment on the
	// document (unknown id, or a document that was never ingested).
	ErrNoPreferredAttachment = errors.New("no preferred attachment for document")
	// ErrRebuildInFlight: a pending/claimed/processing job already exists for
	// the attachment — one rebuild at a time.
	ErrRebuildInFlight = errors.New("a job for this attachment is already active")
	// ErrNoContentHash: neither the attachment nor any prior job carries a
	// content hash — the claim path requires one (contract hash rule).
	ErrNoContentHash = errors.New("no content hash available for rebuild")
)

// EnqueueForceRebuild (#259, design a): first-class re-ingest after
// ARTIFACTS_EXPIRED. Creates a NEW pending job with force_rebuild=true for
// the document's preferred attachment instead of mutating the completed
// job's frozen snapshot (frozen inputs stay immutable — contract §frozen).
// The claim path then freezes a fresh snapshot and derives the force
// idempotency key `…:force-<jobID>` — distinct from the old job's key, so
// the runner's durable dedup never answers 409/ARTIFACTS_EXPIRED for it.
// The partial unique index (attachment_id, content_hash) WHERE
// force_rebuild=false permits the additional row by design.
func (r *Repo) EnqueueForceRebuild(ctx context.Context, documentID string) (*Job, error) {
	// Resolve the preferred, non-deleted attachment: operators name
	// documents (that is what /api/zotero/documents lists); the rebuild
	// targets the attachment ingest actually used.
	var sourceID, attID string
	var attHash *string
	err := r.pool.QueryRow(ctx, `
		SELECT a.source_id::text, a.id::text, a.content_hash
		FROM zotero_attachments a
		WHERE a.document_id = $1::uuid AND a.preferred AND NOT a.deleted
		ORDER BY a.updated_at DESC
		LIMIT 1`, documentID).Scan(&sourceID, &attID, &attHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoPreferredAttachment
	} else if err != nil {
		return nil, err
	}

	// The claim path requires a content hash; the attachment's current hash
	// wins, falling back to the latest job's hash (the generation being
	// rebuilt).
	hash := attHash
	if hash == nil {
		if err := r.pool.QueryRow(ctx, `
			SELECT content_hash FROM ingest_jobs
			WHERE attachment_id=$1 AND content_hash IS NOT NULL
			ORDER BY enqueued_at DESC LIMIT 1`, attID).Scan(&hash); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if hash == nil {
		return nil, ErrNoContentHash
	}

	// One rebuild in flight per attachment — SERIALIZED, not snapshot-tricked:
	// under READ COMMITTED a bare INSERT…SELECT…WHERE NOT EXISTS is NOT a
	// mutual exclusion (two concurrent statements each evaluate the predicate
	// against the pre-commit snapshot of the other; the partial unique index
	// covers force_rebuild=false rows only, so no constraint backstops force
	// rows either). A transaction-scoped advisory lock keyed on the attachment
	// id serializes the enqueues; the NOT EXISTS then decides — under the lock,
	// after the winner committed — whether this call wins or refuses.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, attID); err != nil {
		return nil, err
	}
	var job Job
	err = tx.QueryRow(ctx, `
		INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
		SELECT $1, $2::uuid, $3::uuid, $4, 'pending', true
		WHERE NOT EXISTS (
			SELECT 1 FROM ingest_jobs
			WHERE attachment_id = $3::uuid AND status IN ('pending','claimed','processing')
		)
		RETURNING id::text, status::text, content_hash, max_attempts, force_rebuild, enqueued_at::text`,
		sourceID, documentID, attID, hash).Scan(
		&job.ID, &job.Status, &job.ContentHash, &job.MaxAttempts, &job.ForceRebuild, &job.EnqueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRebuildInFlight // guard blocked the insert (lock-serialized)
	} else if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	job.SourceID = &sourceID
	job.DocumentID = &documentID
	job.AttachmentID = &attID
	return &job, nil
}

// ListJobs returns the most recent ingest jobs, newest first. Nullable FK
// columns are scanned as pointers so a legacy/broken job with a NULL reference
// does not abort the whole listing.
func (r *Repo) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id, document_id, attachment_id,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text, quality_state,
		       force_rebuild
		FROM ingest_jobs
		ORDER BY enqueued_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.SourceID, &j.DocumentID, &j.AttachmentID,
			&j.Status, &j.ContentHash, &j.Attempt, &j.MaxAttempts,
			&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt,
			&j.QualityState, &j.ForceRebuild); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ActiveJobs returns the in-flight ingest jobs (pending/claimed/processing),
// newest first. These are the job-state rows a WS snapshot needs: work the
// dispatcher may yet pick up or is currently driving. Terminal jobs
// (completed/failed/cancelled/obsolete) are excluded — they are history.
func (r *Repo) ActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id, document_id, attachment_id,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text, quality_state,
		       force_rebuild
		FROM ingest_jobs
		WHERE status IN ('pending','claimed','processing')
		ORDER BY enqueued_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.SourceID, &j.DocumentID, &j.AttachmentID,
			&j.Status, &j.ContentHash, &j.Attempt, &j.MaxAttempts,
			&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt,
			&j.QualityState, &j.ForceRebuild); err != nil {
			return nil, fmt.Errorf("scan active job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ListJobsByAttachment returns the jobs for a single attachment, newest first.
func (r *Repo) ListJobsByAttachment(ctx context.Context, attachmentID string) ([]Job, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id, document_id, attachment_id,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text, quality_state,
		       force_rebuild
		FROM ingest_jobs
		WHERE attachment_id = $1
		ORDER BY enqueued_at DESC
	`, attachmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.SourceID, &j.DocumentID, &j.AttachmentID,
			&j.Status, &j.ContentHash, &j.Attempt, &j.MaxAttempts,
			&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt,
			&j.QualityState, &j.ForceRebuild); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// CountJobsForSource returns the number of ingest jobs for a source.
func (r *Repo) CountJobsForSource(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ingest_jobs WHERE source_id = $1`, sourceID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}
