package dispatcher

// #249 (solo-mode S6): the runner live view's reality. Root cause (production
// 2026-09-04): the #169 deriver subscribes to its OWN process's event bus,
// but the bus is process-local — with the carrier-era three-agent topology,
// /api/runners/live ran in a process whose bus no dispatcher fed, so 3
// claimed/computing jobs produced an EMPTY 200. These tests pin both sides:
// the end-to-end truth under the supported single-agent topology (#248), and
// the blindness itself as the documented mechanism.

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
)

// TestRunnerLiveEndToEndSingleAgent (#249 acceptance): under the supported
// topology — ONE dispatcher agent whose process ALSO serves the API (#248
// runbook) — a REAL claim loop drives the bus, the deriver folds it, and
// /api/runners/live shows the busy runner with book + stage while the job
// computes. This is the production wiring (main.go) compressed into one IT.

// TestRunnerLiveBlindToForeignProcessBus (#249 root cause, pinned): claims
// published on OTHER processes' buses are invisible to a process whose own
// dispatcher never runs — the mechanism behind the empty production view.
// This documents WHY the topology fix (#248: one agent, API in the same
// process) is load-bearing: no amount of deriver logic can see events that
// never arrive on its bus.
func TestRunnerLiveBlindToForeignProcessBus(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	h.seedJob(t, "FV1", 3)
	h.seedJob(t, "FV2", 3)

	fp := newFakeProcessor(t)
	fp.statuses = runningScript(400)

	// Two agents (the retired topology), each with its OWN process bus…
	busA, busB := events.NewBroker(), events.NewBroker()
	a := newDispatcher(t, h, fp, Config{RunnerName: "local", WorkerID: "fv-a"})
	a.SetEventBroker(busA)
	b := newDispatcher(t, h, fp, Config{RunnerName: "local", WorkerID: "fv-b"})
	b.SetEventBroker(busB)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go func() { _ = a.Run(ctxA) }()
	go func() { _ = b.Run(ctxB) }()

	// …and the API process: a THIRD bus nobody feeds, with the deriver on it.
	apiBus := events.NewBroker()
	apiView := events.NewRunnerLive(apiBus, log.New(io.Discard, "", 0))
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go apiView.Start(done)
	apiView.WaitReady()

	// Claims are real (DB) — the budget holds the pair to ONE lane on the
	// shared capacity-1 runner; whichever agent holds it, it is ACTIVE.
	deadline := time.Now().Add(3 * time.Second)
	for {
		active, _ := activeLaneStats(t, h)
		if active >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no active claim observed — repro precondition failed")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The API-process view sees NOTHING: its bus never carried the claims.
	if snap := apiView.Snapshot(); len(snap) != 0 {
		t.Fatalf("API-process view %v — expected blindness: events never arrive on a foreign process's bus (root cause #249)", snap)
	}
}
