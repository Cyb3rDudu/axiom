package repo

// L-sourceurl: source-download lookup for the /api/processor/source endpoint.
// Read-only, single query; the endpoint enforces signature, status and lease.
//
// Clock-domain note (#271): the lease-freshness decision is made INSIDE the
// SQL lookup (`lease_until > now()`, DB clock) and travels to the handler as a
// boolean. The handler must never compare a Host `time.Now()` against the DB
// timestamp: the two clock domains (host sleep → VM lag, NTP step, remote DB)
// are not guaranteed monotonic relative to each other, and the #270 incident
// saw a 431 s host/VM offset silently 404 every source download.

import (
	"context"
)

// ProcessorSource is the endpoint's view of a claimed job's source file.
// Size is deliberately not loaded: file_size is nullable in the schema and a
// NULL would fail Scan (silent 404); the endpoint streams via ServeContent,
// which sets Content-Length from the file itself.
//
// LeaseFresh is evaluated by the database (`lease_until IS NOT NULL AND
// lease_until > now()`), so freshness lives in one clock domain. The raw
// timestamp is intentionally NOT exposed to the handler — there is nothing
// for it to compare against.
type ProcessorSource struct {
	LocalPath   string
	ContentType string
	Status      string
	LeaseFresh  bool
}

// ProcessorSource loads the source facts for a job. Returns pgx.ErrNoRows
// when the job does not exist (the endpoint maps every failure to 404 —
// no existence oracle).
func (r *Repo) ProcessorSource(ctx context.Context, jobID string) (ProcessorSource, error) {
	var s ProcessorSource
	err := r.pool.QueryRow(ctx, `
		SELECT a.local_path, a.content_type, j.status,
		       (j.lease_until IS NOT NULL AND j.lease_until > now()) AS lease_fresh
		FROM ingest_jobs j
		JOIN zotero_attachments a ON a.id = j.attachment_id
		WHERE j.id = $1::uuid
	`, jobID).Scan(&s.LocalPath, &s.ContentType, &s.Status, &s.LeaseFresh)
	if err != nil {
		return ProcessorSource{}, err
	}
	return s, nil
}
