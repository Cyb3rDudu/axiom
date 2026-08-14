package repo

// L-sourceurl: source-download lookup for the /api/processor/source endpoint.
// Read-only, single query; the endpoint enforces signature, status and lease.

import (
	"context"
	"time"
)

// ProcessorSource is the endpoint's view of a claimed job's source file.
// Size is deliberately not loaded: file_size is nullable in the schema and a
// NULL would fail Scan (silent 404); the endpoint streams via ServeContent,
// which sets Content-Length from the file itself.
type ProcessorSource struct {
	LocalPath   string
	ContentType string
	Status      string
	LeaseUntil  time.Time
}

// ProcessorSource loads the source facts for a job. Returns pgx.ErrNoRows
// when the job does not exist (the endpoint maps every failure to 404 —
// no existence oracle).
func (r *Repo) ProcessorSource(ctx context.Context, jobID string) (ProcessorSource, error) {
	var s ProcessorSource
	err := r.pool.QueryRow(ctx, `
		SELECT a.local_path, a.content_type, j.status, j.lease_until
		FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id = j.attachment_id
		WHERE j.id = $1::uuid
	`, jobID).Scan(&s.LocalPath, &s.ContentType, &s.Status, &s.LeaseUntil)
	if err != nil {
		return ProcessorSource{}, err
	}
	return s, nil
}
