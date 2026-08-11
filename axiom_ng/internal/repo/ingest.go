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
	ContentHash  *string
	Attempt      int
	MaxAttempts  int
	ErrorCode    *string
	ErrorMessage *string
	ResolvedAt   *string
	EnqueuedAt   string
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

// ListJobs returns the most recent ingest jobs, newest first.
func (r *Repo) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id::text, document_id::text, attachment_id::text,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text
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
			&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ListJobsByAttachment returns the jobs for a single attachment, newest first.
func (r *Repo) ListJobsByAttachment(ctx context.Context, attachmentID string) ([]Job, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, source_id::text, document_id::text, attachment_id::text,
		       status::text, content_hash, attempt, max_attempts, error_code,
		       error_message, resolved_at::text, enqueued_at::text
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
			&j.ErrorCode, &j.ErrorMessage, &j.ResolvedAt, &j.EnqueuedAt); err != nil {
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
