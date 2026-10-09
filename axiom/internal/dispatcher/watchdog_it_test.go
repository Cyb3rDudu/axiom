// watchdog_it_test.go — #369 progress-coupled liveness: a holder that keeps
// answering "running" with a FROZEN position loses the job once the
// no-progress bound elapses; advancing position keeps the lease alive; the
// stalled holder never starves the queue behind it. All probes run against
// the real repo (fenced retry semantics) with the fake processor scripted
// into the observed production shapes.
package dispatcher

import (
	"context"
	"io"
	"log"
	"os"
	"testing"
	"time"
)

// TestNoProgressWatchdogEvictsStalledHolder — the deterministic repro the
// issue demands: heartbeats continue (the fake answers every poll) but the
// position never advances. Within the bound the job must be evicted through
// the existing retry path (pending + backoff, attempt advanced) and the
// runner job cancelled (orphan compute stop).
func TestNoProgressWatchdogEvictsStalledHolder(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R1", 99)
	fp := newFakeProcessor(t)
	fp.statuses = repeatStr("running", 2000) // never completes on its own
	fp.frozenProgress = 7                    // position pinned at 7/100

	d := newDispatcher(t, h, fp, Config{
		Concurrency: 1, LeaseDuration: 2 * time.Second,
		RenewalInterval: 50 * time.Millisecond, NoProgressLimit: 400 * time.Millisecond,
	})
	runFor(t, d, context.Background(), 4*time.Second)

	if got := h.jobAttempt(t, jobID); got < 2 {
		t.Fatalf("attempt = %d, want >= 2 (stalled holder must be evicted and retried)", got)
	}
	fp.mu.Lock()
	cancels := fp.cancelHits
	fp.mu.Unlock()
	if cancels < 1 {
		t.Fatal("watchdog never cancelled the runner job (orphan compute stop missing)")
	}
	if got := h.jobStatus(t, jobID); got == "completed" {
		t.Fatal("a never-progressing job must not complete")
	}
}

// TestWatchdogDisabledRenewsUnconditionally — the negative control (and the
// mutation probe for the watchdog): with NoProgressLimit=0 the same frozen
// holder keeps its lease renewed — holder existence suffices, the pre-#369
// behavior pinned as the disabled mode.
func TestWatchdogDisabledRenewsUnconditionally(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R1", 99)
	fp := newFakeProcessor(t)
	fp.statuses = repeatStr("running", 2000)
	fp.frozenProgress = 7

	d := newDispatcher(t, h, fp, Config{
		Concurrency: 1, LeaseDuration: 2 * time.Second,
		RenewalInterval: 50 * time.Millisecond, NoProgressLimit: 0, // disabled
	})
	runFor(t, d, context.Background(), 2*time.Second)

	if got := h.jobAttempt(t, jobID); got != 1 {
		t.Fatalf("attempt = %d, want 1 (disabled watchdog must not evict)", got)
	}
	if !h.leaseWasRenewed(t, jobID) {
		t.Fatal("disabled watchdog must keep renewing (heartbeat-only liveness)")
	}
}

// TestAdvancingProgressSuppressesWatchdog — the legitimate-heavy-leg probe:
// one stage, position advancing every poll, run lasting far beyond the
// bound: the lease stays alive and the job completes.
func TestAdvancingProgressSuppressesWatchdog(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = repeatStr("running", 60)
	fp.statuses = append(fp.statuses, "completed")
	fp.progressStep = 5 // position advances 5/poll within ONE stage
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{
		Concurrency: 1, LeaseDuration: 2 * time.Second,
		RenewalInterval: 25 * time.Millisecond, NoProgressLimit: 300 * time.Millisecond,
	})
	runFor(t, d, context.Background(), 8*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed (advancing position must never trip the bound)", got)
	}
	if !h.leaseWasRenewed(t, jobID) {
		t.Fatal("advancing job was never renewed")
	}
}

// TestStalledJobDoesNotStarveQueue — lane fairness (the wave incident: one
// hung document blocked three healthy ones on a concurrency=1 lane). The
// stalled holder self-evicts at the bound; the healthy job behind it is
// claimed while the stalled one sits in retry backoff.
func TestStalledJobDoesNotStarveQueue(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	stalled := h.seedJob(t, "R1", 99)
	healthy := h.seedJob(t, "R2", 3)
	fp := newFakeProcessor(t)
	fp.stallJobID = stalled // only the stalled job freezes; everything else completes
	fp.statuses = repeatStr("running", 3000)
	fp.result = `{"contract_version":"1.0","job_id":"` + healthy + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{
		Concurrency: 1, LeaseDuration: 2 * time.Second,
		RenewalInterval: 50 * time.Millisecond, NoProgressLimit: 400 * time.Millisecond,
	})
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, healthy); got != "completed" {
		t.Fatalf("healthy job status = %q, want completed (stalled holder must not starve the lane)", got)
	}
	if got := h.jobAttempt(t, stalled); got < 2 {
		t.Fatalf("stalled job attempt = %d, want >= 2 (evicted at least once)", got)
	}
	if got := h.jobStatus(t, stalled); got == "completed" {
		t.Fatal("the never-completing stalled job must not complete")
	}
}

// TestJobProgressMirroredToJobsRow — coarse visibility: phase + position
// observed by the poll loop land on the jobs row (what the jobs endpoint
// serves): the operator's "stille Arbeit vs Hänger" discriminator.
func TestJobProgressMirroredToJobsRow(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "R1", 3)
	fp := newFakeProcessor(t)
	fp.statuses = repeatStr("running", 30)
	fp.statuses = append(fp.statuses, "completed")
	fp.progressStep = 5
	fp.stages = []string{"captions"}
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	d := newDispatcher(t, h, fp, Config{
		Concurrency: 1, LeaseDuration: 2 * time.Second,
		RenewalInterval: 25 * time.Millisecond, NoProgressLimit: time.Hour,
	})
	if os.Getenv("WATCHDOG_DEBUG") != "" {
		d.logger = log.New(os.Stderr, "dbg: ", log.LstdFlags|log.Lmicroseconds)
	}
	runFor(t, d, context.Background(), 8*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("job status = %q, want completed (mirror test needs the full run)", got)
	}

	var phase *string
	var done, total *int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT progress_phase, progress_done, progress_total FROM ingest_jobs WHERE id=$1`, jobID,
	).Scan(&phase, &done, &total); err != nil {
		t.Fatalf("read progress columns: %v", err)
	}
	if phase == nil || *phase != "captions" {
		t.Fatalf("progress_phase = %v, want captions", phase)
	}
	if done == nil || *done < 5 {
		t.Fatalf("progress_done = %v, want >= 5 (position mirrored)", done)
	}
	if total == nil || *total != 1000 {
		t.Fatalf("progress_total = %v, want 1000", total)
	}
	// The repo surface the endpoint serves carries them too.
	jobs, err := h.rep.ListJobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs {
		if j.ID == jobID {
			if j.ProgressPhase == nil || j.ProgressDone == nil {
				t.Fatal("ListJobs does not surface progress columns")
			}
			return
		}
	}
	t.Fatal("seeded job not found in ListJobs")
}

// jobAttempt already exists in dispatcher_integration_test.go.
var _ = log.New(io.Discard, "", 0) // keep imports honest if probes shrink
