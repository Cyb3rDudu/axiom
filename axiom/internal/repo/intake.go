// intake.go — the revision-typed intake mint (F09 #303): IngestRevision's
// durable half. One statement decides replay / mismatch / dedup / mint;
// the #294 active-snapshot suppression resolves against the Store's own
// store_documents projection (#358 — no mirror access: the rendition's
// durable attachment identity was denormalized at intake/sync time).
package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// intakeKeyConstraint names the intake-key unique index — the 23505
// arbiter of the concurrent-mint re-ask. A literal, pinned against the
// migration by TestIntakeKeyConstraintMatchesMigration.
const intakeKeyConstraint = "ingest_jobs_intake_key_uq"

// ErrIntakeKeyMismatch: the idempotency key was reused with a DIFFERENT
// revision (canonical JSON differs) — the contract's
// contracterr.IdempotencyMismatch source.
var ErrIntakeKeyMismatch = errors.New("intake idempotency key reused with a different revision")

// resolveIntakeKey answers the intake-key question once — the ONE SQL
// text both the fast path and the 23505 re-ask share (a duplicated copy
// of the subtle `(revision_json = $2::jsonb)` comparison would drift
// silently on the effectively-untestable race path). Returns the replay
// job, ErrIntakeKeyMismatch, or pgx.ErrNoRows (caller decides).
func resolveIntakeKey(ctx context.Context, q queryer, req IntakeRequest) (*Job, error) {
	var existing Job
	var identical bool
	err := q.QueryRow(ctx, `
		SELECT id::text, status::text, COALESCE(content_hash,''), attempt, max_attempts,
		       enqueued_at::text, error_code, error_message, COALESCE(revision_no,''), updated_at,
		       (revision_json = $2::jsonb)
		FROM ingest_jobs
		WHERE intake_kind='revision' AND intake_idempotency_key=$1`, req.IdempotencyKey, string(req.RevisionJSON)).
		Scan(&existing.ID, &existing.Status, &existing.ContentHash, &existing.Attempt,
			&existing.MaxAttempts, &existing.EnqueuedAt, &existing.ErrorCode, &existing.ErrorMessage,
			&existing.RevisionNo, &existing.UpdatedAt, &identical)
	if err != nil {
		return nil, err
	}
	if !identical {
		return nil, ErrIntakeKeyMismatch
	}
	existing.IntakeKey = req.IdempotencyKey
	return &existing, nil
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
// them from the store_documents projection (loadAndLockRevisionState).
func (r *Repo) EnqueueRevisionIntake(ctx context.Context, req IntakeRequest) (*Job, bool, error) {
	return r.enqueueRevisionIntake(ctx, r.pool, req)
}

// EnqueueRevisionIntakeTx is the caller-transaction variant: the sync's
// store-effect phase mints (projection upserts, failed-file records and
// snapshot reconciliation share one commit).
func (r *Repo) EnqueueRevisionIntakeTx(ctx context.Context, tx pgx.Tx, req IntakeRequest) (*Job, bool, error) {
	return r.enqueueRevisionIntake(ctx, tx, req)
}

// reopenFailedIntake (#365) re-arms a TERMINAL-FAILED intake row when the
// same offer comes back: attempts must remain (an exhausted row keeps its
// verdict — force-rebuild stays the escape for genuinely poison content)
// and the content must not be served (the #294 suppression predicate,
// mirrored). Returns true when the row was reopened to pending.
// execQueryer is the union the reopen path needs: pool or caller-owned
// transaction (same shape as the mint's queryer plus Exec).
type execQueryer interface {
	queryer
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (r *Repo) reopenFailedIntake(ctx context.Context, ex execQueryer, existing *Job, req IntakeRequest) (bool, error) {
	if existing.Status != "failed" || existing.Attempt >= existing.MaxAttempts {
		return false, nil
	}
	tag, err := ex.Exec(ctx, `
		UPDATE ingest_jobs SET
			status='pending', attempt=0,
			error_code=NULL, error_message=NULL, resolved_at=NULL,
			next_attempt_at=NULL, claimed_by=NULL, lease_token=NULL, lease_until=NULL,
			enqueued_at=now(), updated_at=now()
		WHERE id=$1 AND status='failed' AND attempt < max_attempts
		  AND NOT EXISTS (
			-- #294 mirror: content already served = no re-offer
			SELECT 1 FROM processing_snapshots s
			JOIN store_documents p ON p.attachment_id = s.attachment_id
			WHERE p.source_id::text = $2 AND p.rendition_key = $3
			  AND p.deleted = false AND s.content_hash = $4 AND s.active)`,
		existing.ID, req.RevisionSourceID, req.RevisionRenditionID, req.ContentHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repo) enqueueRevisionIntake(ctx context.Context, ex queryer, req IntakeRequest) (*Job, bool, error) {
	// Intake-key idempotency precedes everything (the contract's
	// precedence rule; validation happened in the service layer already).
	existing, err := resolveIntakeKey(ctx, ex, req)
	if err == nil {
		return existing, false, nil // replay: the SAME job, no side effects
	}
	if errors.Is(err, ErrIntakeKeyMismatch) {
		return nil, false, ErrIntakeKeyMismatch
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	// pgx.ErrNoRows: the key is free — mint below.

	// Mint (or join) by revision identity, with the #294 suppression in the
	// same statement (the legacy writeJobsTx shape): an ACTIVE snapshot for
	// the same content on the rendition's CURRENT projection row means
	// processed-and-served — no re-enqueue while unchanged. An unresolvable
	// projection row mints the job with NULL FKs; the claim obsoletes it with a
	// readable reason (transition-honest: intake never refuses on a missing
	// projection — the boundary keeps the Library out of the intake decision).
	var job Job
	var joinedExisting bool
	err = ex.QueryRow(ctx, `
		INSERT INTO ingest_jobs (intake_kind, intake_idempotency_key, content_hash, status,
		                         revision_source_id, revision_record_id, revision_rendition_id,
		                         revision_no, revision_json, max_attempts)
		SELECT 'revision', $1, $2, 'pending', $3, $4, $5, $6, $7::jsonb, 3
		WHERE NOT EXISTS (
			-- #294: active snapshot for the SAME content = processed & served
			SELECT 1 FROM processing_snapshots s
			JOIN store_documents p ON p.attachment_id = s.attachment_id
			WHERE p.source_id::text = $3 AND p.rendition_key = $5 AND p.deleted = false
			  AND s.content_hash = $2 AND s.active)
		ON CONFLICT (revision_source_id, revision_rendition_id, content_hash)
		WHERE intake_kind='revision' AND force_rebuild=false
		  AND status IN ('pending','claimed','processing')
		DO UPDATE SET
		  -- Identity re-target (#358 review): a joined ACTIVE job whose
		  -- record identity MOVED (reparent — same rendition + hash, new
		  -- record) carries the fresh revision artifact, or the claim's
		  -- record-key guard would obsolete it with no replacement minted
		  -- (a stall until the next version bump). (key, json) stay
		  -- consistent so replays never see a spurious key mismatch;
		  -- updated_at stays UNTOUCHED — bookkeeping must not re-rank the
		  -- outcome read model (#294).
		  intake_idempotency_key = EXCLUDED.intake_idempotency_key,
		  revision_record_id     = EXCLUDED.revision_record_id,
		  revision_no            = EXCLUDED.revision_no,
		  revision_json          = EXCLUDED.revision_json,
		  updated_at             = ingest_jobs.updated_at
		RETURNING id::text, status::text, COALESCE(content_hash,''), attempt, max_attempts, enqueued_at::text,
		          error_code, error_message, COALESCE(revision_no,''), updated_at, (xmax <> 0) AS was_existing`,
		req.IdempotencyKey, req.ContentHash, req.RevisionSourceID, req.RevisionRecordID,
		req.RevisionRenditionID, req.RevisionNo, string(req.RevisionJSON)).
		Scan(&job.ID, &job.Status, &job.ContentHash, &job.Attempt, &job.MaxAttempts, &job.EnqueuedAt,
			&job.ErrorCode, &job.ErrorMessage, &job.RevisionNo, &job.UpdatedAt, &joinedExisting)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrIntakeSuppressed
		}
		// Concurrent same-key race (review F1.4): the SELECT saw nothing,
		// a concurrent mint won the intake-key unique index — the loser's
		// 23505 is the SAME question the SELECT answers; re-ask it once and
		// classify honestly instead of surfacing an Internal error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == intakeKeyConstraint {
			// The concurrent winner committed (Postgres blocks on
			// uncommitted index entries): re-ask and classify honestly.
			replay, rerr := resolveIntakeKey(ctx, ex, req)
			if rerr != nil {
				return nil, false, rerr
			}
			return replay, false, nil
		}
		return nil, false, err
	}
	job.IntakeKey = req.IdempotencyKey
	if joinedExisting {
		return &job, false, nil // identity dedup: the rendition+hash job already existed
	}
	return &job, true, nil
}
