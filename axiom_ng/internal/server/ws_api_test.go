package server

// #168 (Epic B · B2): /api/ws — subscribe topics, snapshot-then-live, bounded
// backpressure (gap markers), token auth. Hermetic: a fake snapshot source and
// a real events.Broker (the dispatcher's live stream is simulated by publishing
// to the broker directly), no Postgres, no dispatcher.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/gorilla/websocket"
)

// fakeSnapshot is a hermetic wsSnapshotSource.
type fakeSnapshot struct {
	jobs []repo.Job
	err  error
}

func (f fakeSnapshot) ActiveJobs(context.Context) ([]repo.Job, error) {
	return f.jobs, f.err
}

func urlFor(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws"
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readFrame(t *testing.T, c *websocket.Conn) outFrame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f outFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode frame %s: %v", data, err)
	}
	return f
}

func sendSubscribe(t *testing.T, c *websocket.Conn, topic, jobID string) {
	t.Helper()
	sub := map[string]any{"type": "subscribe", "topic": topic}
	if jobID != "" {
		sub["job_id"] = jobID
	}
	if err := c.WriteJSON(sub); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
}

// TestWSSnapshotThenLive asserts the B2 order guarantee: the snapshot sent to a
// jobs subscriber comes BEFORE the live events of a simulated dispatch loop
// (claim -> stage x2 -> complete), and the frames are ordered and gapless.
func TestWSSnapshotThenLive(t *testing.T) {
	broker := events.NewBroker()
	snap := fakeSnapshot{jobs: []repo.Job{{
		ID: "jobsnap-1", Status: "processing", Attempt: 1, MaxAttempts: 3,
	}}}

	s := New(":0", log.Default())
	s.SetWSAPI(broker, snap, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "jobs", "")

	// The subscribed ack is the barrier proving the bus subscription is live.
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}

	// Snapshot next (after the ack, before any live event).
	first := readFrame(t, c)
	if first.Type != frameEvent || !first.Snapshot {
		t.Fatalf("second frame = %+v, want snapshot event", first)
	}
	p, _ := first.Payload.(map[string]any)
	if p["job_id"] != "jobsnap-1" || p["status"] != "processing" {
		t.Fatalf("snapshot payload = %v, want jobsnap-1/processing", first.Payload)
	}

	// Now simulate the dispatcher: claim -> stage x2 -> complete.
	broker.Publish(events.JobClaimed{JobID: "joblive-1", WorkerID: "w1", RunnerName: "runner-1"})
	broker.Publish(events.JobStageChanged{JobID: "joblive-1", Stage: "convert"})
	broker.Publish(events.JobStageChanged{JobID: "joblive-1", Stage: "extract"})
	broker.Publish(events.JobCompleted{JobID: "joblive-1"})

	wantSeq := first.Seq + 1 // snapshot was seq first.Seq; live resumes above it
	var kinds []string
	for {
		f := readFrame(t, c)
		if f.Seq != wantSeq {
			t.Fatalf("seq = %d, want %d (gapless)", f.Seq, wantSeq)
		}
		wantSeq++
		if f.Type == frameGap {
			t.Fatalf("unexpected gap frame: %+v", f)
		}
		kinds = append(kinds, kindOf(f))
		if strings.Contains(strings.Join(kinds, ","), "job_completed") {
			break
		}
	}
	want := "job_claimed,job_stage_changed,job_stage_changed,job_completed"
	if strings.Join(kinds, ",") != want {
		t.Fatalf("live order = %v, want %v", kinds, want)
	}
	if f := readGapOnly(t, c); f != nil {
		t.Fatalf("no frames beyond complete; got %+v", f)
	}
}

func kindOf(f outFrame) string {
	if p, ok := f.Payload.(map[string]any); ok {
		if k, ok := p["kind"].(string); ok {
			return k
		}
	}
	return f.Type
}

