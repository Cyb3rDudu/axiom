package repo

import (
	"context"
	"fmt"
)

// PendingJob describes a processing unit the sync layer wants to enqueue.
type PendingJob struct {
	SourceID     string
	DocumentID   string
	AttachmentID string
	ContentHash  string
	ForceRebuild bool
}

// Job is a row of ingest_jobs.
type Job struct {
	ID           string
	SourceID     string
	DocumentID   string
	AttachmentID string
	Status       string
	ContentHash  string
	Attempt      int
	MaxAttempts  int
	ErrorCode    *string
	ErrorMessage *string
	EnqueuedAt   string
}

// Enqueue inserts new ingest_jobs for the given pending units, skipping any
// that already have a job for the same attachment_id + content_hash unless
// ForceRebuild is set. The partial unique index
//
//	ingest_jobs_idempotency_idx (attachment_id, content_hash) WHERE NOT force_rebuild
//
// makes this atomic and safe under concurrent Sync calls: two goroutines
// inserting the same unit race, but only one wins; the loser's ON CONFLICT DO
// NOTHING just inserts no row.
func (r *Repo) Enqueue(ctx context.Context, pending []PendingJob) (int, error) {
	if len(pending) == 0 {
		return 0, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	inserted := 0
	for _, p := range pending {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
			VALUES ($1,$2,$3,$4, 'pending', $5)
			ON CONFLICT (attachment_id, content_hash) WHERE force_rebuild = false DO NOTHING
		`, p.SourceID, p.DocumentID, p.AttachmentID, p.ContentHash, p.ForceRebuild)
		if err != nil {
			return inserted, err
		}
		inserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return inserted, err
	}
	return inserted, nil
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

// EnqueueFailed records a failed ingest job for an attachment whose local file
// could not be resolved. A retryable failure is re-attempted by a future run;
// a non-retryable one (e.g. FILE_NOT_FOUND) stays failed.
func (r *Repo) EnqueueFailed(ctx context.Context, f FailedJob) error {
	code := f.ErrorCode
	if code == "" {
		code = "IO_ERROR"
	}
	maxAttempts := 0
	if f.Retryable {
		maxAttempts = 3
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO ingest_jobs
			(source_id, document_id, attachment_id, status, error_code, error_message, max_attempts)
		VALUES ($1,$2,$3, 'failed', $4, $5, $6)
	`, f.SourceID, f.DocumentID, f.AttachmentID, code, f.ErrorMessage, maxAttempts)
	if err != nil {
		return fmt.Errorf("enqueue failed job: %w", err)
	}
	return nil
}

// ListJobs returns the most recent ingest jobs, newest first.
func (r *Repo) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id::text, document_id::text, attachment_id::text,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, enqueued_at::text
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
			&j.ErrorCode, &j.ErrorMessage, &j.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
