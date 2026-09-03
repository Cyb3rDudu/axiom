package server

// #169 (Epic B · B3): the runner live view — state deriver, WS runners topic
// through the #168 machinery, REST snapshot, type identity, idle semantics,
// and the <1s live-switch proof (log-synchronized publish vs WS receipt).
// Hermetic: the canonical dispatch-loop event sequence is published onto a
// real broker (the dispatcher suite already proves a real ingest emits exactly
// these events); these tests prove the derivation and the machinery latency.

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
)

// startRunnerView wires a server with a broker + deriver (as main does) and
// returns the server and test harness.
func startRunnerView(t *testing.T) (*Server, *events.Broker, *httptest.Server) {
	t.Helper()
	broker := events.NewBroker()
	s := New(":0", log.Default())
	view := NewRunnerLive(broker, log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	s.SetRunnerLive(view)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go view.Start(done)
	// The deriver's subscription must be live before callers publish, or the
	// first events race the startup and are lost (surfaced under full-suite load).
	view.WaitReady()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, broker, ts
}

// TestRunnerLiveDerivationLifecycle drives the canonical dispatch sequence
// (claim -> stage x2 -> complete) and asserts the derived state at each step.
func TestRunnerLiveDerivationLifecycle(t *testing.T) {
	_, broker, _ := startRunnerView(t)

	sub := events.NewSubscription().WithMatch(func(e events.Event) bool {
		_, ok := e.(events.RunnerStateChanged)
		return ok
	})
	broker.Subscribe(sub, 16)
	done := make(chan struct{})
	close(done)

	// Claim: busy with the book identity.
	broker.Publish(events.JobClaimed{
		JobID: "job-1", WorkerID: "w-7", RunnerName: "carrier-gpu0",
		AttachmentFilename: "book.pdf", DocumentTitle: "The Book",
	})
	st := nextRunnerState(t, sub)
	if st.State != "busy" || st.JobID != "job-1" || st.RunnerName != "carrier-gpu0" {
		t.Fatalf("after claim: %+v", st)
	}
	if st.DocumentTitle != "The Book" || st.AttachmentFilename != "book.pdf" || st.WorkerID != "w-7" {
		t.Fatalf("claim identity: %+v", st)
	}
	if st.GPU != "gpu0" {
		t.Fatalf("GPU = %q, want gpu0 (from the runner-name config stamp)", st.GPU)
	}

	// Stage deltas update the current job's stage + hint.
	broker.Publish(events.JobStageChanged{JobID: "job-1", Stage: "convert", ProgressHint: "convert"})
	st = nextRunnerState(t, sub)
	if st.Stage != "convert" || st.ProgressHint != "convert" {
		t.Fatalf("after stage: %+v", st)
	}
	broker.Publish(events.JobStageChanged{JobID: "job-1", Stage: "extract"})
	st = nextRunnerState(t, sub)
	if st.Stage != "extract" {
		t.Fatalf("after stage 2: %+v", st)
	}

	// Complete: idle + last-job tail + counter.
	broker.Publish(events.JobCompleted{JobID: "job-1"})
	st = nextRunnerState(t, sub)
	if st.State != "idle" {
		t.Fatalf("after complete: state=%s, want idle", st.State)
	}
	if st.JobsCompleted != 1 {
		t.Fatalf("jobs_completed = %d, want 1", st.JobsCompleted)
	}
	if st.LastJobID != "job-1" || st.LastTitle != "The Book" {
		t.Fatalf("idle tail: %+v", st)
	}
	if st.LastEndedAtMs == 0 {
		t.Fatalf("idle tail missing the when (last_ended_at_ms)")
	}
	if st.JobID != "" || st.Stage != "" {
		t.Fatalf("idle must clear the current job: %+v", st)
	}

	// A second job on the same runner keeps the session counter.
	broker.Publish(events.JobClaimed{JobID: "job-2", RunnerName: "carrier-gpu0", DocumentTitle: "Second"})
	st = nextRunnerState(t, sub)
	if st.JobsCompleted != 1 || st.JobID != "job-2" {
		t.Fatalf("second claim: %+v", st)
	}
	broker.Publish(events.JobCompleted{JobID: "job-2"})
	st = nextRunnerState(t, sub)
	if st.JobsCompleted != 2 || st.LastTitle != "Second" {
		t.Fatalf("second complete: %+v", st)
	}
}

func nextRunnerState(t *testing.T, sub *events.Subscription) events.RunnerStateChanged {
	t.Helper()
	deadline := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(deadline) })
	for {
		e, _, ok := sub.Next(deadline)
		if !ok {
			t.Fatal("timed out waiting for a RunnerStateChanged event")
		}
		if st, is := e.(events.RunnerStateChanged); is {
			return st
		}
	}
}

