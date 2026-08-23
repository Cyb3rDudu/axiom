package dispatcher

// R4 (#134) ingest failover integration: primary runner unreachable → the
// dispatcher's jobs complete via the local fallback runner and the failover
// transition is documented in the log. Runs against the real dispatcher loop
// and a real test DB (AXIOM_TEST_DATABASE_URL gate, same as the suite).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// deadURL returns a loopback URL that refuses connections (bound then
// released listener). Connection-refused is the transport-class outage that
// must trigger failover.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func TestIngestFailoverJobCompletesViaLocalFallback(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "P1", 3)

	// Primary: dead. Fallback: the healthy fake runner.
	fallback := newFakeProcessor(t)
	fallback.statuses = []string{"running", "completed"}
	fallback.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	var logBuf bytes.Buffer
	fc := processor.NewFailover(
		mustClient(t, deadURL(t)),
		mustClient(t, fallback.url()),
		log.New(&logBuf, "", 0),
	)

	d := NewWithPersister(h.rep, fc, &recordingPersister{rep: h.rep}, Config{
		Concurrency:      1,
		LeaseDuration:    5 * time.Minute,
		RenewalInterval:  25 * time.Millisecond,
		PollInterval:     15 * time.Millisecond,
		AckRetryInterval: 100 * time.Millisecond,
		Profile:          json.RawMessage(`{"profile":"full-rag-v1"}`),
	}, log.New(io.Discard, "", 0))
	runFor(t, d, context.Background(), 6*time.Second)

	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("job must complete via the fallback runner, status = %q", got)
	}
	if fallback.processHits != 1 {
		t.Fatalf("submit must land on the fallback, hits = %d", fallback.processHits)
	}
	if fallback.ackHits != 1 {
		t.Fatalf("ack must route to the owning fallback, hits = %d", fallback.ackHits)
	}
	fails := logBuf.String()
	// The failover document line uses the candidate-based wording of the
	// ordered list feature (#207): a dead primary is "candidate <url>
	// unavailable" (the old "primary runner" phrasing predates the chain).
	if !strings.Contains(fails, "ingest failover: candidate ") || !strings.Contains(fails, "unavailable") {
		t.Fatalf("failover must be documented in the log, got: %q", fails)
	}
	t.Logf("[IT] failover log: %s", fails)
}
