// Package dispatcher runs the claim-to-processor loop for axiom-ng: it claims
// eligible ingest jobs, submits them to a document processor over the contract
// v1 HTTP API, drives them to a terminal state and acknowledges durable results.
// It runs ONLY when explicitly started (binary config or a test); there is no
// implicit background loop and tests never start one unintentionally.
package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
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
	// RunnerName is the human identity of the processor this dispatcher
	// drives (#122): goes into the phases log line (runner=...) and into
	// ingest_jobs.processor_name at claim time. Empty falls back to the
	// processor URL host (wired in main).
	RunnerName string
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
	// AckRetryInterval is how often the separate ack-retry pass looks for
	// completed jobs with a pending acknowledgement.
	AckRetryInterval time.Duration
	// Profile is the processing profile to freeze at claim time.
	Profile json.RawMessage
	// ArtifactRoot is the durable derived-artifact root (AXIOMNG_ARTIFACT_ROOT).
	// Verified processor artifacts are staged here then atomically renamed on the
	// same filesystem (work-order §7). Empty disables artifact commit (jobs whose
	// result declares durable artifacts then fail validation).
	ArtifactRoot string

	// OpenSearchURL enables the L5 outbox drainer when non-empty. Empty
	// disables the worker entirely (outbox rows stay pending, no error).
	OpenSearchURL string
	// OpenSearchUsername/Password are optional basic-auth credentials; empty
	// means anonymous (the local mothership runs without auth).
	OpenSearchUsername string
	OpenSearchPassword string

	// ProcessorSourceBaseURL is the externally reachable base URL of
	// axiom-ng's /api/processor/source endpoint (e.g. the Tailnet address).
	// Non-empty (plus ProcessorSourceSecret) makes the dispatcher attach a
	// signed source_url to every process request — remote source delivery.
	ProcessorSourceBaseURL string
	// ProcessorSourceSecret is the shared HMAC secret for source URLs; must
	// match the server's AXIOMNG_PROCESSOR_SOURCE_SECRET.
	ProcessorSourceSecret string
}

// Dispatcher owns the worker pool and the lease/processor plumbing.
type Dispatcher struct {
	cfg    Config
	rep    *repo.Repo
	client *processor.Client
	logger *log.Logger
	// persist is the durability boundary for completed results; nil means jobs
	// that reach completion FAIL rather than being completed+acked without a
	// durable snapshot (Gate 2 F1). Tests inject a recording fake.
	persist ResultPersister
	// caps is the negotiated processor capability set; set once in Run before any
	// claim is dispatched and used to bound concurrency and validate each job.
	caps *processor.Capabilities
}

// New builds a Dispatcher. It starts no goroutines; call Run to process. It uses
// an error persister by default (no durable storage until Gate 4), so no job can
// be completed/acked without a real persistence boundary.
func New(rep *repo.Repo, client *processor.Client, cfg Config, logger *log.Logger) *Dispatcher {
	return NewWithPersister(rep, client, &errPersister{msg: "no result persister configured (Gate 2: persistence arrives in Gate 4)"}, cfg, logger)
}

// NewWithPersister builds a Dispatcher with an explicit result-persistence
// boundary (tests supply a recording fake; Gate 4 will supply the real one).
func NewWithPersister(rep *repo.Repo, client *processor.Client, persist ResultPersister, cfg Config, logger *log.Logger) *Dispatcher {
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
	if cfg.AckRetryInterval <= 0 {
		cfg.AckRetryInterval = 30 * time.Second
	}
	if logger == nil {
		logger = log.New(log.Writer(), "axiom-ng: dispatcher: ", log.LstdFlags)
	}
	return &Dispatcher{cfg: cfg, rep: rep, client: client, logger: logger, persist: persist}
}

// Run processes jobs until ctx is cancelled. It returns when all workers have
// drained their current jobs and shut down gracefully. Capability negotiation
// is required (work order section 7 step 5): this fails fast on a broken or
// unsupported processor so claims are never held hostage by one, and it clamps
// the configured concurrency to the processor's declared maximum.
func (d *Dispatcher) Run(ctx context.Context) error {
	// L5: OpenSearch outbox drainer — own goroutine, own path; an OpenSearch
	// outage never touches snapshots or jobs (work order §10.3). Starts BEFORE
	// capability negotiation: draining has no processor dependency, and a
	// processor outage must not stall outbox draining either. Empty URL
	// disables it (rows just stay pending).
	if d.cfg.OpenSearchURL != "" {
		osc := newOpenSearchClient(d.cfg.OpenSearchURL, d.cfg.OpenSearchUsername, d.cfg.OpenSearchPassword, d.logger)
		go outboxWorker(ctx, d, osc)
		d.logger.Printf("outbox drainer enabled: index=%s url=%s", outboxIndexName, d.cfg.OpenSearchURL)
	} else {
		d.logger.Printf("outbox drainer disabled (AXIOMNG_OPENSEARCH_URL empty); outbox rows stay pending")
	}
	caps, err := d.client.Capabilities(ctx)
	if err != nil {
		d.logger.Printf("capability negotiation failed: %v", err)
		return fmt.Errorf("negotiate capabilities: %w", err)
	}
	if !supportsContract(caps) {
		d.logger.Printf("processor does not support contract v1 (versions=%v)", caps.ContractVersions)
		return fmt.Errorf("processor does not support contract v1")
	}
	if caps.Limits.MaxConcurrentJobs > 0 && d.cfg.Concurrency > caps.Limits.MaxConcurrentJobs {
		d.logger.Printf("clamping concurrency %d -> %d (processor max)", d.cfg.Concurrency, caps.Limits.MaxConcurrentJobs)
		d.cfg.Concurrency = caps.Limits.MaxConcurrentJobs
	}
	d.caps = caps
	// Separate ack-retry pass: re-acknowledges completed jobs whose ack failed,
	// never reprocessing them (F3). Runs until ctx is cancelled.
	go retryAcks(ctx, d)
	var wg sync.WaitGroup
	for i := 0; i < d.cfg.Concurrency; i++ {
		wg.Add(1)
		go d.worker(ctx, &wg, i)
	}
	wg.Wait()
	return nil
}

