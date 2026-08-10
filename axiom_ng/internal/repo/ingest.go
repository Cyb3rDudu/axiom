package repo

import (
	"context"
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
	ErrorCode    string
	ErrorMessage string
	EnqueuedAt   string
}

// Enqueue inserts new ingest_jobs for the given pending units, skipping any
// that already have a job for the same attachment_id + content_hash unless
// ForceRebuild is set (guarded by the partial unique index).
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
		// Force-rebuild rows bypass the idempotency guard; a matching job for
		// this attachment content is left alone otherwise.
		if !p.ForceRebuild {
			existing, err := r.hasJob(ctx, tx, p.AttachmentID, p.ContentHash)
			if err != nil {
				return inserted, err
			}
			if existing {
				continue
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status)
			VALUES ($1,$2,$3,$4, 'pending')
		`, p.SourceID, p.DocumentID, p.AttachmentID, p.ContentHash)
		if err != nil {
			return inserted, err
		}
		inserted++
	}
	if err := tx.Commit(ctx); err != nil {
		return inserted, err
	}
	return inserted, nil
}

// hasJob reports whether a job already covers this attachment content, in
// which case we should not enqueue another.
func (r *Repo) hasJob(ctx context.Context, tx pgx.Tx, attachmentID, contentHash string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM ingest_jobs
		WHERE attachment_id = $1 AND content_hash = $2
		LIMIT 1
	`, attachmentID, contentHash).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
