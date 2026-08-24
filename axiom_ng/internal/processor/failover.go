// Package processor — ingest failover chain (epic #130 R4, #134; generalized
// to an ordered candidate list in #207).
//
// Role model: AXIOM_PROCESSOR_URLS (or the legacy singular URL plus fallback
// variable) defines an ORDERED chain of ingest runners — Carrier GPU first
// when present, the local always-on runner as the every-installation floor.
// A periodic health probe keeps dead candidates out of the front of the
// submit path, and submit-time failover (transport error or 5xx on a
// candidate → next living candidate; 4xx → error everywhere) remains the
// safety net. Failover happens at SUBMIT time: a job accepted by a runner
// stays that runner's job (job state lives in the accepting process);
// polling, result fetch, artifacts and acks are routed through a per-job
// map. Jobs lost with a dead candidate are re-claimed by the dispatcher's
// lease recovery and then submit to a living candidate — no mid-job
// migration needed (Nicht-Ziel: orchestration).
package processor

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// ErrNoCandidates is returned when the candidate chain is empty (e.g.
// NewFailoverChain with all-nil members): no runner can serve the call, so
// this explicit error replaces a silent nil/nil "success" that would
// nil-deref far from the cause.
var ErrNoCandidates = errors.New("no ingest candidates")

// FailoverClass reports whether an error should trigger the ingest fallback:
// transport-level failures and server-side 5xx. A 4xx is OUR request being
// wrong — the fallback would reject it identically.
func FailoverClass(err error) bool {
	if err == nil {
		return false
	}
	// Caller cancellation is not a runner problem — a fallback attempt
	// would be doomed by the same context.
	if errors.Is(err, ErrCancelled) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	// Everything else from do() is transport: connection refused, timeout,
	// canceled, EOF, decode of a broken response.
	// Known ceiling: a 2xx whose body fails SubmitProcess's validation is a
	// plain error too and lands here — an at-least-once double submit (job
	// dedup is per-runner). Classifying validation errors explicitly is the
	// upgrade path.
	return true
}

// FailoverClient routes ingest calls across an ordered candidate chain of
// runners (#207 generalizes the former primary/fallback pair). It satisfies
// the dispatcher's processorClient interface. Query-side calls
// (Embed/Rerank) intentionally have NO failover: the query runner is the
// always-local role, and its failure degrades retrieval per R3 instead of
// silently switching runners.
type FailoverClient struct {
	// clients is the ordered candidate chain (clients[0] = preferred).
	// ordered() re-ranks it by liveness: alive candidates first (configured
	// order among them), health-wise dead ones last — a dead Carrier is not
	// asked first, so the per-submit connect timeout disappears (#207).
	clients []*Client
	log     *log.Logger

	mu sync.Mutex
	// routes maps jobID -> accepting client. Only non-head accepts create
	// an entry (clients[0] is the routed() default).
	// Ceiling: jobs that end without a successful ack/cancel keep their
	// entry for the process lifetime — bounded by one entry per job. Cleared
	// on successful ack/cancel AND on chain-head re-accept of the jobID.
	routes map[string]*Client
	// down remembers the last known liveness per client (submit outcomes +
	// health probe) so transitions are logged once per outage, not per call.
	down map[*Client]bool
}

// NewFailover builds the legacy two-member chain. A nil fallback disables
// failover (pure primary). A nil logger silences the transition logs.
func NewFailover(primary, fallback *Client, lg *log.Logger) *FailoverClient {
	clients := []*Client{primary}
	if fallback != nil {
		clients = append(clients, fallback)
	}
	return NewFailoverChain(clients, lg)
}

// NewFailoverChain builds the ordered candidate chain (#207). Nil entries
// are dropped; the chain must not end up empty.
func NewFailoverChain(clients []*Client, lg *log.Logger) *FailoverClient {
	var cs []*Client
	for _, c := range clients {
		if c != nil {
			cs = append(cs, c)
		}
	}
	return &FailoverClient{clients: cs, log: lg, routes: map[string]*Client{}, down: map[*Client]bool{}}
}

// StartHealthMonitor probes every candidate periodically and keeps the
// liveness map fresh so dead runners leave the front of the submit path
// (#207). Best-effort by design: probe errors never block — submit-time
// failover remains the safety net if health is stale. interval <= 0
// disables the background probe.
func (f *FailoverClient) StartHealthMonitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			f.probeAll(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// probeBudget bounds one candidate's /v1/health call inside a probe cycle.
// It is deliberately short so a slow-but-alive runner does not stall the
// whole probe (which also stretches one cycle to ~n*budget for n candidates).
const probeBudget = 3 * time.Second

// probeAll pings each candidate once with a short budget.
func (f *FailoverClient) probeAll(ctx context.Context) {
	for _, c := range f.clients {
		// During shutdown the cancel fires while a cycle is in flight: health
		// fails fast for every candidate and would mark the whole chain down
		// (and log an "unavailable" line per runner) for the exit path. Skip
		// the rest of the cycle once the context is gone.
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, probeBudget)
		err := c.Health(pctx)
		cancel()
		f.setLiveness(c, err != nil, "health")
	}
}

// ordered returns the candidate chain re-ranked by liveness (head first).
// Caller must not mutate the result; called with f.mu NOT held.
func (f *FailoverClient) ordered() []*Client {
	f.mu.Lock()
	alive := make([]*Client, 0, len(f.clients))
	var dead []*Client
	for _, c := range f.clients {
		if f.down[c] {
			dead = append(dead, c)
		} else {
			alive = append(alive, c)
		}
	}
	f.mu.Unlock()
	return append(alive, dead...)
}

