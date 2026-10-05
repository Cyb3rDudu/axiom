// leases.go — the maintenance gate's lease drain over the legacy
// ingest_jobs protocol (the 0006/0007 columns): an ACTIVE lease is a
// row in claimed/processing with a future lease_until. The three
// documented choices for a stuck lease, mirroring the repo's own
// semantics (repo/lease.go: RequestCancellation + terminalizeStale):
//
//	wait   poll until the workers finish (bounded by the timeout)
//	cancel request cancellation (pending rows converge immediately;
//	       claimed/processing rows converge once their lease expires —
//	       the gate itself applies the expired-cancel terminalization,
//	       because in the window no dispatcher is running to do it)
//	abort  stop the run, listing every lease (identifiers only)
//
// Every encounter is recorded as a LeaseDecision — observed state,
// choice, action, outcome — in run.json.
package cutover

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaseInfo is one observed active lease (identifiers only — never
// payload snapshots).
type LeaseInfo struct {
	JobID           string     `json:"job_id"`
	Status          string     `json:"status"`
	ClaimedBy       string     `json:"claimed_by,omitempty"`
	LeaseUntil      time.Time  `json:"lease_until"`
	LastHeartbeat   *time.Time `json:"last_heartbeat_at,omitempty"`
	CancelRequested bool       `json:"cancel_requested"`
}

// heartbeatAge is the age of the last heartbeat (nil-safe).
func (l LeaseInfo) heartbeatAge(now time.Time) int64 {
	if l.LastHeartbeat == nil {
		return 0
	}
	return int64(now.Sub(*l.LastHeartbeat).Seconds())
}

// activeLeases lists the currently-active leases on the pool.
func activeLeases(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]LeaseInfo, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, status::text, COALESCE(claimed_by,''), lease_until, last_heartbeat_at,
		       (cancel_requested_at IS NOT NULL) AS cancel_requested
		  FROM ingest_jobs
		 WHERE status IN ('claimed','processing') AND lease_until > $1
		 ORDER BY id`, now)
	if err != nil {
		return nil, fmt.Errorf("lease scan: %w", err)
	}
	defer rows.Close()
	var out []LeaseInfo
	for rows.Next() {
		var l LeaseInfo
		if err := rows.Scan(&l.JobID, &l.Status, &l.ClaimedBy, &l.LeaseUntil, &l.LastHeartbeat, &l.CancelRequested); err != nil {
			return nil, fmt.Errorf("lease scan row: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// requestCancellation mirrors repo.RequestCancellation: a pending row
// cancels immediately; a claimed/processing row records the request
// and converges via the expired-cancel terminalization below.
func requestCancellation(ctx context.Context, pool *pgxpool.Pool, jobID string) error {
	_, err := pool.Exec(ctx, `
		UPDATE ingest_jobs SET
			status = CASE WHEN status = 'pending' THEN 'cancelled'::ingest_job_status ELSE status END,
			cancel_requested_at = COALESCE(cancel_requested_at, now()),
			claimed_by = CASE WHEN status = 'pending' THEN NULL ELSE claimed_by END,
			lease_token = CASE WHEN status = 'pending' THEN NULL ELSE lease_token END,
			lease_until = CASE WHEN status = 'pending' THEN NULL ELSE lease_until END,
			last_heartbeat_at = CASE WHEN status = 'pending' THEN NULL ELSE last_heartbeat_at END,
			next_attempt_at = CASE WHEN status = 'pending' THEN NULL ELSE next_attempt_at END,
			completed_at = CASE WHEN status = 'pending' THEN COALESCE(completed_at, now()) END,
			updated_at = now()
		WHERE id = $1 AND status NOT IN ('completed','failed','cancelled','skipped')`, jobID)
	return err
}

// convergeExpiredCancels is terminalizeStale's cancel statements,
// applied by the gate itself (no dispatcher is alive in the window to
// converge a cancel-requested row whose lease has expired).
func convergeExpiredCancels(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE ingest_jobs SET status='cancelled', claimed_by=NULL, lease_token=NULL, lease_until=NULL,
		          completed_at=COALESCE(completed_at, now()), updated_at=now()
		 WHERE status IN ('claimed','processing') AND cancel_requested_at IS NOT NULL
		   AND lease_until IS NOT NULL AND lease_until <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// jobMutationWitness is the "no silent continuation" probe: a digest
// of the job table's mutability signals (row count + freshest
// updated_at + active leases). Taken before the freeze and re-checked
// after: any movement means something kept writing through the
// maintenance gate.
type jobMutationWitness struct {
	Count        int64      `json:"count"`
	MaxUpdatedAt *time.Time `json:"max_updated_at,omitempty"`
	ActiveLeases int        `json:"active_leases"`
}

func snapshotJobs(ctx context.Context, pool *pgxpool.Pool) (jobMutationWitness, error) {
	var w jobMutationWitness
	if err := pool.QueryRow(ctx, `SELECT count(*), max(updated_at) FROM ingest_jobs`).Scan(&w.Count, &w.MaxUpdatedAt); err != nil {
		return w, fmt.Errorf("job witness: %w", err)
	}
	now := time.Now()
	leases, err := activeLeases(ctx, pool, now)
	if err != nil {
		return w, err
	}
	w.ActiveLeases = len(leases)
	return w, nil
}

// maxUpdatedAtStr renders the freshest updated_at ("" when nil).
func (w jobMutationWitness) maxUpdatedAtStr() string {
	if w.MaxUpdatedAt == nil {
		return ""
	}
	return w.MaxUpdatedAt.UTC().Format(time.RFC3339Nano)
}

// record persists the witness for the run manifest (resume-safe).
func (w jobMutationWitness) record() *WitnessRecord {
	return &WitnessRecord{
		Count: w.Count, MaxUpdatedAt: w.maxUpdatedAtStr(), ActiveLeases: w.ActiveLeases,
	}
}

// maxUpdatedAt renders the persisted freshest updated_at.
func (w *WitnessRecord) maxUpdatedAt() string { return w.MaxUpdatedAt }

// drainOptions is the maintenance gate's runtime policy (plan-derived).
type drainOptions struct {
	Choice       string
	WaitTimeout  time.Duration
	PollInterval time.Duration
	OnDecision   func(LeaseDecision) // recording hook (run manifest)
	Logf         func(string, ...any)
}

// drainLeases enforces quiescence per the documented choice. It
// returns when zero active leases remain, or aborts with a full
// diagnosis (identifiers only).
func drainLeases(ctx context.Context, pool *pgxpool.Pool, opts drainOptions) error {
	timeout := opts.WaitTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for {
		now := time.Now()
		leases, err := activeLeases(ctx, pool, now)
		if err != nil {
			return err
		}
		if len(leases) == 0 {
			return nil
		}

		for _, l := range leases {
			d := LeaseDecision{
				JobID: l.JobID, Status: l.Status, ClaimedBy: l.ClaimedBy,
				LeaseUntil:    l.LeaseUntil.UTC().Format(time.RFC3339),
				HeartbeatAgeS: l.heartbeatAge(now), Choice: opts.Choice, At: nowTS(),
			}
			switch opts.Choice {
			case LeaseAbort:
				d.Action, d.Outcome = "abort the run", "aborted"
				if opts.OnDecision != nil {
					opts.OnDecision(d)
				}
				return fmt.Errorf("maintenance: %d active lease(s) and the plan chooses abort — run aborted (first: job %s held by %q, lease until %s)",
					len(leases), l.JobID, l.ClaimedBy, d.LeaseUntil)
			case LeaseCancel:
				if !l.CancelRequested {
					if err := requestCancellation(ctx, pool, l.JobID); err != nil {
						return fmt.Errorf("cancel job %s: %w", l.JobID, err)
					}
					d.Action = "cancellation requested (converges when the lease expires)"
				} else {
					d.Action = "cancellation already requested"
				}
				d.Outcome = "cancelled"
			case LeaseWait:
				d.Action, d.Outcome = "waiting for the worker to finish", "drained"
			}
			if opts.OnDecision != nil {
				opts.OnDecision(d)
			}
			if opts.Logf != nil {
				opts.Logf("lease: job %s status %s held by %q — %s", l.JobID, l.Status, l.ClaimedBy, d.Action)
			}
		}

		if time.Now().After(deadline) {
			// One last convergence attempt for cancels whose leases
			// expired at the wire, then the honest abort.
			if opts.Choice == LeaseCancel {
				if _, err := convergeExpiredCancels(ctx, pool); err != nil {
					return fmt.Errorf("converge expired cancels: %w", err)
				}
				leases, err := activeLeases(ctx, pool, time.Now())
				if err == nil && len(leases) == 0 {
					return nil
				}
			}
			return fmt.Errorf("maintenance: lease drain did not converge within %s — %d active lease(s) remain (first: job %s); the run aborts rather than freeze a moving corpus",
				timeout, len(leases), leases[0].JobID)
		}

		// Canceled rows whose leases have expired converge here on
		// every poll (the no-dispatcher convergence path).
		if opts.Choice == LeaseCancel {
			if _, err := convergeExpiredCancels(ctx, pool); err != nil {
				return fmt.Errorf("converge expired cancels: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