// TestRunnerLiveIdleSemantics: a runner that never claimed (or finished long
// ago) shows idle; the view answers "is anything still running?".
func TestRunnerLiveIdleSemantics(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	broker.Publish(events.JobClaimed{
		JobID: "j", RunnerName: "mini", DocumentTitle: "T", AttachmentFilename: "t.epub",
	})
	broker.Publish(events.JobFailed{JobID: "j"})

	// REST snapshot: idle + last-job fields.
	deadline := time.Now().Add(3 * time.Second)
	var snap []events.RunnerStateChanged
	for time.Now().Before(deadline) {
		r, err := http.Get(ts.URL + "/api/runners/live")
		if err != nil {
			t.Fatalf("GET runners/live: %v", err)
		}
		_ = json.NewDecoder(r.Body).Decode(&snap)
		r.Body.Close()
		if len(snap) == 1 && snap[0].State == "idle" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v, want one runner", snap)
	}
	st := snap[0]
	if st.State != "idle" {
		t.Fatalf("state = %s, want idle", st.State)
	}
	if st.LastJobID != "j" || st.LastTitle != "T" || st.LastEndedAtMs == 0 {
		t.Fatalf("idle tail fields: %+v", st)
	}
	if st.JobsCompleted != 0 {
		t.Fatalf("a failed job does not count as completed: %+v", st)
	}
}

// TestRunnerLiveRESTAndWSIdentity pins the #169 type identity: the REST
// snapshot and the WS runners topic deliver structurally identical data (the
// same events.RunnerStateChanged values on the wire).
func TestRunnerLiveRESTAndWSIdentity(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	broker.Publish(events.JobClaimed{
		JobID: "ident-1", WorkerID: "w1", RunnerName: "carrier-gpu2",
		AttachmentFilename: "ident.pdf", DocumentTitle: "Ident Book",
	})

	// WS: subscribe runners, read the snapshot frame.
	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}
	wsFrame := readFrame(t, c)
	if wsFrame.Type != frameEvent || !wsFrame.Snapshot || wsFrame.Topic != "runners" {
		t.Fatalf("ws snapshot frame = %+v", wsFrame)
	}
	var fromWS events.RunnerStateChanged
	if b, err := json.Marshal(wsFrame.Payload); err != nil {
		t.Fatalf("re-marshal ws payload: %v", err)
	} else if err := json.Unmarshal(b, &fromWS); err != nil {
		t.Fatalf("unmarshal ws payload into RunnerStateChanged: %v", err)
	}

	// REST: the same struct.
	var fromREST []events.RunnerStateChanged
	r, err := http.Get(ts.URL + "/api/runners/live")
	if err != nil {
		t.Fatalf("GET runners/live: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("runners/live = %d", r.StatusCode)
	}
	if err := json.NewDecoder(r.Body).Decode(&fromREST); err != nil {
		t.Fatalf("decode rest: %v", err)
	}

	// THE TYPE TEST: the WS payload IS one of the REST entries (deep equal).
	if len(fromREST) == 0 {
		t.Fatalf("rest snapshot empty")
	}
	for _, st := range fromREST {
		if st == fromWS {
			return // identity proven
		}
	}
	t.Fatalf("WS payload %+v not structurally identical to any REST entry %+v", fromWS, fromREST)
}