// routed returns the client that owns jobID (default: the preferred head,
// clients[0]). A head accept deliberately leaves no route entry (SubmitProcess
// deletes it), so the default MUST be the immutable preferred head — NOT
// ordered()[0] (first ALIVE): if the head accepted a job and is later marked
// down by a probe or an unrelated submit, follow-ups must still hit the head,
// which is processing the job. ordered()[0] would route them to a runner that
// never saw the job, producing 404s and a spurious resubmit.
func (f *FailoverClient) routed(jobID string) *Client {
	f.mu.Lock()
	c, ok := f.routes[jobID]
	f.mu.Unlock()
	if ok {
		return c
	}
	return f.clients[0]
}

// done forgets a finished job's routing (callers keep it on error — see
// Ack).
func (f *FailoverClient) done(jobID string) {
	f.mu.Lock()
	delete(f.routes, jobID)
	f.mu.Unlock()
}

// setLiveness records per-candidate up/down and logs the transition once
// per edge. source names the observer ("submit"/"health") for the log.
func (f *FailoverClient) setLiveness(c *Client, isDown bool, source string) {
	f.mu.Lock()
	prev := f.down[c]
	f.down[c] = isDown
	f.mu.Unlock()
	if prev == isDown {
		return
	}
	if f.log != nil {
		if isDown {
			f.log.Printf("ingest failover: candidate %s unavailable (%s) — new jobs go to the next living candidate", c.baseURL, source)
		} else {
			f.log.Printf("ingest failover: candidate %s back (%s) — eligible for new jobs again", c.baseURL, source)
		}
	}
}

// SubmitProcess tries the candidates in liveness order and fails over on
// FailoverClass errors. The accepting runner is recorded so follow-up calls
// route correctly.
func (f *FailoverClient) SubmitProcess(ctx context.Context, req *ProcessRequest) (*ProcessAccepted, error) {
	var lastErr error
	for _, c := range f.ordered() {
		acc, err := c.SubmitProcess(ctx, req)
		if err == nil {
			f.setLiveness(c, false, "submit")
			// The dispatcher may resubmit a job whose earlier attempt another
			// candidate accepted (e.g. lease lost during an outage, recovered
			// later). Re-accept by the chain head overwrites ownership.
			f.mu.Lock()
			if c == f.clients[0] {
				delete(f.routes, req.JobID)
			} else {
				f.routes[req.JobID] = c
			}
			f.mu.Unlock()
			return acc, nil
		}
		lastErr = err
		if !FailoverClass(err) {
			return nil, err // 4xx: the request is wrong on every candidate
		}
		f.setLiveness(c, true, "submit")
	}
	if lastErr == nil {
		// Empty candidate chain (NewFailoverChain with all-nil members): the
		// loop never ran. Return an explicit error instead of a nil/nil
		// "success" that would nil-deref far from the cause (matches the
		// Capabilities/Health guards).
		lastErr = ErrNoCandidates
	}
	return nil, lastErr
}

// Capabilities negotiates with the first living candidate — ingest must be
// able to start even when the preferred runner is down.
func (f *FailoverClient) Capabilities(ctx context.Context) (*Capabilities, error) {
	var lastErr error
	for _, c := range f.ordered() {
		caps, err := c.Capabilities(ctx)
		if err == nil {
			return caps, nil
		}
		lastErr = err
		if !FailoverClass(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoCandidates
	}
	return nil, lastErr
}

// Preflight (#175) runs /v1/pdf/preflight on the first living candidate. A
// preflight is a cheap diagnostic; a dead/hostile candidate failing over to
// the next is the same liveness discipline as Capabilities/Health.
func (f *FailoverClient) Preflight(ctx context.Context, pdf []byte) (*PreflightReport, error) {
	var lastErr error
	for _, c := range f.ordered() {
		r, err := c.Preflight(ctx, pdf)
		if err == nil {
			return r, nil
		}
		lastErr = err
		if !FailoverClass(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoCandidates
	}
	return nil, lastErr
}

// Health is green when ANY candidate can serve ingest.
func (f *FailoverClient) Health(ctx context.Context) error {
	var lastErr error
	for _, c := range f.ordered() {
		err := c.Health(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoCandidates
	}
	return lastErr
}

// JobStatus polls the runner that owns the job.
func (f *FailoverClient) JobStatus(ctx context.Context, jobID string) (*JobStatus, error) {
	return f.routed(jobID).JobStatus(ctx, jobID)
}

// JobResult fetches from the owning runner.
func (f *FailoverClient) JobResult(ctx context.Context, jobID string) ([]byte, error) {
	return f.routed(jobID).JobResult(ctx, jobID)
}

// Artifact fetches from the owning runner.
func (f *FailoverClient) Artifact(ctx context.Context, jobID, ref string) ([]byte, error) {
	return f.routed(jobID).Artifact(ctx, jobID, ref)
}

// Cancel targets the owning runner and forgets the routing.
func (f *FailoverClient) Cancel(ctx context.Context, jobID string) error {
	err := f.routed(jobID).Cancel(ctx, jobID)
	f.done(jobID)
	return err
}

// Ack targets the owning runner; the routing is forgotten only on success.
// The dispatcher retries failed acks (retryAcks/MarkAckFailed) — those
// retries must hit the OWNING runner again, not the primary default. Ack
// is idempotent on the runner side, so re-acking is safe.
func (f *FailoverClient) Ack(ctx context.Context, jobID string, ack Ack) error {
	err := f.routed(jobID).Ack(ctx, jobID, ack)
	if err == nil {
		f.done(jobID)
	}
	return err
}