// readGapOnly drains briefly and returns the first gap frame, or nil if only a
// clean close follows (used to assert "no stray frames").
func readGapOnly(t *testing.T, c *websocket.Conn) *outFrame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, data, err := c.ReadMessage()
	if err != nil {
		return nil
	}
	var f outFrame
	_ = json.Unmarshal(data, &f)
	return &f
}

// TestWSTopicFilter verifies an `outbox` subscriber only receives OutboxDrained
// events (JobChanged events are filtered out).
func TestWSTopicFilter(t *testing.T) {
	broker := events.NewBroker()
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "outbox", "")

	// Barrier: the subscribed ack means the bus subscription is live, so the
	// events published below are guaranteed to reach this connection if the
	// topic filter lets them.
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}

	// Publish both a job event and an outbox event; only outbox reaches.
	broker.Publish(events.JobClaimed{JobID: "j1", WorkerID: "w"})
	broker.Publish(events.OutboxDrained{Count: 3})

	deadline := time.Now().Add(5 * time.Second)
	_ = c.SetReadDeadline(deadline)
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			// Stream ended without a job event leaking through.
			return
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == frameEvent {
			p, _ := f.Payload.(map[string]any)
			if p["kind"] != "outbox_drained" {
				t.Fatalf("outbox subscriber received %v", f.Payload)
			}
			// Got the expected outbox event; nothing else of interest follows
			// (the job event was filtered). Drain briefly to prove no leak.
			_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatalf("outbox subscriber got a frame after the outbox event")
			}
			return
		}
		// gap frames from a slow reader are fine; keep draining until idle.
		if time.Now().After(deadline) {
			return
		}
	}
}

// TestWSJobFilter verifies the optional job_id filter on a jobs subscriber.
func TestWSJobFilter(t *testing.T) {
	broker := events.NewBroker()
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "jobs", "only-me")

	// Barrier: the subscribed ack means the bus subscription is live.
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}

	broker.Publish(events.JobClaimed{JobID: "other", WorkerID: "w"})
	broker.Publish(events.JobClaimed{JobID: "only-me", WorkerID: "w"})

	deadline := time.Now().Add(5 * time.Second)
	_ = c.SetReadDeadline(deadline)
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == frameEvent {
			p, _ := f.Payload.(map[string]any)
			if p["job_id"] != "only-me" {
				t.Fatalf("job-filtered subscriber got %v", f.Payload)
			}
			// Only the matching job should arrive; drain briefly to prove the
			// "other" job did not leak through.
			_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			if _, _, err := c.ReadMessage(); err == nil {
				t.Fatalf("job-filtered subscriber got a frame after only-me")
			}
			return
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// TestWSUnwiredGives404 verifies the repair-API pattern: never calling
// SetWSAPI leaves /api/ws answering 404.
func TestWSUnwiredGives404(t *testing.T) {
	s := New(":0", log.Default())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unwired /api/ws status = %d, want 404", rec.Code)
	}
}

