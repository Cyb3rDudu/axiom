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