// TestRunnerLiveLiveSwitchUnder1s is the <1s live proof: after the bus event,
// the WS runners subscriber sees the state change within one second
// (log-synchronized publish vs receipt timestamps).
func TestRunnerLiveLiveSwitchUnder1s(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want subscribed ack, got %+v", ack)
	}

	// The stage progression: claim, then the stage switch we time.
	broker.Publish(events.JobClaimed{JobID: "lat-1", RunnerName: "carrier-gpu1", DocumentTitle: "Latency"})
	// Drain the claim-derived frame.
	if f := readFrame(t, c); f.Type != frameEvent || f.Payload.(map[string]any)["job_id"] != "lat-1" {
		t.Fatalf("claim frame = %+v", f)
	}

	var publishedAt time.Time
	publishedAt = time.Now()
	broker.Publish(events.JobStageChanged{JobID: "lat-1", Stage: "extract", ProgressHint: "extract"})

	// Measure until the stage switch arrives on the WS stream.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for the stage switch: %v", err)
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type != frameEvent {
			continue
		}
		var st events.RunnerStateChanged
		if b, err := json.Marshal(f.Payload); err == nil {
			_ = json.Unmarshal(b, &st)
		}
		if st.Stage == "extract" {
			latency := time.Since(publishedAt)
			if latency >= time.Second {
				t.Fatalf("stage switch took %s, want <1s (event->deriver->bus->WS)", latency)
			}
			t.Logf("stage switch latency: %s (<1s proven)", latency)
			return
		}
	}
}

// TestRunnerLiveTopicIsolation: the runners topic runs through the #168
// WithMatch machinery — raw job events never reach a runners subscriber.
func TestRunnerLiveTopicIsolation(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want ack, got %+v", ack)
	}
	// A claim produces exactly ONE derived frame — never a raw job frame.
	broker.Publish(events.JobClaimed{JobID: "iso-1", RunnerName: "carrier-gpu0", DocumentTitle: "Iso"})
	f := readFrame(t, c)
	if f.Type != frameEvent || f.Topic != "runners" {
		t.Fatalf("frame = %+v, want a runners-topic event", f)
	}
	if p, ok := f.Payload.(map[string]any); !ok || p["kind"] != nil && p["runner_name"] == nil {
		t.Fatalf("payload is not a runner state: %+v", f.Payload)
	}
	// No further frames: the raw job event itself must not arrive.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, data, err := c.ReadMessage(); err == nil {
		t.Fatalf("runners subscriber received an extra (raw job) frame: %s", data)
	}
}

// TestRunnerLiveUnwiredREST404: without SetRunnerLive the REST route answers
// 404 (the repair-API pattern).
func TestRunnerLiveUnwiredREST404(t *testing.T) {
	s := New(":0", log.Default())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runners/live", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unwired /api/runners/live = %d, want 404", rec.Code)
	}
}

