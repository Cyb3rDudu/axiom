package processor

// #207: candidate-chain behavior — health-based ordering, three candidates,
// dead head skipped, recovery restores preference. The two-member regression
// cases stay in failover_test.go.

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
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

	// Health says the carrier is down: after the probe the dead head is NOT
	// asked first anymore (no per-submit connect timeout against a corpse) —
	// the living candidate serves. The head avoids the connect timeout and
	// stays un-asked for the new job.
	carrier.healthFail.Store(true)
	fc.probeAll(context.Background())
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "b"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 || local.submitHits.Load() != 1 {
		t.Fatalf("after probe the dead head must not serve new jobs: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
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

// TestChain_HeadAcceptedFollowUpStaysOnHead pins the #207 routed() default
// invariant (auto-review C1): a job accepted by the HEAD leaves no route entry
// (SubmitProcess deletes it; clients[0] is the routed default). If the head is
// then marked DOWN by a health probe, follow-ups (job status/result/ack) for
// that head-owned job must STILL route to the head — which is processing it —
// NOT to the first ALIVE candidate (ordered()[0]), where the job is unknown and
// would 404 into a spurious dispatcher resubmit.
func TestChain_HeadAcceptedFollowUpStaysOnHead(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	// Head accepts job "a": no route entry is created (head-accept overwrites
	// ownership, so follow-ups rely on the head default).
	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 || local.submitHits.Load() != 0 {
		t.Fatalf("healthy head must accept: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
	}
	if _, owned := fc.routes["a"]; owned {
		t.Fatalf("head-accept must NOT create a route entry for the head-owned job")
	}

	// Health probe demotes the head (carrier still processing job "a").
	carrier.healthFail.Store(true)
	fc.probeAll(context.Background())

	// Follow-up (ack) for the head-owned job must route to the HEAD, not the
	// now-first-alive local candidate.
	if err := fc.Ack(context.Background(), "a", Ack{Persisted: true}); err != nil {
		t.Fatal(err)
	}
	if carrier.ackHits.Load() != 1 {
		t.Fatalf("head-owned job's follow-up must route to the head even after probe-demotion: carrier acks=%d (got routed to the alive fallback)", carrier.ackHits.Load())
	}
	if local.ackHits.Load() != 0 {
		t.Fatalf("fallback must NOT receive the head-owned job's follow-up: local acks=%d", local.ackHits.Load())
	}
}

// TestChain_EmptyChainFailsExplicitly pins a #207-review hardening (W4):
// NewFailoverChain with an all-nil member set builds an empty chain. Instead
// of returning (nil, nil) — a silent "success" that nil-derefs downstream —
// SubmitProcess, Capabilities, and Health must each surface an explicit
// "no ingest candidates" error. The routed()-based getters (JobStatus,
// JobResult, Artifact, Cancel, Ack) are NOT covered here: they index
// clients[0] and would panic on an empty chain — unreachable in production
// because IngestCandidates never yields an empty chain.
func TestChain_EmptyChainFailsExplicitly(t *testing.T) {
	fc := NewFailoverChain([]*Client{nil}, log.New(io.Discard, "", 0))

	_, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"})
	if err == nil {
		t.Fatal("SubmitProcess on an empty chain must return an error, not (nil, nil)")
	}
	if !strings.Contains(err.Error(), "no ingest candidates") {
		t.Fatalf("empty-chain SubmitProcess error = %v, want a description of the missing candidates", err)
	}

	if _, err := fc.Capabilities(context.Background()); err == nil {
		t.Fatal("Capabilities on an empty chain must fail")
	}
	if err := fc.Health(context.Background()); err == nil {
		t.Fatal("Health on an empty chain must fail")
	}
}

// TestChain_HealthMonitorEndToEnd exercises the PRODUCTION entry (review W3):
// StartHealthMonitor starts goroutines only when interval>0 and fades a
// probed-down head out of the submit path, then fades a recovered head back
// in, all driven by the background ticker (not a hand-called probeAll).
func TestChain_HealthMonitorEndToEnd(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.StartHealthMonitor(ctx, 20*time.Millisecond)

	// The monitor runs on its own ticker; poll by submitting a fresh job until
	// the observed routing reflects the monitor's liveness flip, or fail loudly
	// after the deadline. Each submit observes current liveness (SubmitProcess
	// iterates ordered()) and re-triggers the routing check.
	pollSubmit := func(what string, want func(carrier, local, prevCarrier int64) bool) {
		t.Helper()
		n := 0
		var prevC int64 = -1
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			n++
			if _, err := fc.SubmitProcess(ctx, &ProcessRequest{JobID: fmt.Sprintf("j%d", n)}); err != nil {
				t.Fatalf("submit: %v", err)
			}
			c := carrier.submitHits.Load()
			l := local.submitHits.Load()
			if want(c, l, prevC) {
				return
			}
			prevC = c
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s (carrier=%d local=%d)", what, carrier.submitHits.Load(), local.submitHits.Load())
	}

	// Downed head: once the monitor demotes it, submits stop reaching the
	// carrier (its count stays put across consecutive submits) while the local
	// runner keeps serving.
	carrier.healthFail.Store(true)
	pollSubmit("demoted head stops serving, local serves", func(c, l, prevC int64) bool {
		return l >= 1 && prevC >= 0 && c == prevC
	})

	// Recovered head: the monitor folds it back, so new jobs head to the
	// carrier again (its count grows past a settled plateau).
	carrier.healthFail.Store(false)
	pollSubmit("recovered head serves again", func(c, l, prevC int64) bool {
		return prevC >= 0 && c > prevC
	})
}

// TestChain_Capabilities4xxDoesNotFailOver covers the new Capabilities
// 4xx short-circuit (review): a runner answering 4xx aborts negotiation with
// an error instead of silently selecting a later runner — mirroring the
// SubmitProcess 4xx rule (the request is wrong, failover won't fix it).
func TestChain_Capabilities4xxDoesNotFailOver(t *testing.T) {
	main := newScriptRunner(t, "main")
	local := newScriptRunner(t, "local")
	fc := newChain(t, main, local)

	// First prove the healthy path serves the head's identity.
	caps, err := fc.Capabilities(context.Background())
	if err != nil || caps.Processor.Name != "main" {
		t.Fatalf("healthy capabilities must serve the head, got %+v err=%v", caps, err)
	}

	// Head answers 4xx: Capabilities must FAIL (not skip to the fallback),
	// and the fallback must never be asked (proven directly via capsHits).
	main.capsFail.Store(true)
	_, err = fc.Capabilities(context.Background())
	if err == nil {
		t.Fatal("Capabilities must propagate the head's 4xx instead of silently failing over")
	}
	if local.capsHits.Load() != 0 {
		t.Fatalf("4xx must not reach the fallback candidate: local caps attempts=%d", local.capsHits.Load())
	}
}

// TestChain_HealthMonitorDisabledInterval pins the disable branch of
// StartHealthMonitor (interval <= 0 returns without starting a goroutine —
// time.NewTicker would panic on a non-positive interval): a probed-down head
// stays in the submit path because nothing demotes it. With the monitor
// disabled, liveness only changes via submit-time failover, as documented
// for AXIOM_RUNNER_HEALTH_INTERVAL<=0.
func TestChain_HealthMonitorDisabledInterval(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.StartHealthMonitor(ctx, 0) // disabled: no background probe

	carrier.healthFail.Store(true)
	time.Sleep(50 * time.Millisecond) // room for a wrongly started goroutine to demote

	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 || local.submitHits.Load() != 0 {
		t.Fatalf("a disabled monitor must not demote the head: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
	}
}

// TestChain_ProbeAllSkipsOnCancelledContext pins the shutdown guard in
// probeAll (ctx.Err() != nil -> return): a cancelled context makes health
// calls fail fast, and probing anyway would mark the whole chain down —
// logging an "unavailable" line per runner on the exit path. After a
// cancelled cycle, liveness is unchanged and submits keep their preference.
func TestChain_ProbeAllSkipsOnCancelledContext(t *testing.T) {
	carrier := newScriptRunner(t, "carrier")
	local := newScriptRunner(t, "local")
	fc := newChain(t, carrier, local)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()                       // shutdown already in flight
	carrier.healthFail.Store(true) // health WOULD fail if consulted
	fc.probeAll(ctx)

	if _, err := fc.SubmitProcess(context.Background(), &ProcessRequest{JobID: "a"}); err != nil {
		t.Fatal(err)
	}
	if carrier.submitHits.Load() != 1 || local.submitHits.Load() != 0 {
		t.Fatalf("a cancelled probe cycle must not demote the chain: carrier=%d local=%d", carrier.submitHits.Load(), local.submitHits.Load())
	}
}
