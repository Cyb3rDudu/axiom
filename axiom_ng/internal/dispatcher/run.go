package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
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

// sourceDownloadRetryable reports whether a /v1/process error body names a
// source-TRANSPORT cause (#235): the runner could not pull the source_url —
// typically the HMAC exp (minted from the claim's lease) aged out while the
// job sat in the runner's single-lane queue. The file itself is intact, so a
// retry with a fresh claim + freshly minted URL succeeds. A genuine poison
// 422 (hash-gate mismatch: "downloaded source failed the content hash gate")
// uses different wording and stays terminal — discrimination is by error
// detail, never by status code alone.
func sourceDownloadRetryable(body string) bool {
	for _, marker := range []string{
		"source_url download failed",
		"source_url returned HTTP",
		"source_url download exceeded",
	} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// handleSubmitFailure maps a failed POST /v1/process onto retry/terminal policy.
func (d *Dispatcher) handleSubmitFailure(ctx context.Context, claimed *repo.ClaimedJob, cause error) {
	ref := claimed.LeaseRef
	if isLost(cause) {
		return
	}
	// #126 replay-after-ack: the runner reports ARTIFACTS_EXPIRED (409) when
	// a resubmit dedups onto an acknowledged job — its artifacts died with
	// the ACK (§15/§19.10). Retrying hits the same wall, so this is terminal
	// with its own code, not PROCESS_SUBMIT_FAILED.
	var se *processor.StatusError
	// Bounded fallback: if the body match ever misses (formatting change),
	// the error degrades to terminal PROCESS_SUBMIT_FAILED (409 is not
	// submit-retryable) — never a retry loop.
	if errors.As(cause, &se) && se.Code == 409 && strings.Contains(se.Body, "ARTIFACTS_EXPIRED") {
		d.markTerminal(ctx, ref, "ARTIFACTS_EXPIRED",
			"runner acknowledged this job; artifacts expired with ACK (contract §15/§19.10) — re-enqueue with force_rebuild to recompute")
		return
	}
	// #235: stale source URL at runner execution — retryable, not poison.
	if errors.As(cause, &se) && se.Code == 422 && sourceDownloadRetryable(se.Body) {
		d.scheduleRetry(ctx, ref, claimed.Attempt, "SOURCE_URL_STALE", cause.Error())
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
	// #167: every terminal failure reaches this choke point; publish the
	// observer-only JobFailed after the fenced mark.
	d.publish(events.JobFailed{JobID: ref.JobID, ErrorCode: code})
}

// maxConsecutiveStatusErrors caps how many consecutive processor status failures
// a single run tolerates before scheduling a retry (instead of renewing the lease
// forever against a dead/unsupported processor).
const maxConsecutiveStatusErrors = 5

// jobPhases records per-phase timestamps for the one-line phase log (L8 fix:
// the next anomaly should be readable from the dispatcher log alone).
type jobPhases struct {
	jobID    string
	claim    time.Time
	submit   time.Time
	complete time.Time // processor reported completed
	result   time.Time // result fetched
	staged   time.Time // artifacts verified + committed
	arts     int
	persist  time.Time
	ack      time.Time
}

func (ph *jobPhases) line() string {
	f := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Format("15:04:05")
	}
	return fmt.Sprintf("job=%s claim=%s submit=%s completed=%s resultFetched=%s artifacts=%d staged=%s persisted=%s acked=%s",
		ph.jobID, f(ph.claim), f(ph.submit), f(ph.complete), f(ph.result), ph.arts, f(ph.staged), f(ph.persist), f(ph.ack))
}

// logPhases emits the phases-so-far line with a terminal/retry note; no-op on
// nil (W6: failed and error paths log too — the next anomaly must be readable
// from the dispatcher log alone, not just the happy path). The runner=
// prefix (#122) makes the line assignable in 3-runner parallel operation.
func (d *Dispatcher) logPhases(ph *jobPhases, note string) {
	if ph == nil {
		return
	}
	runner := d.cfg.RunnerName
	if runner == "" {
		runner = "-"
	}
	d.logger.Printf("phases[%s]: runner=%s %s", note, runner, ph.line())
}

// pollAndFinish drives a job from 'processing' to a terminal state: polls the
// processor status while renewing the lease, handles cancellation, then on
// completion fetches + minimally validates the result and marks completed.
func (d *Dispatcher) pollAndFinish(ctx context.Context, claimed *repo.ClaimedJob, ph *jobPhases) {
	if ph == nil {
		ph = &jobPhases{jobID: claimed.LeaseRef.JobID}
	}
	ref := claimed.LeaseRef
	fields := []any{ref.JobID, claimed.AttachmentID, claimed.DocumentID, claimed.Attempt}
	consecutive := 0
	// lastStage tracks the most recently observed poll stage; a change to it
	// publishes JobStageChanged (#167, poll-delta). Observer-only.
	lastStage := ""
	// Renewal decoupled from the poll cadence (L8 fix): one goroutine renews
	// for the WHOLE job lifetime — poll loop, result fetch, artifact staging
	// and persist. It stops on ctx or a lost lease; the fenced mutations plus
	// the claim scan's expired-recovery keep correctness when it stops early.
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	lost := make(chan struct{})
	go d.renewLoop(renewCtx, ref, fields, lost)
	for {
		if ctx.Err() != nil {
			return
		}
		// Stop early when renewal reports the lease lost (another owner took
		// over): further fenced marks would no-op anyway; the claim scan's
		// expired-recovery owns the row from here (optimization — the fence is
		// the correctness guard).
		select {
		case <-lost:
			d.logger.Printf("%v: lease lost while processing; not acknowledging", fields)
			return
		default:
		}
		// Honour a cancellation request before renewing/continuing: tell the
		// processor to stop and converge fenced to cancelled. The repo's
		// terminalize/claim scan would eventually cancel on lease expiry, but we
		// must not keep renewing a job an operator asked to stop.
		//
		// Silent-exit fix (L8 anomaly): a failed cancel-request read used to
		// bare-return, leaving the row 'processing' with a dying lease and NO
		// marking — a single transient DB hiccup abandoned the job. Every exit
		// from here on must mark (retry/terminal) or hand the row to recovery.
		cancelRequested, cerr := d.rep.JobCancelRequested(ctx, ref.JobID)
		if cerr != nil {
			d.scheduleRetry(ctx, ref, claimed.Attempt, "CANCEL_READ_FAILED", cerr.Error())
			return
		}
		if cancelRequested {
			if err := d.client.Cancel(ctx, ref.JobID); err != nil {
				d.logger.Printf("%v: processor cancel: %v", fields, err)
			}
			d.onCancelled(ctx, ref) // fenced MarkCancelled; lost lease no-ops
			return
		}

		// Lease renewal runs in its own goroutine (renewLoop) so a slow status
		// poll, the result fetch, artifact staging or the persist transaction
		// can never consume the renewal window — the L8 anomaly showed a 300s
		// poll budget starving the 300s lease mid-job, and onCompleted (hundreds
		// of artifact fetches) ran entirely without renewal until every fenced
		// mark no-op'd lost and the row stuck in 'processing' forever.
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
			ph.complete = time.Now()
			d.onCompleted(ctx, claimed, ph)
			return
		case "failed":
			if st.Error != nil {
				d.onFailed(ctx, claimed, st.Error, ph)
			} else {
				d.onFailed(ctx, claimed, &processor.JobError{Code: "PROCESS_FAILED", Message: "processor reported failure without error details", Retryable: true}, ph)
			}
			return
		case "cancelled":
			d.onCancelled(ctx, ref)
			return
		default: // accepted, running, advisory in-progress states
			// #167: observe a stage transition across polls (poll-delta). The
			// stage name doubles as a light progress hint; a full progress
			// contract is the runner's (x236).
			if st.Stage != lastStage {
				lastStage = st.Stage
				if st.Stage != "" {
					d.publish(events.JobStageChanged{
						JobID:        ref.JobID,
						Stage:        st.Stage,
						ProgressHint: st.Stage,
					})
				}
			}
			if !waitFor(ctx, jitter(d.cfg.RenewalInterval)) {
				return
			}
		}
	}
}