// TestGPULabel pins the #5c identity derivation: the GPU assignment comes
// from the runner-name configuration stamp, never from metrics.
func TestGPULabel(t *testing.T) {
	cases := map[string]string{
		"carrier-gpu0":   "gpu0",
		"mini_gpu3":      "gpu3",
		"node.gpu2.host": "gpu2",
		"plain-runner":   "",
		"gpu":            "", // too short to be a stamp
	}
	for name, want := range cases {
		if got := GPULabel(name); got != want {
			t.Errorf("GPULabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestRunnerLiveParallelWorkersPerRunner (#169 review F1): the dispatcher
// allows multiple parallel workers per runner, so a finished job must NOT
// flip the view to idle while another job is still active on the same
// runner — the pre-review single-slot model corrupted exactly this case.
func TestRunnerLiveParallelWorkersPerRunner(t *testing.T) {
	_, broker, _ := startRunnerView(t)
	sub := events.NewSubscription().WithMatch(func(e events.Event) bool {
		_, ok := e.(events.RunnerStateChanged)
		return ok
	})
	broker.Subscribe(sub, 32)
	done := make(chan struct{})
	close(done)

	// Two claims on the SAME runner (two parallel workers).
	broker.Publish(events.JobClaimed{JobID: "p1", WorkerID: "w1", RunnerName: "carrier-gpu0", DocumentTitle: "First"})
	broker.Publish(events.JobClaimed{JobID: "p2", WorkerID: "w2", RunnerName: "carrier-gpu0", DocumentTitle: "Second"})

	// Both claims produce views; the display shows the most recent claim.
	st := nextRunnerState(t, sub)
	if st.State != "busy" {
		t.Fatalf("after claim 1: %+v", st)
	}
	st = nextRunnerState(t, sub)
	if st.JobID != "p2" {
		t.Fatalf("display should be the most recent claim, got %+v", st)
	}

	// THE REVIEW CASE: job 1 completes while job 2 is still active.
	broker.Publish(events.JobCompleted{JobID: "p1"})
	st = nextRunnerState(t, sub)
	if st.State != "busy" || st.JobID != "p2" {
		t.Fatalf("job1 done while job2 active: state=%s job=%s, want busy/p2 — the view must NOT flip to idle", st.State, st.JobID)
	}
	if st.JobsCompleted != 1 {
		t.Fatalf("completed counter = %d, want 1 (job1 counted while job2 runs)", st.JobsCompleted)
	}

	// The last active job ending flips to idle; the tail names job 2.
	broker.Publish(events.JobCompleted{JobID: "p2"})
	st = nextRunnerState(t, sub)
	if st.State != "idle" || st.LastJobID != "p2" || st.JobsCompleted != 2 {
		t.Fatalf("after the last job: %+v, want idle/last=p2/completed=2", st)
	}
}

// TestRunnerLiveJobFilterSeesIdleTransition (#169 review F2): a runner
// subscriber filtered by job_id must see BOTH the busy frame AND the terminal
// idle transition — the idle frame carries the job in last_job_id.
func TestRunnerLiveJobFilterSeesIdleTransition(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners", "job_id": "jf-1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want ack, got %+v", ack)
	}

	// Claim + complete of the FILTERED job; another runner's noise in between.
	broker.Publish(events.JobClaimed{JobID: "other", RunnerName: "noise", DocumentTitle: "Noise"})
	broker.Publish(events.JobClaimed{JobID: "jf-1", RunnerName: "carrier-gpu0", DocumentTitle: "Filtered"})
	broker.Publish(events.JobCompleted{JobID: "other"})
	broker.Publish(events.JobCompleted{JobID: "jf-1"})

	// Busy frame for jf-1 must arrive (the claim).
	sawBusy, sawIdle := false, false
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil || f.Type != frameEvent {
			continue
		}
		var st events.RunnerStateChanged
		if b, err := json.Marshal(f.Payload); err == nil {
			_ = json.Unmarshal(b, &st)
		}
		if st.RunnerName != "carrier-gpu0" {
			continue // the noise runner is filtered out
		}
		if st.JobID == "jf-1" && st.State == "busy" {
			sawBusy = true
		}
		if st.State == "idle" && st.LastJobID == "jf-1" {
			sawIdle = true
		}
		if sawBusy && sawIdle {
			return // both transitions witnessed through the job filter
		}
	}
	if !sawBusy || !sawIdle {
		t.Fatalf("job-filtered runner subscriber saw busy=%v idle=%v, want both", sawBusy, sawIdle)
	}
}

// TestRunnerLiveWiringOrderInverted (#169 review F3): SetRunnerLive BEFORE
// SetWSAPI must still wire the WS runners snapshot — both orders end wired.
func TestRunnerLiveWiringOrderInverted(t *testing.T) {
	broker := events.NewBroker()
	s := New(":0", log.Default())
	view := NewRunnerLive(broker, log.Default())
	// INVERTED order: deriver first, WS second.
	s.SetRunnerLive(view)
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	if s.ws == nil || s.ws.runnerSnap == nil {
		t.Fatalf("SetWSAPI after SetRunnerLive left the runners snapshot unwired (order inversion)")
	}
}

// TestRunnerLiveJobFilterParallelTerminal (#169 r3 F1): a p1-filtered runner
// subscriber must see p1's terminal transition even while p2 stays active on
// the SAME runner — the derived state then carries JobID=p2 AND LastJobID=p1,
// so a single-value match never reaches the tail and loses the frame.
func TestRunnerLiveJobFilterParallelTerminal(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners", "job_id": "p1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want ack, got %+v", ack)
	}

	// Two parallel jobs on one runner; p1 is the FILTERED one.
	broker.Publish(events.JobClaimed{JobID: "p1", RunnerName: "carrier-gpu0", DocumentTitle: "P1"})
	broker.Publish(events.JobClaimed{JobID: "p2", RunnerName: "carrier-gpu0", DocumentTitle: "P2"})
	// p1 completes while p2 stays active: the state carries JobID=p2,
	// LastJobID=p1 — p1's subscriber MUST still see this terminal frame.
	broker.Publish(events.JobCompleted{JobID: "p1"})

	sawBusy, sawTerminal := false, false
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil || f.Type != frameEvent {
			continue
		}
		var st events.RunnerStateChanged
		if b, err := json.Marshal(f.Payload); err == nil {
			_ = json.Unmarshal(b, &st)
		}
		if st.RunnerName != "carrier-gpu0" {
			continue
		}
		if st.JobID == "p1" && st.State == "busy" {
			sawBusy = true
		}
		// THE PARALLEL TERMINAL: p1 finished (tail) while p2 runs (current).
		if st.LastJobID == "p1" && st.JobID == "p2" {
			sawTerminal = true
		}
		if sawBusy && sawTerminal {
			return // both witnessed through the p1 filter
		}
	}
	if !sawBusy || !sawTerminal {
		t.Fatalf("p1-filtered subscriber: busy=%v parallel-terminal=%v, want both", sawBusy, sawTerminal)
	}
}

