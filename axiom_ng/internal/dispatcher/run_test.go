package dispatcher

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

// TestAutoQueueRepairClassesLegacyAndNewSpelling (#283 review fix): both
// findings of the scan class auto-queue — the renamed operator-facing
// string AND the legacy one (stored cases, mixed-binary runners). Removing
// either key breaks repair admission for a whole deployment generation.
func TestAutoQueueRepairClassesLegacyAndNewSpelling(t *testing.T) {
	for _, class := range []string{
		"🔴 scan-ohne-textlayer (OCR-Wiederaufbau nötig)",
		"🔴 unpaginiert",
		"🔴 reparierbar",
		"SOURCE_UNREADABLE",
	} {
		if !autoQueueRepairClasses[class] {
			t.Fatalf("class %q must auto-queue", class)
		}
	}
}

// refusingClient fails capability negotiation with a non-failover
// StatusError (4xx: a reachable candidate rejecting the request) — Run
// must return that error on the FIRST attempt, deterministically, without
// touching rep-beyond-skewWatch or persist (negotiation is client-only).
type refusingClient struct{}

func (refusingClient) Capabilities(ctx context.Context) (*processor.Capabilities, error) {
	return nil, &processor.StatusError{Code: 400, Body: "refusing probe: contract rejected"}
}
func (refusingClient) SubmitProcess(ctx context.Context, req *processor.ProcessRequest) (*processor.ProcessAccepted, error) {
	return nil, errors.New("refusing probe")
}
func (refusingClient) Preflight(ctx context.Context, doc []byte, contentType string) (*processor.PreflightReport, error) {
	return nil, errors.New("refusing probe")
}
func (refusingClient) JobStatus(ctx context.Context, jobID string) (*processor.JobStatus, error) {
	return nil, errors.New("refusing probe")
}
func (refusingClient) JobResult(ctx context.Context, jobID string) ([]byte, error) {
	return nil, errors.New("refusing probe")
}
func (refusingClient) Artifact(ctx context.Context, jobID, ref string) ([]byte, error) {
	return nil, errors.New("refusing probe")
}
func (refusingClient) Cancel(ctx context.Context, jobID string) error {
	return errors.New("refusing probe")
}
func (refusingClient) Ack(ctx context.Context, jobID string, ack processor.Ack) error {
	return errors.New("refusing probe")
}

// TestWaitReadyReportsRunErrorAfterStopped (#298 auto-review C1): the
// runErr write must happen-BEFORE close(stopped) — a concurrent WaitReady
// reading runErr after <-stopped fires is a data race when Run's defers
// close first under LIFO. Run under -race to prove the ordering holds;
// the assertion proves the error is not lost (diagnosis, not just
// "stopped before ready"). DB-backed like every Run-caller in this
// package: Run's skewWatch probes the repo clock immediately, so a
// nil-repo dispatcher cannot reach the negotiation failure path.
func TestWaitReadyReportsRunErrorAfterStopped(t *testing.T) {
	h := openDispatchDB(t)
	d := NewWithPersister(h.rep, refusingClient{}, nil, Config{
		Concurrency:          1,
		MaxStartupWait:       10 * time.Millisecond,
		StartupRetryInterval: 5 * time.Millisecond,
	}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyErr := make(chan error, 1)
	go func() { readyErr <- d.WaitReady(ctx) }()

	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = d.Run(ctx) }()

	select {
	case err := <-readyErr:
		if err == nil {
			t.Fatal("WaitReady must fail when Run stops pre-ready")
		}
		if !strings.Contains(err.Error(), "stopped before ready") || !strings.Contains(err.Error(), "refusing probe") {
			t.Fatalf("WaitReady must surface the run error ('stopped before ready' + cause), got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitReady did not return after Run stopped")
	}

	// Run itself must have returned (drained its background passes) —
	// Stopped closes only after bg.Wait.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the negotiation failure")
	}
	select {
	case <-d.Stopped():
	default:
		t.Fatal("Stopped must be closed after Run returned")
	}
}
