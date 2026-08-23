package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptRunner is a minimal fake runner for failover tests: health,
// capabilities, process-submit (echo), status, ack.
type scriptRunner struct {
	*httptest.Server
	submitHits   atomic.Int64
	ackHits      atomic.Int64
	artifactHits atomic.Int64
	submitCode   int         // 0 = accept; otherwise reply with this status
	ackFail      atomic.Bool // ack endpoint replies 500 while set (read-only per test phase)
	healthFail   atomic.Bool // health endpoint replies 503 while set (#207 probes)
	name         string
}

func newScriptRunner(t *testing.T, name string) *scriptRunner {
	t.Helper()
	sr := &scriptRunner{name: name}
	sr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/health":
			if sr.healthFail.Load() {
				writeJSONT(w, 503, map[string]any{"status": "unavailable"})
				return
			}
			writeJSONT(w, 200, map[string]any{"status": "ok"})
		case r.URL.Path == "/v1/capabilities":
			writeJSONT(w, 200, map[string]any{
				"contract_versions": []string{"1.0"},
				"processor":         map[string]any{"name": name, "version": "0.1"},
				"formats":           []string{"application/pdf"},
				"features":          map[string]bool{"markdown": true, "query_embedding": true, "reranking": true},
				"models":            map[string]any{},
				"limits":            map[string]any{},
			})
		case r.URL.Path == "/v1/process":
			sr.submitHits.Add(1)
			if sr.submitCode != 0 {
				w.WriteHeader(sr.submitCode)
				_, _ = w.Write([]byte(`{"detail":"boom"}`))
				return
			}
			var body struct {
				JobID string `json:"job_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSONT(w, 202, map[string]any{
				"contract_version": "1.0", "job_id": body.JobID, "status": "accepted",
			})
		case strings.Contains(r.URL.Path, "/artifacts/"):
			sr.artifactHits.Add(1)
			writeJSONT(w, 200, map[string]any{
				"contract_version": "1.0", "job_id": "j", "status": "completed",
			})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			sr.ackHits.Add(1)
			if sr.ackFail.Load() {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"detail":"ack boom"}`))
				return
			}
			writeJSONT(w, 200, map[string]any{"contract_version": "1.0", "job_id": "j", "status": "acked", "ok": true})
		default:
			// status/result/artifact: minimal job payload
			writeJSONT(w, 200, map[string]any{
				"contract_version": "1.0", "job_id": "j", "status": "completed",
			})
		}
	}))
	t.Cleanup(sr.Close)
	return sr
}

func writeJSONT(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newClientT(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFailover_SubmitFallsBackWhenPrimaryDown(t *testing.T) {
	dead := newScriptRunner(t, "dead")
	dead.Close() // primary endpoint gone
	fallback := newScriptRunner(t, "local")
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	fc := NewFailover(newClientT(t, dead.URL), newClientT(t, fallback.URL), lg)

	acc, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"})
	if err != nil {
		t.Fatal(err)
	}
	if acc.JobID != "j" || fallback.submitHits.Load() != 1 {
		t.Fatalf("submit must land on the fallback: acc=%+v hits=%d", acc, fallback.submitHits.Load())
	}
	// Transition logged exactly once per outage edge.
	if got := strings.Count(buf.String(), "ingest failover: candidate"); got != 1 {
		t.Fatalf("want 1 failover log line, got %d: %q", got, buf.String())
	}
	if !strings.Contains(buf.String(), "unavailable") {
		t.Fatalf("log must document the failover: %q", buf.String())
	}

	// Follow-up calls route to the runner that owns the job.
	if _, err := fc.JobStatus(context.Background(), "j"); err != nil {
		t.Fatal(err)
	}
	if err := fc.Ack(context.Background(), "j", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if fallback.ackHits.Load() != 1 {
		t.Fatalf("ack must route to the owning fallback runner, hits=%d", fallback.ackHits.Load())
	}
	// Routing is forgotten after a SUCCESSFUL ack; entries for jobs that
	// end without ack (onFailed, invalid results) persist — see routes doc.
	fc.mu.Lock()
	routes := len(fc.routes)
	fc.mu.Unlock()
	if routes != 0 {
		t.Fatalf("ack must clear the job route, remaining=%d", routes)
	}
}

func TestFailover_RecoveryLogsReturnToPrimary(t *testing.T) {
	primary := newScriptRunner(t, "carrier")
	fallback := newScriptRunner(t, "local")
	var buf bytes.Buffer
	fc := NewFailover(newClientT(t, primary.URL), newClientT(t, fallback.URL), log.New(&buf, "", 0))

	// Force the primary down for one submit, then bring it back.
	primary.submitCode = 503
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	primary.submitCode = 0
	// #207: the 503 marked the head candidate down; a health probe
	// notices the recovery (log edge "back") before the next submit.
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "b"}); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "unavailable") || !strings.Contains(logs, "back") {
		t.Fatalf("both outage edges must be logged once: %q", logs)
	}
	if strings.Count(logs, "ingest failover") != 2 {
		t.Fatalf("exactly one line per edge, got: %q", logs)
	}
	// Primary saw both submits (a rejected with 503, b accepted after
	// recovery); the fallback saw exactly one (a).
	if primary.submitHits.Load() != 2 || fallback.submitHits.Load() != 1 {
		t.Fatalf("recovery routing wrong: primary hits=%d (want 2), fallback hits=%d (want 1)",
			primary.submitHits.Load(), fallback.submitHits.Load())
	}
}