// repairTrackFailureCodes (#237): runner-reported failure codes that name
// a DOCUMENT defect (the file itself is unreadable) rather than an
// infrastructure fault. These go to the repair track: a repair case is
// created (the attachment row carries content_type, routing the fix.sh
// EPUB/PDF arm) instead of burning retries against a poison input.
var repairTrackFailureCodes = map[string]bool{
	"SOURCE_UNREADABLE": true,
}

// autoQueueRepairClasses (#238): suspicion classes whose repair case
// queues ITSELF at creation — strictly the clearly-repairable, per the
// design decision on the issue: preflight 🔴 reparierbar and the #237
// SOURCE_UNREADABLE track. Unpaginiert NEVER auto-queues (the design nail
// — it is not in this set, and repo.QueueRepairCase refuses it again on
// its own). EPUB red (defekt/DRM) stays on the manual repair API until
// the EPUB fixer arm has queue-proven efficacy; extending automation is
// a one-line addition here. Exact string match — the findings are runner
// contract values, not prefixes to guess from.
var autoQueueRepairClasses = map[string]bool{
	"🔴 reparierbar":     true,
	"SOURCE_UNREADABLE": true,
}

func (d *Dispatcher) onFailed(ctx context.Context, claimed *repo.ClaimedJob, jobErr *processor.JobError, ph *jobPhases) {
	ref := claimed.LeaseRef
	d.logPhases(ph, jobErr.Code)
	if jobErr.Retryable {
		d.scheduleRetry(ctx, ref, claimed.Attempt, jobErr.Code, jobErr.Message)
		return
	}
	// #237: document failures are repair-track, not infra-track — the
	// structured runner FAIL (error class, stage, retryable=false, message
	// excerpt) becomes the repair case's analysis so the fixer starts from
	// the actual corruption evidence.
	if repairTrackFailureCodes[jobErr.Code] {
		analysis, _ := json.Marshal(map[string]any{
			"error_class": jobErr.Code,
			"stage":       jobErr.Stage,
			"retryable":   false,
			"message":     jobErr.Message,
		})
		if c, created, err := d.rep.CreateRepairCase(ctx, claimed.AttachmentID, claimed.DocumentID, jobErr.Code, analysis); err != nil && !isLost(err) {
			d.logger.Printf("repair-case for %s: %v", ref.JobID, err)
		} else if created && c != nil && autoQueueRepairClasses[c.SuspicionClass] {
			// #238: only a FRESH case auto-queues (created == false means a
			// recycled open case — old evidence, never queued by a newer
			// verdict). Queue failure is logged, not fatal: the case stays
			// rejected, the manual path remains.
			if err := d.rep.QueueRepairCase(ctx, c.ID, c.SuspicionClass, c.Analysis); err != nil && !isLost(err) {
				d.logger.Printf("auto-queue repair case for %s: %v (stays rejected)", ref.JobID, err)
			} else if err == nil {
				d.logger.Printf("repair case auto-queued for %s (%s)", ref.JobID, c.SuspicionClass)
			}
		}
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
func (d *Dispatcher) onCompleted(ctx context.Context, claimed *repo.ClaimedJob, ph *jobPhases) {
	ref := claimed.LeaseRef
	resultBytes, err := d.client.JobResult(ctx, ref.JobID)
	ph.result = time.Now()
	if err != nil {
		// We cannot complete without the result.
		d.logPhases(ph, "RESULT_FETCH_FAILED")
		d.markTerminal(ctx, ref, "RESULT_FETCH_FAILED", err.Error())
		return
	}
	// Minimal structural check: the result must echo the job id. Full §14
	// validation runs inside PersistResult before any row is inserted.
	var resMeta struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(resultBytes, &resMeta); err != nil || resMeta.JobID != ref.JobID {
		d.logPhases(ph, "RESULT_INVALID")
		d.markTerminal(ctx, ref, "RESULT_INVALID", "processor result does not echo job_id")
		return
	}

	// Stage + verify the durable artifacts (§13/§14.4.6): fetch each artifact's
	// bytes, hash + length-check against the result's declaration, commit via an
	// atomic rename under ArtifactRoot. A digest/size mismatch makes the job
	// terminal (validation failure) before any snapshot row is inserted.
	arts, aerr := d.stageArtifacts(ctx, ref.JobID, resultBytes)
	ph.staged = time.Now()
	ph.arts = len(arts)
	if aerr != nil {
		d.logPhases(ph, "RESULT_INVALID")
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
	ph.persist = time.Now()
	if err != nil {
		d.logPhases(ph, "RESULT_PERSIST_FAILED")
		d.markTerminal(ctx, ref, "RESULT_PERSIST_FAILED", err.Error())
		return
	}

	// #167: the result is durably persisted and fenced-completed — the job is
	// done regardless of the ACK below (F3 never reprocesses). Observer-only.
	d.publish(events.JobCompleted{JobID: ref.JobID, Took: time.Since(ph.claim)})

	// Acknowledge after durable commit. ACK failure keeps the job completed but
	// marks ack_pending so the separate retry pass re-acknowledges it; it is
	// never reprocessed (F3).
	if err := d.client.Ack(ctx, ref.JobID, processor.Ack{Persisted: true, SnapshotID: snapshotID}); err != nil {
		d.logger.Printf("%v: ack failed; will retry separately: %v", []any{ref.JobID}, err)
		if merr := d.rep.MarkAckFailed(ctx, ref.JobID); merr != nil {
			d.logger.Printf("%v: mark ack-pending: %v", []any{ref.JobID}, merr)
		}
		// W5: ph.ack stays unset — the line honestly reports '-' instead of
		// stamping a moment where no ack succeeded.
		d.logPhases(ph, "ack_pending")
		return
	}
	ph.ack = time.Now()
	d.logPhases(ph, "ok")
}

// renewLoop renews the job's lease on RenewalInterval until ctx is done —
// decoupled from the poll cadence (L8 fix) so slow status polls, the result
// fetch, artifact staging and the persist transaction can never consume the
// renewal window. Transient renew failures are logged and retried (the lease
// may still be alive); a lost lease closes `lost` once so the poll loop stops
// early — the claim scan's expired-recovery then owns the row.
func (d *Dispatcher) renewLoop(ctx context.Context, ref repo.LeaseRef, fields []any, lost chan struct{}) {
	ticker := time.NewTicker(jitter(d.cfg.RenewalInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.rep.RenewLease(ctx, ref, d.cfg.LeaseDuration); err != nil {
				if isLost(err) {
					d.logger.Printf("%v: renewal: lease lost", fields)
					close(lost) // exactly once: this is the only close site
					return
				}
				// Transient (DB hiccup): keep trying; if the lease really expires,
				// the next renew reports lost and the claim recovery takes over.
				d.logger.Printf("%v: renewal failed (transient): %v", fields, err)
			}
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
