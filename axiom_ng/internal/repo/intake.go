// intake.go — the revision-typed intake mint (F09 #303): IngestRevision's
// durable half. One statement decides replay / mismatch / dedup / mint;
// the #294 active-snapshot suppression is applied via the mirror-resolved
// attachment (the documented dual-read — SQL against the shared mirror
// tables, no adapter import; DM06/F12 abate it).
package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrIntakeKeyMismatch: the idempotency key was reused with a DIFFERENT
// revision (canonical JSON differs) — the contract's
// contracterr.IdempotencyMismatch source.
var ErrIntakeKeyMismatch = errors.New("intake idempotency key reused with a different revision")

// enqueueRevisionIntakeReplay re-runs the intake-key lookup after a
// concurrent mint won the unique index — the key exists now.
func (r *Repo) enqueueRevisionIntakeReplay(ctx context.Context, req IntakeRequest) (*Job, bool, error) {
	var existing Job
	var identical bool
	err := r.pool.QueryRow(ctx, `
		SELECT id::text, status::text, COALESCE(content_hash,''), attempt, max_attempts,
		       enqueued_at::text, (revision_json = $2::jsonb)
		FROM ingest_jobs
		WHERE intake_kind='revision' AND intake_idempotency_key=$1`, req.IdempotencyKey, string(req.RevisionJSON)).
		Scan(&existing.ID, &existing.Status, &existing.ContentHash, &existing.Attempt,
			&existing.MaxAttempts, &existing.EnqueuedAt, &identical)
	if err != nil {
		return nil, false, err
	}
	if identical {
		existing.IntakeKey = req.IdempotencyKey
		return &existing, false, nil
	}
	return nil, false, ErrIntakeKeyMismatch
}

// ErrIntakeSuppressed: the revision's content is already processed and
// served (an ACTIVE snapshot for the same hash on the rendition's current
// mirror attachment) — the #294 defense: no "never processed" re-enqueue
// while unchanged. The service answers a committed-looking IngestJob echo
// (v1 has no job row to point at — the suppression mints nothing).
var ErrIntakeSuppressed = errors.New("revision content already served by an active snapshot")

// IntakeRequest is the durable intake input: the caller-chosen idempotency
// key, the full revision identity and the canonical revision JSON (the
// verbatim SourceRevision DTO — the store's frozen copy).
type IntakeRequest struct {
	IdempotencyKey      string
	RevisionSourceID    string
	RevisionRecordID    string
	RevisionRenditionID string
	RevisionNo          string
	ContentHash         string
	RevisionJSON        []byte
}

// EnqueueRevisionIntake mints one revision-typed pending job (or answers
// the existing one). Outcomes:
//   - (job, false, nil): replay or identity dedup — the SAME durable job
//   - (nil, false, ErrIntakeKeyMismatch): the key exists with a different revision
//   - (nil, false, ErrIntakeSuppressed): content already served (#294)
//   - (job, true, nil): a NEW pending job was minted
//
// The document/attachment FK columns stay NULL at mint; the claim resolves
// them from the mirror (loadAndLockState's revision lane).
func (r *Repo) EnqueueRevisionIntake(ctx context.Context, req IntakeRequest) (*Job, bool, error) {
	// Intake-key idempotency precedes everything (the contract's
	// precedence rule; validation happened in the service layer already).
	var existing Job
	var identical bool
	err := r.pool.QueryRow(ctx, `
		SELECT id::text, status::text, COALESCE(content_hash,''), attempt, max_attempts,
		       enqueued_at::text, (revision_json = $2::jsonb)
		FROM ingest_jobs
		WHERE intake_kind='revision' AND intake_idempotency_key=$1`, req.IdempotencyKey, string(req.RevisionJSON)).
		Scan(&existing.ID, &existing.Status, &existing.ContentHash, &existing.Attempt,
			&existing.MaxAttempts, &existing.EnqueuedAt, &identical)
	if err == nil {
		if identical {
			existing.IntakeKey = req.IdempotencyKey
			return &existing, false, nil // replay: the SAME job, no side effects
		}
		return nil, false, ErrIntakeKeyMismatch
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	// Mint (or join) by revision identity, with the #294 suppression in the
	// same statement (the legacy writeJobsTx shape): an ACTIVE snapshot for
	// the same content on the rendition's CURRENT attachment row means
	// processed-and-served — no re-enqueue while unchanged. An unresolvable
	// mirror row mints the job with NULL FKs; the claim obsoletes it with a
	// readable reason (transition-honest: intake never refuses on a mirror
	// miss — the boundary keeps the Library out of the intake decision).
	var job Job
	var joinedExisting bool
	err = r.pool.QueryRow(ctx, `
		INSERT INTO ingest_jobs (intake_kind, intake_idempotency_key, content_hash, status,
		                         revision_source_id, revision_record_id, revision_rendition_id,
		                         revision_no, revision_json, max_attempts)
		SELECT 'revision', $1, $2, 'pending', $3, $4, $5, $6, $7::jsonb, 3
		WHERE NOT EXISTS (
			-- #294: active snapshot for the SAME content = processed & served
			SELECT 1 FROM processing_snapshots s
			JOIN zotero_attachments a ON a.id = s.attachment_id
			WHERE a.source_id::text = $3 AND a.zotero_key = $5 AND a.deleted = false
			  AND s.content_hash = $2 AND s.active)
		ON CONFLICT (revision_rendition_id, content_hash) WHERE intake_kind='revision' AND force_rebuild=false
		DO UPDATE SET updated_at = ingest_jobs.updated_at
		RETURNING id::text, status::text, COALESCE(content_hash,''), attempt, max_attempts, enqueued_at::text, (xmax <> 0) AS was_existing`,
		req.IdempotencyKey, req.ContentHash, req.RevisionSourceID, req.RevisionRecordID,
		req.RevisionRenditionID, req.RevisionNo, string(req.RevisionJSON)).
		Scan(&job.ID, &job.Status, &job.ContentHash, &job.Attempt, &job.MaxAttempts, &job.EnqueuedAt, &joinedExisting)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrIntakeSuppressed
		}
		// Concurrent same-key race (review F1.4): the SELECT saw nothing,
		// a concurrent mint won the intake-key unique index — the loser's
		// 23505 is the SAME question the SELECT answers; re-ask it once and
		// classify honestly instead of surfacing an Internal error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "ingest_jobs_intake_key_uq" {
			return r.enqueueRevisionIntakeReplay(ctx, req)
		}
		return nil, false, err
	}
	job.IntakeKey = req.IdempotencyKey
	if joinedExisting {
		return &job, false, nil // identity dedup: the rendition+hash job already existed
	}
	return &job, true, nil
}