// TestRunnerLiveSnapshotRespectsJobFilter (#169 r3 F2): a job-filtered
// runners subscriber sees ONLY the states relevant to its job in the connect
// snapshot — same match semantics as the live stream (JobID OR LastJobID).
func TestRunnerLiveSnapshotRespectsJobFilter(t *testing.T) {
	_, broker, ts := startRunnerView(t)
	// Seed two runners: one with the filtered job active, one foreign.
	broker.Publish(events.JobClaimed{JobID: "mine", RunnerName: "mine-runner", DocumentTitle: "Mine"})
	broker.Publish(events.JobClaimed{JobID: "theirs", RunnerName: "other-runner", DocumentTitle: "Theirs"})
	// Wait until the deriver has folded both (REST grows to 2).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := http.Get(ts.URL + "/api/runners/live")
		var snap []events.RunnerStateChanged
		_ = json.NewDecoder(r.Body).Decode(&snap)
		r.Body.Close()
		if len(snap) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := dialWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws")
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "runners", "job_id": "mine"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want ack, got %+v", ack)
	}

	// Snapshot frames: ONLY the mine-runner state may appear.
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return // idle: snapshot complete, no foreign frame arrived
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil || !f.Snapshot {
			continue
		}
		var st events.RunnerStateChanged
		if b, err := json.Marshal(f.Payload); err == nil {
			_ = json.Unmarshal(b, &st)
		}
		if st.RunnerName != "mine-runner" {
			t.Fatalf("job-filtered snapshot delivered a foreign runner state: %+v", st)
		}
		if st.JobID != "mine" {
			t.Fatalf("snapshot state for the wrong job: %+v", st)
		}
	}
}
