// Package processor — ingest failover chain (epic #130 R4, #134).
//
// Role model: AXIOM_PROCESSOR_URL is the BEST-AVAILABLE ingest runner
// (Carrier GPU when present); when it is unreachable (transport error or
// 5xx), ingest falls back to AXIOM_INGEST_FALLBACK_URL (default: the local
// always-on runner — #128 proved local chunking is complete, just slower).
// Failover happens at SUBMIT time: a job accepted by a runner stays that
// runner's job (job state lives in the accepting process); polling, result
// fetch, artifacts and acks are routed through a per-job map. Jobs lost with
// a dead primary are re-claimed by the dispatcher's lease recovery and then
// submit to the fallback — no mid-job migration needed (Nicht-Ziel:
// orchestration).
package processor

import (
	"context"
	"errors"
	"log"
	"sync"
)

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

// FailoverClient routes ingest calls between a primary and a fallback runner.
// It satisfies the dispatcher's processorClient interface. Query-side calls
// (Embed/Rerank) intentionally have NO failover: the query runner is the
// always-local role, and its failure degrades retrieval per R3 instead of
// silently switching runners.
type FailoverClient struct {
	primary  *Client
	fallback *Client
	log      *log.Logger

	mu sync.Mutex
	// routes maps jobID -> accepting client for fallback-owned jobs. Only
	// fallback accepts create an entry (primary is the routed() default).
	// Ceiling: jobs that end without a successful ack/cancel (onFailed,
	// RESULT_INVALID/RESULT_FETCH_FAILED) keep their entry for the process
	// lifetime — bounded by one entry per fallback-accepted job. Cleared on
	// successful ack/cancel AND on primary re-accept of the same jobID.
	routes map[string]*Client
	// primaryDown remembers the last failover state so the transition is
	// logged once per outage, not once per request.
	primaryDown bool
}

// NewFailover builds the chain. A nil fallback disables failover (pure
// primary). A nil logger silences the transition logs.
func NewFailover(primary, fallback *Client, lg *log.Logger) *FailoverClient {
	return &FailoverClient{primary: primary, fallback: fallback, log: lg, routes: map[string]*Client{}}
}

// routed returns the client that owns jobID (default: primary).
func (f *FailoverClient) routed(jobID string) *Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.routes[jobID]; ok {
		return c
	}
	return f.primary
}

// done forgets a finished job's routing (callers keep it on error — see
// Ack).
func (f *FailoverClient) done(jobID string) {
	f.mu.Lock()
	delete(f.routes, jobID)
	f.mu.Unlock()
}

// markState logs primary up/down transitions once per edge.
func (f *FailoverClient) markState(down bool) {
	f.mu.Lock()
	changed := down != f.primaryDown
	f.primaryDown = down
	f.mu.Unlock()
	if changed && f.log != nil {
		if down {
			f.log.Printf("ingest failover: primary runner %s unavailable — new jobs go to fallback %s",
				f.primary.baseURL, f.fallback.baseURL)
		} else if f.fallback != nil {
			f.log.Printf("ingest failover: primary runner %s back — new jobs return to primary", f.primary.baseURL)
		}
	}
}

// SubmitProcess tries the primary and fails over on FailoverClass errors.
// The accepting runner is recorded so follow-up calls route correctly.
func (f *FailoverClient) SubmitProcess(ctx context.Context, req *ProcessRequest) (*ProcessAccepted, error) {
	acc, err := f.primary.SubmitProcess(ctx, req)
	if err == nil {
		f.markState(false)
		// The dispatcher may resubmit a job whose earlier attempt the
		// fallback accepted (e.g. lease lost during the outage, recovered
		// after primary returned). Primary re-accept overwrites ownership.
		f.done(req.JobID)
		return acc, nil
	}
	if f.fallback == nil || !FailoverClass(err) {
		return nil, err
	}
	f.markState(true)
	acc, ferr := f.fallback.SubmitProcess(ctx, req)
	if ferr != nil {
		return nil, ferr
	}
	f.mu.Lock()
	f.routes[req.JobID] = f.fallback
	f.mu.Unlock()
	return acc, nil
}

// Capabilities negotiates with the primary, falling back when it is down —
// ingest must be able to start even when only the fallback runner lives.
func (f *FailoverClient) Capabilities(ctx context.Context) (*Capabilities, error) {
	caps, err := f.primary.Capabilities(ctx)
	if err == nil {
		return caps, nil
	}
	if f.fallback == nil || !FailoverClass(err) {
		return nil, err
	}
	return f.fallback.Capabilities(ctx)
}

// Health is green when EITHER runner can serve ingest.
func (f *FailoverClient) Health(ctx context.Context) error {
	if err := f.primary.Health(ctx); err == nil {
		return nil
	}
	if f.fallback == nil {
		return f.primary.Health(ctx) // surface the primary error
	}
	return f.fallback.Health(ctx)
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
