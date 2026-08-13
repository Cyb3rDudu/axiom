package dispatcher

import (
	"context"
	"errors"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5"
)

// ResultPersister durably commits a validated processor result for a job and
// returns the durable snapshot id. This is the persistence boundary the work
// order assigns to Gate 4; axiom-ng cannot truthfully acknowledge a result, so
// a job whose result cannot be persisted is failed, never completed and never
// acknowledged. ACK (persisted:true) is sent ONLY after a persister returns a
// snapshot id, so a processor is never told its result was committed when
// nothing durable exists (Gate 2 finding F1).
type ResultPersister interface {
	// PersistResult durably commits result for jobID with the capability
	// dimension and verified artifact records, returning the snapshot id.
	PersistResult(ctx context.Context, jobID string, result []byte, opts repo.PersistOptions) (string, error)
}

// errPersister is the fallback when no real persister is wired: any job
// reaching completion must fail rather than be marked completed with no durable
// snapshot. Tests inject a recording fake persister instead.
type errPersister struct {
	msg string
}

func (e *errPersister) PersistResult(context.Context, string, []byte, repo.PersistOptions) (string, error) {
	return "", e
}

func (e *errPersister) Error() string { return e.msg }

// retryAcks periodically re-acknowledges completed jobs whose ack previously
// failed (ack_pending_at set). It NEVER reprocesses; it only retries the
// idempotent processor acknowledgement and clears the pending mark on success.
func retryAcks(ctx context.Context, d *Dispatcher) {
	ticker := time.NewTicker(d.cfg.AckRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pending, err := d.rep.AckPendingJobs(ctx, 64)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				d.logger.Printf("ack retry: list pending: %v", err)
				continue
			}
			for _, p := range pending {
				jobID := p[0]
				if err := d.client.Ack(ctx, jobID, processor.Ack{Persisted: true, SnapshotID: p[1]}); err != nil {
					if ctx.Err() != nil {
						return
					}
					d.logger.Printf("ack retry %s: %v", jobID, err)
					continue
				}
				if err := d.rep.ClearAckPending(ctx, jobID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					d.logger.Printf("ack retry %s: clear pending: %v", jobID, err)
				}
			}
		}
	}
}
