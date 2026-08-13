package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// isLost reports whether an error is the repo's lost-lease sentinel, in which
// case the dispatcher must stop local mutation and never persist/ack.
func isLost(err error) bool {
	return errors.Is(err, repo.ErrLostLease)
}

// jitter scales d by a random factor in [0.75, 1.25) so concurrent workers don't
// re-poll in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	f := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(d) * f)
}

// submitRetryable decides whether a failed POST /v1/process is worth retrying.
// A 4xx (other than transient 429) is a request contract defect and retrying
// fails identically; 5xx/429 and transport errors are transient. A cancelled
// caller context is the dispatcher shutting down, not a processor fault.
func submitRetryable(err error) bool {
	var se *processor.StatusError
	if errors.As(err, &se) {
		return se.Code >= 500 || se.Code == 429
	}
	if errors.Is(err, processor.ErrCancelled) {
		return false
	}
	return true
}

// handleSubmitFailure maps a failed POST /v1/process onto retry/terminal policy.
func (d *Dispatcher) handleSubmitFailure(ctx context.Context, claimed *repo.ClaimedJob, cause error) {
	ref := claimed.LeaseRef
	if isLost(cause) {
		return
	}
	if submitRetryable(cause) {
		d.scheduleRetry(ctx, ref, claimed.Attempt, "PROCESS_SUBMIT_FAILED", cause.Error())
		return
	}
	d.markTerminal(ctx, ref, "PROCESS_SUBMIT_FAILED", cause.Error())
}

func (d *Dispatcher) scheduleRetry(ctx context.Context, ref repo.LeaseRef, attempt int, code, msg string) {
	// Bounded exponential backoff keyed by attempt (1-indexed). The repo clamps
	// the delay and owns the row fencing.
	delay := time.Duration(1<<max(0, attempt-1)) * time.Second
	if delay > d.cfg.MaxRetryBackoff {
		delay = d.cfg.MaxRetryBackoff
	}
	if _, err := d.rep.ScheduleRetry(ctx, ref, code, msg, int(delay.Seconds())); err != nil && !isLost(err) {
		d.logger.Printf("schedule retry: %v", err)
	}
}

func (d *Dispatcher) markTerminal(ctx context.Context, ref repo.LeaseRef, code, msg string) {
	if err := d.rep.MarkFailed(ctx, ref, code, msg); err != nil && !isLost(err) {
		d.logger.Printf("mark failed: %v", err)
	}
}

// maxConsecutiveStatusErrors caps how many consecutive processor status failures
// a single run tolerates before scheduling a retry (instead of renewing the lease
// forever against a dead/unsupported processor).
const maxConsecutiveStatusErrors = 5

// pollAndFinish drives a job from 'processing' to a terminal state: polls the
// processor status while renewing the lease, handles cancellation, then on
// completion fetches + minimally validates the result and marks completed.
func (d *Dispatcher) pollAndFinish(ctx context.Context, claimed *repo.ClaimedJob) {
	ref := claimed.LeaseRef
	fields := []any{ref.JobID, claimed.AttachmentID, claimed.DocumentID, claimed.Attempt}
	next := time.Now()
	consecutive := 0
	for {
		if ctx.Err() != nil {
			return
		}
		// Honour a cancellation request before renewing/continuing: tell the
		// processor to stop and converge fenced to cancelled. The repo's
		// terminalize/claim scan would eventually cancel on lease expiry, but we
		// must not keep renewing a job an operator asked to stop.
		cancelRequested, cerr := d.rep.JobCancelRequested(ctx, ref.JobID)
		if cerr != nil {
			d.logger.Printf("%v: read cancel request: %v", fields, cerr)
			return
		}
		if cancelRequested {
			if err := d.client.Cancel(ctx, ref.JobID); err != nil {
				d.logger.Printf("%v: processor cancel: %v", fields, err)
			}
			d.onCancelled(ctx, ref) // fenced MarkCancelled; lost lease no-ops
			return
		}

		// Renew the lease on our interval so a long processor run is not lost.
		if time.Now().After(next) {
			if err := d.rep.RenewLease(ctx, ref, d.cfg.LeaseDuration); err != nil {
				if isLost(err) {
					d.logger.Printf("%v: lease lost while processing; not acknowledging", fields)
					return
				}
				d.logger.Printf("%v: renew lease: %v", fields, err)
				return
			}
			next = time.Now().Add(jitter(d.cfg.RenewalInterval))
		}

		st, err := d.client.JobStatus(ctx, ref.JobID)
		if err != nil {
			// A transient status error is tolerable briefly, but a processor that
			// keeps failing status (404/401/bad JSON -> the client rejects it) must
			// not keep a lease renewed forever. After a cap of consecutive failures
			// we return the job to pending via retry; when it is reclaimed later it
			// is re-submitted with the SAME frozen idempotency key so a surviving
			// processor dedupes (F6 recovery without rerunning duplicate work).
			consecutive++
			if consecutive >= maxConsecutiveStatusErrors {
				d.scheduleRetry(ctx, ref, claimed.Attempt, "PROCESS_STATUS_FAILED", err.Error())
				return
			}
			if ctx.Err() != nil {
				return
			}
			if !waitFor(ctx, jitter(d.cfg.RenewalInterval)) {
				return
			}
			continue
		}
		consecutive = 0
		switch st.Status {
		case "completed":
			d.onCompleted(ctx, claimed)
			return
		case "failed":
			if st.Error != nil {
				d.onFailed(ctx, claimed, st.Error)
			} else {
				d.onFailed(ctx, claimed, &processor.JobError{Code: "PROCESS_FAILED", Message: "processor reported failure without error details", Retryable: true})
			}
			return
		case "cancelled":
			d.onCancelled(ctx, ref)
			return
		default: // accepted, running, advisory in-progress states
			if !waitFor(ctx, jitter(d.cfg.RenewalInterval)) {
				return
			}
		}
	}
}

