// Package dispatcher runs the claim-to-processor loop for axiom-ng: it claims
// eligible ingest jobs, submits them to a document processor over the contract
// v1 HTTP API, drives them to a terminal state and acknowledges durable results.
// It runs ONLY when explicitly started (binary config or a test); there is no
// implicit background loop and tests never start one unintentionally.
package dispatcher

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// Config controls a Dispatcher.
type Config struct {
	// WorkerID is the stable identity this process uses for claims. Empty
	// defaults to a stable host-process-scoped id.
	WorkerID string
	// Concurrency is the number of parallel claim/process worker slots, bounded
	// by the processor's declared MaxConcurrentJobs.
	Concurrency int
	// PollInterval with jitter governs how long a worker waits when no job is
	// claimable.
	PollInterval time.Duration
	// LeaseDuration is the base lease for each claim.
	LeaseDuration time.Duration
	// RenewalInterval is how often a running job's lease is renewed while the
	// processor works.
	RenewalInterval time.Duration
	// MaxRetryBackoff caps the exponential backoff scheduled on retryable
	// processor failure.
	MaxRetryBackoff time.Duration
	// Profile is the processing profile to freeze at claim time.
	Profile json.RawMessage
	// RequireCapabilities, when true, checks the processor version + features
	// before dispatch and treats an unsupported processor as fatal for the job.
	RequireCapabilities bool
}

// Dispatcher owns the worker pool and the lease/processor plumbing.
type Dispatcher struct {
	cfg    Config
	rep    *repo.Repo
	client *processor.Client
	logger *log.Logger
}

// New builds a Dispatcher. It starts no goroutines; call Run to process.
func New(rep *repo.Repo, client *processor.Client, cfg Config, logger *log.Logger) *Dispatcher {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "axiom-ng"
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.RenewalInterval <= 0 {
		cfg.RenewalInterval = cfg.LeaseDuration / 3
		if cfg.RenewalInterval < time.Second {
			cfg.RenewalInterval = time.Second
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.MaxRetryBackoff <= 0 {
		cfg.MaxRetryBackoff = 5 * time.Minute
	}
	if logger == nil {
		logger = log.New(log.Writer(), "axiom-ng: dispatcher: ", log.LstdFlags)
	}
	return &Dispatcher{cfg: cfg, rep: rep, client: client, logger: logger}
}

// Run processes jobs until ctx is cancelled. It returns when all workers have
// drained their current jobs and shut down gracefully.
func (d *Dispatcher) Run(ctx context.Context) error {
	// Negotiate capabilities once up front; if capabilities are required and the
	// processor is unhealthy, fail fast so a broken processor is visible instead
	// of silently stalling every claim.
	if d.cfg.RequireCapabilities {
		if _, err := d.client.Capabilities(ctx); err != nil {
			d.logger.Printf("capability negotiation failed: %v", err)
			return err
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < d.cfg.Concurrency; i++ {
		wg.Add(1)
		go d.worker(ctx, &wg, i)
	}
	wg.Wait()
	return nil
}

func (d *Dispatcher) worker(ctx context.Context, wg *sync.WaitGroup, slot int) {
	defer wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := d.rep.ClaimNextJob(ctx, repo.ClaimOptions{
			WorkerID:      d.cfg.WorkerID,
			LeaseDuration: d.cfg.LeaseDuration,
			Profile:       d.cfg.Profile,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.Printf("slot %d: claim error: %v", slot, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(d.cfg.PollInterval)):
			}
			continue
		}
		if claimed == nil {
			// Nothing to do; wait for the poll interval (with jitter).
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(d.cfg.PollInterval)):
			}
			continue
		}
		d.driveJob(ctx, claimed)
	}
}

// driveJob takes a claimed job through the full lifecycle. On ErrLostLease it
// stops immediately and never acknowledges, leaving recovery to the claim scan.
func (d *Dispatcher) driveJob(ctx context.Context, claimed *repo.ClaimedJob) {
	ref := claimed.LeaseRef
	tokenPrefix := ref.LeaseToken
	if len(tokenPrefix) > 8 {
		tokenPrefix = tokenPrefix[:8]
	}
	fields := []any{ref.JobID, claimed.AttachmentID, claimed.DocumentID, claimed.Attempt, tokenPrefix}

	req, err := buildRequest(claimed.InputSnapshot)
	if err != nil {
		d.markNotProcessable(ctx, ref, fields, err)
		return
	}

	if _, err := d.client.SubmitProcess(ctx, req); err != nil {
		d.handleSubmitFailure(ctx, claimed, err)
		return
	}

	// Fenced claimed -> processing after acceptance.
	if err := d.rep.MarkProcessing(ctx, ref); err != nil {
		if isLost(err) {
			d.logger.Printf("%v: lost lease after accept; not acknowledging", fields)
			return
		}
		d.logger.Printf("%v: mark processing: %v", fields, err)
		return
	}

	// Poll the processor while renewing the lease.
	d.pollAndFinish(ctx, claimed)
}

// markNotProcessable schedules a retry/terminal for a job whose frozen snapshot
// is unusable. Such a defect is not transient, so it becomes terminal failed.
func (d *Dispatcher) markNotProcessable(ctx context.Context, ref repo.LeaseRef, fields []any, cause error) {
	d.logger.Printf("%v: not processable: %v", fields, cause)
	if err := d.rep.MarkFailed(ctx, ref, "NOT_PROCESSABLE", cause.Error()); err != nil && !isLost(err) {
		d.logger.Printf("%v: mark failed: %v", fields, err)
	}
}
