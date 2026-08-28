// Package dispatcher runs the claim-to-processor loop for axiom-ng: it claims
// eligible ingest jobs, submits them to a document processor over the contract
// v1 HTTP API, drives them to a terminal state and acknowledges durable results.
// It runs ONLY when explicitly started (binary config or a test); there is no
// implicit background loop and tests never start one unintentionally.
package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
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
	// ingest_jobs.runner_name at claim time. Empty falls back to the
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
	// StartupRetryInterval is how long to wait between capability-negotiation
	// attempts at start while the processor is unreachable (#214). Leaving it
	// zero defaults it (5s).
	StartupRetryInterval time.Duration
	// MaxStartupWait caps how long the dispatcher retries capability
	// negotiation before giving up. A processor that stays unreachable past
	// this window is fatal (#214): Run returns the error and main must exit
	// non-zero so the supervisor restarts the process.
	MaxStartupWait time.Duration
	// Profile is the processing profile to freeze at claim time.
	Profile json.RawMessage
	// ArtifactRoot is the durable derived-artifact root (AXIOM_ARTIFACT_ROOT).
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
	// match the server's AXIOM_PROCESSOR_SOURCE_SECRET.
	ProcessorSourceSecret string

	// PreflightEnabled (#175): when set, a claimed job's source PDF is sent
	// to the runner's /v1/pdf/preflight BEFORE full processing. A failed
	// preflight (quality gate red) skips the job (status=skipped, reason =
	// preflight) and marks the attachment as a repair-case candidate instead
	// of producing junk chunks. When unset, no preflight runs (jobs process
	// as today). Each preflight call is bounded by the processor client's
	// small-call budget (15s).
	PreflightEnabled bool
}

// processorClient is the runner surface the dispatcher drives. The concrete
// *processor.Client satisfies it; processor.FailoverClient (R4 #134) wraps
// primary + local fallback behind the same shape.
type processorClient interface {
	Capabilities(ctx context.Context) (*processor.Capabilities, error)
	SubmitProcess(ctx context.Context, req *processor.ProcessRequest) (*processor.ProcessAccepted, error)
	// #175 quality gate; #220: routes EPUB bytes via the content type.
	Preflight(ctx context.Context, doc []byte, contentType string) (*processor.PreflightReport, error)
	JobStatus(ctx context.Context, jobID string) (*processor.JobStatus, error)
	JobResult(ctx context.Context, jobID string) ([]byte, error)
	Artifact(ctx context.Context, jobID, ref string) ([]byte, error)
	Cancel(ctx context.Context, jobID string) error
	Ack(ctx context.Context, jobID string, ack processor.Ack) error
}

