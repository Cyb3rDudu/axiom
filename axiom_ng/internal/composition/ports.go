// ports.go — the injection seams of the composition root (#298, F04).
//
// V1 binds everything LOCALLY (direct in-process construction, the pre-F04
// wiring). The seams exist so their replaceability is proven, not promised:
// the fake-binding test starts the SAME registry against faked ports, and
// F11 adds transport bindings (HTTP) without touching component code.
package composition

import (
	"context"
	"log"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// RunnerClient is the dispatcher→runner port: the processor surface the
// claim loop drives. *processor.Client and *processor.FailoverClient satisfy
// it structurally; a test fake implements it directly.
type RunnerClient interface {
	Capabilities(ctx context.Context) (*processor.Capabilities, error)
	SubmitProcess(ctx context.Context, req *processor.ProcessRequest) (*processor.ProcessAccepted, error)
	Preflight(ctx context.Context, doc []byte, contentType string) (*processor.PreflightReport, error)
	JobStatus(ctx context.Context, jobID string) (*processor.JobStatus, error)
	JobResult(ctx context.Context, jobID string) ([]byte, error)
	Artifact(ctx context.Context, jobID, ref string) ([]byte, error)
	Cancel(ctx context.Context, jobID string) error
	Ack(ctx context.Context, jobID string, ack processor.Ack) error
	// Health backs the /api/health runner check through the same binding.
	Health(ctx context.Context) error
}

// HealthRunner is the subset the /api/health runner checks probe.
type HealthRunner interface {
	Health(ctx context.Context) error
}

// Ports carries the composition's injection seams. The zero value means
// local bindings everywhere (fillLocal installs them); replace one field to
// rebind exactly that port.
type Ports struct {
	// IngestRunner builds the dispatcher's runner client. Local binding:
	// the #207 ordered failover chain over cfg.IngestCandidates() (with the
	// periodic health monitor); a binding without StartHealthMonitor simply
	// runs unmonitored — monitor-less fakes/drives are a legal port shape,
	// mirroring the dispatcher's optional LaneCapacity assert.
	IngestRunner func(cfg config.Config, logger *log.Logger) (RunnerClient, error)
	// QueryRunner builds the search-side processor client. Local binding:
	// processor.New over cfg.QueryRunnerURL.
	QueryRunner func(cfg config.Config) (*processor.Client, error)
	// RepairExecutor executes one axiom-repair-worker invocation (F08
	// #302). Local binding: nil (library/repair's process-group-hardened
	// LocalExecutor). See repair.RepairExecutor.
	RepairExecutor repair.RepairExecutor
}

// fillLocal installs the local bindings for every unset seam.
func (p *Ports) fillLocal() {
	if p.IngestRunner == nil {
		p.IngestRunner = localIngestRunner
	}
	if p.QueryRunner == nil {
		p.QueryRunner = func(cfg config.Config) (*processor.Client, error) {
			return processor.New(processor.Options{BaseURL: cfg.QueryRunnerURL})
		}
	}
}

// localIngestRunner is the local binding for the dispatcher→runner port:
// the #207 ordered candidate chain. An ORDERED candidate list from
// AXIOM_PROCESSOR_URLS (plural wins) or the legacy singular pair. A
// periodic health probe keeps dead candidates out of the submit path;
// submit-time failover stays as the safety net.
func localIngestRunner(cfg config.Config, logger *log.Logger) (RunnerClient, error) {
	var clients []*processor.Client
	for _, url := range cfg.IngestCandidates() {
		c, err := processor.New(processor.Options{
			BaseURL:       url,
			ResultTimeout: cfg.ProcessorRequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		clients = append(clients, c)
	}
	return processor.NewFailoverChain(clients, logger), nil
}

// runnerCheck adapts a runner health surface (primary or failover client)
// to the server's Checker interface for /api/health — moved verbatim from
// the pre-F04 main.go.
type runnerCheckFn struct {
	health func(ctx context.Context) error
}

func (r runnerCheckFn) Ready() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.health(ctx)
}

func runnerCheck(h HealthRunner) runnerCheckFn { return runnerCheckFn{health: h.Health} }

// probeQueryRunnerRole verifies at startup that the configured query runner
// actually serves the query roles (R4 Ziel 3): capabilities must advertise
// query_embedding and reranking. A capable-but-different runner is a valid
// query runner; a runner without them gets a WARNING — search stays up and
// degrades per R3. #216: the roles line also distinguishes warm from cold.
// Moved verbatim from the pre-F04 main.go.
func probeQueryRunnerRole(ctx context.Context, c *processor.Client, url string, logger *log.Logger) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	caps, err := c.Capabilities(probeCtx)
	if err != nil {
		logger.Printf("runner roles: query runner %s not reachable at start (search degrades per R3 until it is): %v", url, err)
		return
	}
	feats := caps.Features
	qe, rk := feats != nil && feats["query_embedding"], feats != nil && feats["reranking"]
	state := "warm"
	if !caps.ModelsWarmed {
		state = "cold (models warmup pending or disabled)"
	}
	switch {
	case qe && rk:
		logger.Printf("runner roles: query runner %s capable (%s, query_embedding=%v reranking=%v, model=%s, models_warmed=%v)", url, state, qe, rk, caps.Processor.Name, caps.ModelsWarmed)
	case !qe && !rk:
		logger.Printf("WARNING: runner roles: query runner %s has NEITHER query role (query_embedding/reranking) — retrieval will run degraded (BM25-only, unreranked); point AXIOM_QUERY_RUNNER_URL at a §7a-capable runner", url)
	default:
		logger.Printf("WARNING: runner roles: query runner %s only partially query-capable (query_embedding=%v reranking=%v) — partial R3 degradation expected", url, qe, rk)
	}
}