func (d *Dispatcher) onFailed(ctx context.Context, claimed *repo.ClaimedJob, jobErr *processor.JobError) {
	ref := claimed.LeaseRef
	if jobErr.Retryable {
		d.scheduleRetry(ctx, ref, claimed.Attempt, jobErr.Code, jobErr.Message)
		return
	}
	d.markTerminal(ctx, ref, jobErr.Code, jobErr.Message)
}

func (d *Dispatcher) onCancelled(ctx context.Context, ref repo.LeaseRef) {
	if err := d.rep.MarkCancelled(ctx, ref); err != nil && !isLost(err) {
		d.logger.Printf("mark cancelled: %v", err)
	}
}

// onCompleted encapsulates the completion boundary. Here we fetch the
// result, validate + durably persist the snapshot + verified artifacts in ONE
// fenced transaction (the persister fence-completes the job atomically in
// that same commit — §10.2), and acknowledge the processor. An
// acknowledgement failure must never rerun processing.
//
// C1 fix: the persister owns the fenced completion (it runs MarkCompletedTx as
// step 6 of its single transaction). The dispatcher must NOT call
// markCompleted again: a second MarkCompletedTx finds status='completed' (not
// 'processing'), returns ErrLostLease, and onCompleted would then skip the ACK
// without setting ack_pending — breaking F3 in production. ACK directly after a
// successful PersistResult.
func (d *Dispatcher) onCompleted(ctx context.Context, claimed *repo.ClaimedJob) {
	ref := claimed.LeaseRef
	resultBytes, err := d.client.JobResult(ctx, ref.JobID)
	if err != nil {
		// We cannot complete without the result.
		d.markTerminal(ctx, ref, "RESULT_FETCH_FAILED", err.Error())
		return
	}
	// Minimal structural check: the result must echo the job id. Full §14
	// validation runs inside PersistResult before any row is inserted.
	var resMeta struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(resultBytes, &resMeta); err != nil || resMeta.JobID != ref.JobID {
		d.markTerminal(ctx, ref, "RESULT_INVALID", "processor result does not echo job_id")
		return
	}

	// Stage + verify the durable artifacts (§13/§14.4.6): fetch each artifact's
	// bytes, hash + length-check against the result's declaration, commit via an
	// atomic rename under ArtifactRoot. A digest/size mismatch makes the job
	// terminal (validation failure) before any snapshot row is inserted.
	arts, aerr := d.stageArtifacts(ctx, ref.JobID, resultBytes)
	if aerr != nil {
		d.markTerminal(ctx, ref, "RESULT_INVALID", aerr.Error())
		return
	}

	// Durably persist + fence-complete the job in ONE transaction. The persister
	// (repo.PersistResult) inserts the snapshot, switches the active flag, writes
	// the outbox and calls MarkCompletedTx — all in a single commit. ACK's
	// persisted flag is only ever true once this returns a snapshot id (F1).
	// CapDim is the int dimension the processor declared in /v1/capabilities
	// (no string fallback — Hivemind Gate-3 hint).
	snapshotID, err := d.persist.PersistResult(ctx, ref.JobID, resultBytes, repo.PersistOptions{
		CapDim:    d.capDim(),
		Artifacts: arts,
	})
	if err != nil {
		d.markTerminal(ctx, ref, "RESULT_PERSIST_FAILED", err.Error())
		return
	}

	// Acknowledge after durable commit. ACK failure keeps the job completed but
	// marks ack_pending so the separate retry pass re-acknowledges it; it is
	// never reprocessed (F3).
	if err := d.client.Ack(ctx, ref.JobID, processor.Ack{Persisted: true, SnapshotID: snapshotID}); err != nil {
		d.logger.Printf("%v: ack failed; will retry separately: %v", []any{ref.JobID}, err)
		if merr := d.rep.MarkAckFailed(ctx, ref.JobID); merr != nil {
			d.logger.Printf("%v: mark ack-pending: %v", []any{ref.JobID}, merr)
		}
	}
}

// waitFor blocks for d or until ctx is done; returns false if ctx done.
func waitFor(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
