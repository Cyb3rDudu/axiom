package processor

// #207: candidate-chain behavior — health-based ordering, three candidates,
// dead head skipped, recovery restores preference. The two-member regression
// cases stay in failover_test.go.

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

func newChain(t *testing.T, members ...*scriptRunner) *FailoverClient {
	t.Helper()
	var clients []*Client
	for _, m := range members {
		clients = append(clients, newClientT(t, m.URL))
	}
	return NewFailoverChain(clients, log.New(io.Discard, "", 0))
}

func TestChain_DeadHeadIsNotAskedFirstAfterProbe(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	// Both alive: preferred carrier serves.
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 || local.submitHits.Load() != 0 {
		t.Fatalf("healthy head must serve: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
	}

	// Carrier dies. First submit after the death still tries it (stale
	// liveness — submit-time failover is the safety net)...
	carrier.Close()
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "b"}); err != nil {
		t.Fatal(err)
	}
	// ...but once the health probe has seen the death, the dead head is NOT
	// asked first anymore — no per-submit connect timeout against a corpse.
	fc.probeAll(context.Background())
	start := time.Now()
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "c"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("submit after probe must skip the dead head fast, took %v", elapsed)
	}
	if local.submitHits.Load() != 2 {
		t.Fatalf("living candidate must serve both post-death jobs: local=%d", local.submitHits.Load())
	}
}

func TestChain_HealthMonitorFlipsCandidateBack(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	// Health says the carrier is down -> the probe demotes it; local serves.
	carrier.healthFail.Store(true)
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if local.submitHits.Load() != 1 || carrier.submitHits.Load() != 0 {
		t.Fatalf("probed-down head must not be asked: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
	}

	// Health recovers -> the probe restores chain preference; the carrier
	// serves the next job again ("rollt wieder auf").
	carrier.healthFail.Store(false)
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "b"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 {
		t.Fatalf("recovered head must serve again: carrier=%d", carrier.submitHits.Load())
	}
}

func TestChain_MiddleCandidateDownSkipsToTail(t *testing.T) {
	gpu0 := newScriptRunner(t, "gpu0")
	gpu1 := newScriptRunner(t, "gpu1")
	local := newScriptRunner(t, "local")
	fc := newChain(t, gpu0, gpu1, local)

	gpu0.Close()
	gpu1.Close()
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if local.submitHits.Load() != 1 {
		t.Fatalf("last living candidate must serve: local=%d", local.submitHits.Load())
	}
	// Routed ownership: the job lives on the local runner.
	if err := fc.Ack(context.Background(), "a", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if local.ackHits.Load() != 1 {
		t.Fatalf("ack must route to the accepting candidate: local acks=%d", local.ackHits.Load())
	}
}