// Dispatcher owns the worker pool and the lease/processor plumbing.
type Dispatcher struct {
	cfg    Config
	rep    *repo.Repo
	client processorClient
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
func New(rep *repo.Repo, client processorClient, cfg Config, logger *log.Logger) *Dispatcher {
	return NewWithPersister(rep, client, &errPersister{msg: "no result persister configured (Gate 2: persistence arrives in Gate 4)"}, cfg, logger)
}

// NewWithPersister builds a Dispatcher with an explicit result-persistence
// boundary (tests supply a recording fake; Gate 4 will supply the real one).
func NewWithPersister(rep *repo.Repo, client processorClient, persist ResultPersister, cfg Config, logger *log.Logger) *Dispatcher {
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
	if cfg.StartupRetryInterval <= 0 {
		cfg.StartupRetryInterval = 5 * time.Second
	}
	if cfg.MaxStartupWait <= 0 {
		cfg.MaxStartupWait = 2 * time.Minute
	}
	if logger == nil {
		logger = log.New(log.Writer(), "axiom-ng: dispatcher: ", log.LstdFlags)
	}
	return &Dispatcher{cfg: cfg, rep: rep, client: client, logger: logger, persist: persist}
}

// Run processes jobs until ctx is cancelled. It returns when all workers have
// drained their current jobs and shut down gracefully. Capability negotiation
// is required (work order section 7 step 5): it fails fast on a broken or
// unsupported (contract-violating) processor so claims are never held hostage
// by one, while a merely unreachable processor is retried with backoff up to
// MaxStartupWait before turning fatal (#214). It clamps the configured
// concurrency to the processor's declared maximum.
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
		d.logger.Printf("outbox drainer disabled (AXIOM_OPENSEARCH_URL empty); outbox rows stay pending")
	}
	// #214: capability negotiation MUST NOT be fatal while the processor is
	// simply not up yet — a rolling restart starts the Dispatcher before the
	// runner finishes booting, and a one-shot fail left the process alive
	// (exit 0) with a dead claim loop: jobs pending forever with no crash or
	// log. Retry with backoff until a candidate is reachable or MaxStartupWait
	// elapses; only then is the still-unreachable processor fatal (the caller
	// must surface that as a non-zero process exit so the supervisor restarts
	// the whole process).
	caps, err := d.negotiateCapabilities(ctx)
	if err != nil {
		// A shutdown that lands during the startup retry window is graceful
		// (exit 0), not a fatal runner outage — the supervisor must not see a
		// non-zero exit for an operator-initiated stop.
		if ctx.Err() != nil {
			return nil
		}
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

// negotiateCapabilities retries capability negotiation with backoff until a
// candidate is reachable, the context is cancelled, or MaxStartupWait elapses
// (#214). Transport-level failures (connection refused while a runner boots)
// are retried; a contract rejection (a candidate answers but does not speak
// contract v1) is not transient and fails immediately like the negotiation on
// a reachable-but-wrong processor would.
func (d *Dispatcher) negotiateCapabilities(ctx context.Context) (*processor.Capabilities, error) {
	deadline := time.Now().Add(d.cfg.MaxStartupWait)
	for attempts := 1; ; attempts++ {
		// Bound each attempt to the retry cadence so a runner that accepts a TCP
		// connection but never answers cannot hang a single attempt past the
		// overall window (a bare unstarted socket blocks in the connect handshake
		// rather than refusing).
		attemptCtx, attemptCancel := context.WithTimeout(ctx, d.cfg.StartupRetryInterval)
		caps, err := d.client.Capabilities(attemptCtx)
		attemptCancel()
		if err == nil {
			if attempts > 1 {
				d.logger.Printf("capability negotiation succeeded after %d attempt(s)", attempts)
			}
			return caps, nil
		}
		// A shutdown that arrives mid-attempt is graceful, not a runner outage.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The per-attempt timeout surfaces as ErrCancelled even though the
		// processor simply isn't answering yet — that is retryable. Only a hard
		// error (a reachable candidate rejects the request outright) is
		// non-transient.
		if !processor.FailoverClass(err) && !errors.Is(err, processor.ErrCancelled) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			d.logger.Printf("capability negotiation failed after %d attempt(s) over %s: %v", attempts, d.cfg.MaxStartupWait, err)
			return nil, err
		}
		d.logger.Printf("capability negotiation attempt %d failed (processor not up yet): %v; retrying in %s", attempts, err, d.cfg.StartupRetryInterval)
		if !waitFor(ctx, d.cfg.StartupRetryInterval) {
			return nil, ctx.Err()
		}
	}
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

	// #175 preflight quality gate: BEFORE full processing, a claimed job's
	// source PDF is sent to the runner's /v1/pdf/preflight. A failed gate
	// (quality red) skips the job (status=skipped, reason=preflight) and
	// marks the attachment as a repair-case candidate instead of producing
	// junk chunks. Advisory by design: an unassessable doc (unreadable local
	// path, runner preflight error) proceeds normally — preflight never
	// blocks a job it cannot measure.
	if d.cfg.PreflightEnabled && d.preflightGate(ctx, claimed, req) {
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

// preflightGate (#175) runs the runner's /v1/pdf/preflight on the claimed job's
// source PDF and applies the quality policy. Returns true if the job was
// handled (skipped + repair-cased) and must NOT proceed to SubmitProcess;
// false means proceed normally. The gate is advisory: it never blocks a job it
// cannot assess (unreadable local source, runner preflight error).
func (d *Dispatcher) preflightGate(ctx context.Context, claimed *repo.ClaimedJob, req *processor.ProcessRequest) bool {
	ref := claimed.LeaseRef
	fields := []any{ref.JobID, claimed.AttachmentID, claimed.DocumentID, claimed.Attempt}

	if req.Attachment.LocalPath == "" {
		d.logger.Printf("%v: preflight skipped — no local source path available", fields)
		return false // proceed: no bytes to assess
	}
	// local_path may carry a file:// prefix (Zotero convention) — strip it
	// before reading, mirroring the fixer invoker's TrimPrefix pattern.
	local := strings.TrimPrefix(req.Attachment.LocalPath, "file://")
	pdf, err := os.ReadFile(local)
	if err != nil {
		d.logger.Printf("%v: preflight skipped — cannot read source %s: %v", fields, local, err)
		return false // proceed: advisory only, never block unassessable
	}
	report, err := d.client.Preflight(ctx, pdf, req.Attachment.ContentType)
	if err != nil {
		d.logger.Printf("%v: preflight skipped — runner preflight error: %v", fields, err)
		return false // proceed: a broken quality gate must not hold a job hostage
	}

	qs := map[string]any{
		"verdict":             boolMap(report.Ok, "pass", "fail"),
		"verdacht":            report.Verdacht,
		"grund":               report.Grund,
		"pages":               report.Details["pages"],
		"text_layer":          report.Details["text_layer"],
		"mean_chars_per_page": report.Details["mean_chars_per_page"],
		"suspicious_patterns": report.Details["suspicious_patterns"],
		// #220: the EPUB branch of the same gate — format flag plus the
		// epubcheck/DRM findings, so repair-case analysis carries the
		// conformance reasons (epubcheck messages live in details).
		"format":     report.Details["format"],
		"drm":        report.Details["drm"],
		"opf_spine":  report.Details["opf_spine"],
		"epubcheck":  report.Details["epubcheck"],
	}
	qsJSON, _ := json.Marshal(qs)
	if err := d.rep.SetQualityState(ctx, ref, qsJSON); err != nil && !isLost(err) {
		d.logger.Printf("%v: set quality_state: %v", fields, err)
	}

	if report.Ok {
		d.logger.Printf("%v: preflight PASS (%s)", fields, report.Verdacht)
		return false // quality green → proceed to full processing
	}

	// Quality red: do not produce junk chunks. Skip the job with a clear
	// reason and mark the attachment as a repair-case candidate (#206/#203).
	reason := "preflight:" + report.Verdacht
	d.logger.Printf("%v: preflight FAIL (%s) — skipping job, marking repair candidate", fields, reason)
	if _, err := d.rep.CreateRepairCase(ctx, claimed.AttachmentID, claimed.DocumentID, report.Verdacht, qsJSON); err != nil && !isLost(err) {
		d.logger.Printf("%v: repair-case: %v", fields, err)
	}
	if err := d.rep.MarkSkipped(ctx, ref, reason); err != nil && !isLost(err) {
		d.logger.Printf("%v: mark skipped: %v", fields, err)
	}
	return true // handled: skip SubmitProcess
}

// boolMap renders a two-state verdict into a compact string.
func boolMap(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}