// supportsContract reports whether any advertised contract version is a v1-minor
// this client can speak (additive v1 evolution).
func supportsContract(caps *processor.Capabilities) bool {
	for _, v := range caps.ContractVersions {
		if v == "1.0" || v == "1.1" || v == "1.2" || v == "1.3" || v == "1.4" || v == "1.5" {
			return true
		}
	}
	return false
}

func (d *Dispatcher) worker(ctx context.Context, wg *sync.WaitGroup, slot int) {
	defer wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		claimed, err := d.rep.ClaimNextJob(ctx, repo.ClaimOptions{
			WorkerID:      d.cfg.WorkerID,
			RunnerName:    d.cfg.RunnerName,
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
		// On graceful shutdown the driveJob may have aborted mid-flight (ctx
		// cancelled) leaving the lease held; return the job to pending so another
		// dispatcher or this one on restart can reclaim it. Releasing is safe even
		// if the job already reached a terminal state (the fence no-ops).
		if ctx.Err() != nil {
			d.releaseLease(claimed)
		}
	}
}

// releaseLease returns a still-held lease to pending (or terminalizes at the
// attempt ceiling) so in-flight work is not stranded by a shutdown. It builds a
// fresh bounded context because the shutdown ctx is already cancelled and would
// abort the DB update.
func (d *Dispatcher) releaseLease(claimed *repo.ClaimedJob) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.rep.ReleaseOrExpireLease(rctx, claimed.LeaseRef); err != nil && !isLost(err) {
		d.logger.Printf("%v: release lease on shutdown: %v", claimed.LeaseRef.JobID, err)
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

	ph := &jobPhases{jobID: ref.JobID, claim: time.Now()}
	req, err := buildRequest(claimed.InputSnapshot, SourceURLOptions{
		BaseURL:    d.cfg.ProcessorSourceBaseURL,
		Secret:     d.cfg.ProcessorSourceSecret,
		LeaseUntil: claimed.LeaseUntil,
	})
	if err != nil {
		d.markNotProcessable(ctx, ref, fields, err)
		return
	}

	// Capability check: the processor must support this job's content type and the
	// requested processing features. Unsupported is a terminal, non-transient
	// condition (the frozen snapshot cannot change at retry).
	if reason := d.trimCapabilityReason(req); reason != "" {
		d.markNotProcessable(ctx, ref, fields, fmt.Errorf("unsupported by processor: %s", reason))
		return
	}

	if _, err := d.client.SubmitProcess(ctx, req); err != nil {
		d.handleSubmitFailure(ctx, claimed, err)
		return
	}
	ph.submit = time.Now()

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
	d.pollAndFinish(ctx, claimed, ph)
}

// trimCapabilityReason returns a non-empty reason if the negotiated capability
// set cannot serve the request (unsupported content type or a requested feature
// the processor does not advertise). Empty string means the job is supported.
func (d *Dispatcher) trimCapabilityReason(req *processor.ProcessRequest) string {
	if d.caps == nil {
		return "no negotiated capabilities"
	}
	supported := func(f string) bool {
		if d.caps.Features == nil {
			return false
		}
		ok, _ := d.caps.Features[f]
		return ok
	}
	ct := req.Attachment.ContentType
	formatOK := false
	if ct != "" {
		for _, f := range d.caps.Formats {
			if f == ct {
				formatOK = true
				break
			}
		}
	}
	if !formatOK {
		return fmt.Sprintf("content type %q not in formats %v", ct, d.caps.Formats)
	}
	rs := req.Processing
	switch {
	case rs.ComputeDenseEmbeddings && !supported("dense_embeddings"):
		return "dense_embeddings not supported"
	case rs.ComputeSparseEmbeddings && !supported("sparse_embeddings"):
		return "sparse_embeddings not supported"
	case rs.ExtractEntities && !supported("entities"):
		return "entities not supported"
	case rs.ExtractRelationships && !supported("entity_relationships"):
		return "entity_relationships not supported"
	}
	return ""
}

// markNotProcessable schedules a retry/terminal for a job whose frozen snapshot
// is unusable. Such a defect is not transient, so it becomes terminal failed.
func (d *Dispatcher) markNotProcessable(ctx context.Context, ref repo.LeaseRef, fields []any, cause error) {
	d.logger.Printf("%v: not processable: %v", fields, cause)
	if err := d.rep.MarkFailed(ctx, ref, "NOT_PROCESSABLE", cause.Error()); err != nil && !isLost(err) {
		d.logger.Printf("%v: mark failed: %v", fields, err)
	}
}