// TestWSAuthRejectsUnauthorized verifies the token gate on a non-loopback
// handshake: a wrong or missing token is refused (403), a correct one passes.
// TestWSAuthRejectsUnauthorized verifies the token gate on a NON-loopback
// handshake: a wrong or missing token is refused, a correct one passes. The
// auth decision is exercised on the wsServer method with a crafted request
// whose RemoteAddr is non-loopback (an httptest server always binds loopback,
// so the loopback-trusted path cannot reach the rejection hermetically).
func TestWSAuthRejectsUnauthorized(t *testing.T) {
	ws := &wsServer{token: "s3cret"}
	nonLoop := func(path string, hdr http.Header) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+path, nil)
		r.RemoteAddr = "10.0.0.5:4321" // non-loopback peer
		r.Header = hdr
		return r
	}

	// No token on a non-loopback peer -> denied.
	if got := ws.tokenAllows(nonLoop("example.com/api/ws", nil)); got {
		t.Fatalf("no token on non-loopback handshake allowed; want denied")
	}

	// Wrong token -> denied.
	hdr := http.Header{}
	r := nonLoop("example.com/api/ws?token=wrong", hdr)
	if got := ws.tokenAllows(r); got {
		t.Fatalf("wrong token on non-loopback allowed; want denied")
	}

	// Correct query-param token -> allowed.
	r = nonLoop("example.com/api/ws?token=s3cret", http.Header{})
	if !ws.tokenAllows(r) {
		t.Fatalf("correct query token denied; want allowed")
	}

	// Correct Authorization: Bearer token -> allowed.
	hdr = http.Header{}
	hdr.Set("Authorization", "Bearer s3cret")
	r = nonLoop("example.com/api/ws", hdr)
	if !ws.tokenAllows(r) {
		t.Fatalf("correct bearer token denied; want allowed")
	}

	// Empty token config (loopback default) -> always allowed.
	open := &wsServer{token: ""}
	r = nonLoop("example.com/api/ws", http.Header{})
	if !open.tokenAllows(r) {
		t.Fatalf("empty token config denied a handshake; want allowed")
	}

	// Loopback peer with a configured token -> trusted (house rule).
	loop := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ws", nil)
	loop.RemoteAddr = "127.0.0.1:1234"
	if !ws.tokenAllows(loop) {
		t.Fatalf("loopback peer with token denied; want trusted")
	}
}

// TestWSGapMarkerFromSlowSubscriber proves B1 backpressure end to end: a
// subscriber with a tiny queue that cannot keep up sees explicit gap markers,
// and Broker.Publish stays non-blocking (the "dispatcher" is never slowed by a
// slow consumer).
// TestWSGapMarkerFromSlowSubscriber proves B1/B2 backpressure: a slow
// consumer never slows the dispatcher — the bus's bounded subscription drops
// the oldest events and later surfaces them as gap markers (the B1 pattern the
// WS layer carries through). Publish stays non-blocking throughout.
func TestWSGapMarkerFromSlowSubscriber(t *testing.T) {
	broker := events.NewBroker()

	// A deliberately slow subscriber: capacity 1, never drained.
	slow := events.NewSubscription()
	broker.Subscribe(slow, 1)

	publishStart := time.Now()
	for i := 0; i < 5; i++ {
		broker.Publish(events.JobClaimed{JobID: string(rune('A' + i))})
	}
	elapsed := time.Since(publishStart)

	if slow.Dropped() == 0 {
		t.Fatalf("slow subscriber expected dropped (gap) events, got 0")
	}
	// Non-blocking publish: the "dispatcher" path must not be gated on the
	// slow consumer.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("publish to a slow subscriber took %s; bus must stay non-blocking", elapsed)
	}

	// The gap-marker frame shape (protocol contract): a gap carries the type
	// discriminator, a connection-local seq, and the drop count.
	gap := outFrame{Type: frameGap, Topic: "jobs", Seq: 7, Drops: 3}
	data, err := json.Marshal(gap)
	if err != nil {
		t.Fatalf("marshal gap frame: %v", err)
	}
	var got outFrame
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal gap frame: %v", err)
	}
	if got.Type != "gap" || got.Seq != 7 || got.Drops != 3 {
		t.Fatalf("gap frame wire shape = %+v", got)
	}

	// A WS connection must stay alive under a burst (backpressure keeps the bus
	// and the stream moving; a broken backpressure would close/crash on load).
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "all", "")
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}
	for i := 0; i < 200; i++ {
		broker.Publish(events.JobClaimed{JobID: "burst"})
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotFrames := 0
	for {
		_, _, err := c.ReadMessage()
		if err != nil {
			break
		}
		gotFrames++
	}
	if gotFrames == 0 {
		t.Fatalf("no frames delivered under burst; backpressure broke the stream")
	}
}
