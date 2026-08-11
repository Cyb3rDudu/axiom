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

// gate2ProcessorVersion is the processor identity stamp written on completion
// during Gate 2. It is deliberately a placeholder; Gate 4 replaces it with the
// real processor name/version from capability negotiation and result validation.
const gate2ProcessorVersion = "0.1.0"

// markCompleted fence-completes a job under a caller-owned transaction so Gate 4
// can persist the snapshot atomically with the completion in one commit. Here in
// Gate 2 the transaction carries only the fenced completion and a marker
// processor identity/snapshot id; full persistence arrives in Gate 4.
func (d *Dispatcher) markCompleted(ctx context.Context, ref repo.LeaseRef, processorName string) error {
	tx, err := d.rep.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := d.rep.MarkCompletedTx(ctx, tx, ref, processorName, gate2ProcessorVersion, ref.JobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// pollAndFinish drives a job from 'processing' to a terminal state: polls the
// processor status while renewing the lease, handles cancellation, then on
// completion fetches + minimally validates the result and marks completed.
func (d *Dispatcher) pollAndFinish(ctx context.Context, claimed *repo.ClaimedJob) {
	ref := claimed.LeaseRef
	fields := []any{ref.JobID, claimed.AttachmentID, claimed.DocumentID, claimed.Attempt}
	next := time.Now()
	for {
		if ctx.Err() != nil {
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
			// A transient status error is tolerable; keep polling.
			if ctx.Err() != nil {
				return
			}
			if !waitFor(ctx, jitter(d.cfg.RenewalInterval)) {
				return
			}
			continue
		}
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

// onCompleted encapsulates Gate 2's completion boundary. Full result validation
// and durable snapshot/artifact persistence arrive in Gate 4. Here we fetch the
// result, mark the job completed (serialized against sync) and acknowledge the
// processor. An acknowledgement failure must never rerun processing.
func (d *Dispatcher) onCompleted(ctx context.Context, claimed *repo.ClaimedJob) {
	ref := claimed.LeaseRef
	resultBytes, err := d.client.JobResult(ctx, ref.JobID)
	if err != nil {
		// We cannot complete without the result.
		d.markTerminal(ctx, ref, "RESULT_FETCH_FAILED", err.Error())
		return
	}
	// Minimal Gate 2 validation: the result must echo the job id. Structural
	// validation + persistence arrive in Gate 4.
	var resMeta struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(resultBytes, &resMeta); err != nil || resMeta.JobID != ref.JobID {
		d.markTerminal(ctx, ref, "RESULT_INVALID", "processor result does not echo job_id")
		return
	}

	// Mark completed under a caller-owned transaction so Gate 4 persists the
	// snapshot atomically; here we use a short owner-transaction for the fenced
	// completion and a marker snapshot id.
	err = d.markCompleted(ctx, ref, "fake-processor-gate2")
	if isLost(err) {
		d.logger.Printf("%v: lost lease at completion; not acknowledging", []any{ref.JobID})
		return
	}
	if err != nil {
		d.logger.Printf("%v: mark completed: %v", []any{ref.JobID}, err)
		return
	}
	// Ack after fenced success. ACK failure keeps the job completed; it is retried
	// separately and never reruns processing.
	// ponytail: Gate 2 does NOT durably persist the snapshot/artifacts (that is
	// Gate 4), so we must not tell a processor its result was persisted and may be
	// GC'd. Send persisted=false until Gate 4 commits real storage, then flip.
	if err := d.client.Ack(ctx, ref.JobID, processor.Ack{Persisted: false, SnapshotID: ref.JobID}); err != nil {
		d.logger.Printf("%v: ack failed (job stays completed): %v", []any{ref.JobID}, err)
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