func TestFailover_NoFailoverOn4xx(t *testing.T) {
	primary := newScriptRunner(t, "p")
	fallback := newScriptRunner(t, "f")
	primary.submitCode = 422 // our request is wrong; fallback would reject too
	fc := NewFailover(newClientT(t, primary.URL), newClientT(t, fallback.URL), log.New(io.Discard, "", 0))
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err == nil {
		t.Fatal("422 must propagate, not fail over")
	}
	if fallback.submitHits.Load() != 0 {
		t.Fatalf("4xx must not reach the fallback, hits=%d", fallback.submitHits.Load())
	}
}

func TestFailover_BothDownPropagatesFallbackError(t *testing.T) {
	dead := newScriptRunner(t, "d1")
	dead.Close()
	dead2 := newScriptRunner(t, "d2")
	dead2.Close()
	fc := NewFailover(newClientT(t, dead.URL), newClientT(t, dead2.URL), log.New(io.Discard, "", 0))
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err == nil {
		t.Fatal("both runners down must surface an error")
	}
}

func TestFailover_NilFallbackIsPlainPrimary(t *testing.T) {
	primary := newScriptRunner(t, "p")
	fc := NewFailover(newClientT(t, primary.URL), nil, log.New(io.Discard, "", 0))
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	if primary.submitHits.Load() != 1 {
		t.Fatal("nil fallback must keep plain primary behavior")
	}
	dead := newScriptRunner(t, "d")
	dead.Close()
	fcDead := NewFailover(newClientT(t, dead.URL), nil, log.New(io.Discard, "", 0))
	if _, err := fcDead.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err == nil {
		t.Fatal("dead primary without fallback must error")
	}
}

func TestFailover_CapabilitiesAndHealthFallback(t *testing.T) {
	dead := newScriptRunner(t, "dead")
	dead.Close()
	fallback := newScriptRunner(t, "local")
	fc := NewFailover(newClientT(t, dead.URL), newClientT(t, fallback.URL), log.New(io.Discard, "", 0))

	if _, err := fc.Capabilities(context.Background()); err != nil {
		t.Fatalf("capabilities must fall back when primary is down: %v", err)
	}
	if err := fc.Health(context.Background()); err != nil {
		t.Fatalf("health is green when the fallback lives: %v", err)
	}
	// Capabilities flavor check: the fallback's identity is served.
	caps, err := fc.Capabilities(context.Background())
	if err != nil || caps.Processor.Name != "local" {
		t.Fatalf("fallback capabilities must be served, got %+v err=%v", caps, err)
	}
}

func TestFailoverClass(t *testing.T) {
	if !FailoverClass(errors.New("connection refused")) {
		t.Fatal("transport errors are failover-class")
	}
	if !FailoverClass(&StatusError{Code: 500}) || !FailoverClass(&StatusError{Code: 503}) {
		t.Fatal("5xx are failover-class")
	}
	if FailoverClass(&StatusError{Code: 422}) || FailoverClass(&StatusError{Code: 404}) {
		t.Fatal("4xx are not failover-class")
	}
	if FailoverClass(nil) {
		t.Fatal("nil is not failover-class")
	}
}

func TestFailover_RouteIsolation(t *testing.T) {
	// Two jobs in one outage window: the fallback-owned job keeps routing
	// there even after the primary recovers (job state lives on its runner).
	primary := newScriptRunner(t, "carrier")
	fallback := newScriptRunner(t, "local")
	fc := NewFailover(newClientT(t, primary.URL), newClientT(t, fallback.URL), log.New(io.Discard, "", 0))

	primary.submitCode = 503
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "during-outage"}); err != nil {
		t.Fatal(err)
	}
	primary.submitCode = 0
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "after-recovery"}); err != nil {
		t.Fatal(err)
	}
	// The fallback-owned job's artifact and ack calls must both land on the
	// fallback (result/artifact/status of a foreign job are runner-private).
	if _, err := fc.Artifact(context.Background(), "during-outage", "pages/1.md"); err != nil {
		t.Fatal(err)
	}
	if fallback.artifactHits.Load() != 1 || primary.artifactHits.Load() != 0 {
		t.Fatalf("artifact must route to the owning fallback: fb=%d p=%d",
			fallback.artifactHits.Load(), primary.artifactHits.Load())
	}
	if err := fc.Ack(context.Background(), "during-outage", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if fallback.ackHits.Load() != 1 || primary.ackHits.Load() != 0 {
		t.Fatalf("outage job must ack on the fallback: fb=%d p=%d", fallback.ackHits.Load(), primary.ackHits.Load())
	}
}

func TestFailover_PrimaryReacceptClearsStaleRoute(t *testing.T) {
	// Review counterexample (proven by execution): outage attempt accepted
	// by the fallback -> primary recovers -> the dispatcher resubmits the
	// SAME jobID (lease recovery) -> primary accepts -> every follow-up
	// must hit the primary, not the stale fallback route from attempt 1.
	primary := newScriptRunner(t, "carrier")
	fallback := newScriptRunner(t, "local")
	fc := NewFailover(newClientT(t, primary.URL), newClientT(t, fallback.URL), log.New(io.Discard, "", 0))

	primary.submitCode = 503
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	primary.submitCode = 0
	// #207: the 503 marked the head candidate down; a health probe notices
	// the recovery and restores chain-head priority BEFORE the resubmit.
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	if err := fc.Ack(context.Background(), "j", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if primary.ackHits.Load() != 1 || fallback.ackHits.Load() != 0 {
		t.Fatalf("primary re-accept must own the job: primary acks=%d fallback acks=%d",
			primary.ackHits.Load(), fallback.ackHits.Load())
	}
}

func TestFailover_AckFailureKeepsRoute(t *testing.T) {
	primary := newScriptRunner(t, "carrier")
	fallback := newScriptRunner(t, "local")
	primary.submitCode = 503 // force fallback acceptance
	fc := NewFailover(newClientT(t, primary.URL), newClientT(t, fallback.URL), log.New(io.Discard, "", 0))
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "j"}); err != nil {
		t.Fatal(err)
	}
	primary.submitCode = 0

	// Ack fails on the owning fallback: the route must SURVIVE so the
	// dispatcher's ack retry (retryAcks via MarkAckFailed) keeps targeting
	// the owning runner instead of the (now healthy) primary default.
	fallback.ackFail.Store(true)
	if err := fc.Ack(context.Background(), "j", Ack{Persisted: true}); err == nil {
		t.Fatal("ack must fail while the fallback ack endpoint errors")
	}
	fallback.ackFail.Store(false)
	if err := fc.Ack(context.Background(), "j", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if fallback.ackHits.Load() != 2 || primary.ackHits.Load() != 0 {
		t.Fatalf("ack retry must stay on the owning fallback: fb=%d p=%d",
			fallback.ackHits.Load(), primary.ackHits.Load())
	}
}

// Compile-time interface guarantee — must mirror dispatcher.processorClient
// (the dispatcher owns the source of truth; kept in sync by hand).
var _ interface {
	Capabilities(ctx context.Context) (*Capabilities, error)
	SubmitProcess(ctx context.Context, req *ProcessRequest) (*ProcessAccepted, error)
	JobStatus(ctx context.Context, jobID string) (*JobStatus, error)
	JobResult(ctx context.Context, jobID string) ([]byte, error)
	Artifact(ctx context.Context, jobID, ref string) ([]byte, error)
	Cancel(ctx context.Context, jobID string) error
	Ack(ctx context.Context, jobID string, ack Ack) error
} = (*FailoverClient)(nil)
