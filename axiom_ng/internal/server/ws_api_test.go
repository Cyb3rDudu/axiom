package server

// #168 (Epic B · B2): /api/ws — subscribe topics, snapshot-then-live, bounded
// backpressure (gap markers), token auth. Hermetic: a fake snapshot source and
// a real events.Broker (the dispatcher's live stream is simulated by publishing
// to the broker directly), no Postgres, no dispatcher.

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestWSAuthRejectsUnauthorized verifies the token gate on a NON-loopback
// handshake. Cases:
//   - no WS secret configured + non-loopback peer -> denied (404, unwired)
//   - secret configured + wrong/missing token on non-loopback -> denied (403)
//   - secret configured + correct token (query or bearer) -> allowed
//   - loopback peer (secret or not) -> always allowed (house rule)
//
// The auth decision is exercised on the wsServer method with a crafted request
// whose RemoteAddr is non-loopback (an httptest server always binds loopback,
// so the loopback-trusted path cannot reach the rejections hermetically).
func TestWSAuthRejectsUnauthorized(t *testing.T) {
	ws := &wsServer{token: "s3cret"}
	nonLoop := func(path string, hdr http.Header) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+path, nil)
		r.RemoteAddr = "10.0.0.5:4321" // non-loopback peer
		r.Header = hdr
		return r
	}

	// No token/header + non-loopback peer with a configured secret -> 403.
	r := nonLoop("example.com/api/ws", http.Header{})
	if got := ws.authorize(r); got != http.StatusForbidden {
		t.Fatalf("no token on non-loopback = %d, want 403", got)
	}

	// Wrong token -> 403.
	r = nonLoop("example.com/api/ws?token=wrong", http.Header{})
	if got := ws.authorize(r); got != http.StatusForbidden {
		t.Fatalf("wrong token = %d, want 403", got)
	}

	// Correct query-param token -> 200.
	r = nonLoop("example.com/api/ws?token=s3cret", http.Header{})
	if got := ws.authorize(r); got != http.StatusOK {
		t.Fatalf("correct query token = %d, want 200", got)
	}

	// Correct Authorization: Bearer token -> 200.
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer s3cret")
	r = nonLoop("example.com/api/ws", hdr)
	if got := ws.authorize(r); got != http.StatusOK {
		t.Fatalf("correct bearer token = %d, want 200", got)
	}

	// *** Counter-proof for the review finding ***: EMPTY token config + a
	// non-loopback peer must be DENIED (404, indistinguishable from unwired),
	// not allowed. The pre-ruling code allowed this.
	open := &wsServer{token: ""} // no secret: loopback-only
	r = nonLoop("example.com/api/ws", http.Header{})
	if got := open.authorize(r); got != http.StatusNotFound {
		t.Fatalf("empty token config + non-loopback = %d, want 404 (denied)", got)
	}

	// Loopback peer + a configured secret -> trusted (house rule).
	loop := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/ws", nil)
	loop.RemoteAddr = "127.0.0.1:1234"
	if got := ws.authorize(loop); got != http.StatusOK {
		t.Fatalf("loopback peer = %d, want 200 (trusted)", got)
	}

	// Loopback peer + empty secret -> trusted too.
	if got := open.authorize(loop); got != http.StatusOK {
		t.Fatalf("loopback peer no secret = %d, want 200 (trusted)", got)
	}
}

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

// The following exercise the #168 review fixes.

// TestWSOutboxSubscriberGetsNoSnapshot (fix: snapshot topic semantics). An
// outbox subscriber must receive NO job snapshot — job state is only
// meaningful to a jobs-relevant subscription.
func TestWSOutboxSubscriberGetsNoSnapshot(t *testing.T) {
	broker := events.NewBroker()
	// The fake snapshot HAS a job; an outbox subscriber must not see it.
	snap := fakeSnapshot{jobs: []repo.Job{{ID: "jobsnap-1", Status: "pending"}}}
	s := New(":0", log.Default())
	s.SetWSAPI(broker, snap, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "outbox", "")
	// Only the subscribed ack should arrive — no job-snapshot frames.
	ack := readFrame(t, c)
	if ack.Type != frameSubscribed {
		t.Fatalf("first frame = %+v, want subscribed ack", ack)
	}
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, data, err := c.ReadMessage()
	if err == nil {
		t.Fatalf("outbox subscriber received an extra frame: %s", data)
	}
}

// TestWSAllSubscriberJobSnapshotsAreTopicJobs (fix: snapshot topic). An
// `all` subscription still gets the job snapshot, but every snapshot frame
// carries topic "jobs" (not "all").
func TestWSAllSubscriberJobSnapshotsAreTopicJobs(t *testing.T) {
	broker := events.NewBroker()
	snap := fakeSnapshot{jobs: []repo.Job{{ID: "all-snap-1", Status: "processing"}}}
	s := New(":0", log.Default())
	s.SetWSAPI(broker, snap, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "all", "")
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want subscribed ack, got %+v", ack)
	}
	snapFrame := readFrame(t, c)
	if snapFrame.Type != frameEvent || !snapFrame.Snapshot {
		t.Fatalf("want a job snapshot frame, got %+v", snapFrame)
	}
	if snapFrame.Topic != "jobs" {
		t.Fatalf("all-subscriber snapshot topic = %q, want \"jobs\"", snapFrame.Topic)
	}
	if p, _ := snapFrame.Payload.(map[string]any); p["job_id"] != "all-snap-1" {
		t.Fatalf("snapshot payload = %v, want all-snap-1", snapFrame.Payload)
	}
}

// TestWSSilentClientCloseReleasesSubscription (fix: subscription leak). A
// client that connects, subscribes, then goes away SILENTLY (raw socket close,
// no close frame, no further events) must release its broker subscription: the
// live loop cancels via the read-pump and Broker.Subscribers returns to the
// pre-connection baseline.
func TestWSSilentClientCloseReleasesSubscription(t *testing.T) {
	broker := events.NewBroker()
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	base := broker.Subscribers()

	c, _, err := websocket.DefaultDialer.Dial(urlFor(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "topic": "jobs"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Wait for the handler to register its broker subscription.
	deadline := time.Now().Add(3 * time.Second)
	for broker.Subscribers() <= base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := broker.Subscribers(); got <= base {
		t.Fatalf("broker sub count after connect = %d, want > base %d", got, base)
	}

	// Close the TCP connection WITHOUT a WS close frame (silent disconnect).
	_ = c.UnderlyingConn().SetDeadline(time.Now())
	_ = c.UnderlyingConn().Close()

	// The read-pump must notice and the deferred Unsubscribe must run.
	deadline = time.Now().Add(3 * time.Second)
	for broker.Subscribers() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := broker.Subscribers(); got != base {
		t.Fatalf("silent client left %d broker subscriptions, want back to base %d", got, base)
	}
}

// TestWSOutboxSubscriberNoDropsUnderJobFlood (fix: filter before the broker
// queue). An outbox subscriber flooded with irrelevant JOB events must suffer
// ZERO drops/gaps — job events are filtered before the bounded queue, so they
// cannot displace (or fake-gap) the outbox events it wants.
func TestWSOutboxSubscriberNoDropsUnderJobFlood(t *testing.T) {
	broker := events.NewBroker()
	// Register the pre-existing slow-subscriber count check as well.
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, urlFor(ts))
	sendSubscribe(t, c, "outbox", "")
	if ack := readFrame(t, c); ack.Type != frameSubscribed {
		t.Fatalf("want subscribed ack, got %+v", ack)
	}

	// The consumer drains in lockstep, but flood MANY outbox-irrelevant job
	// events first — none may occupy/overflow the outbox subscription's queue.
	for i := 0; i < 500; i++ {
		broker.Publish(events.JobClaimed{JobID: "job-flood"})
	}
	// A single relevant outbox event must get through with zero drops.
	broker.Publish(events.OutboxDrained{Count: 1})

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	sawOutbox := false
	sawGap := false
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		var f outFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == frameGap {
			sawGap = true
		}
		if f.Type == frameEvent {
			if p, _ := f.Payload.(map[string]any); p["kind"] == "outbox_drained" {
				sawOutbox = true
			}
		}
		if sawOutbox {
			break
		}
	}
	if !sawOutbox {
		t.Fatalf("outbox subscriber never got the outbox event under job flood")
	}
	if sawGap {
		t.Fatalf("outbox subscriber saw a gap from irrelevant job events (filter must precede the queue)")
	}
}

// TestWSCSWSHForeignOriginRejected (#168 r3): a browser context announces
// itself via the Origin header. A FOREIGN origin with no token configured
// must be rejected EVEN from a loopback peer — otherwise any website open in
// a local browser could read job metadata through ws://127.0.0.1 (cross-site
// WebSocket hijacking). The loopback trust applies to processes (no Origin
// header), not to browser contexts.
func TestWSCSWSHForeignOriginRejected(t *testing.T) {
	// Empty token config (the loopback-only default) — the vulnerable setup.
	open := &wsServer{token: ""}
	mk := func(origin string, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8011/api/ws", nil)
		r.RemoteAddr = remote
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	// Counter-proof: foreign Origin + empty token, from a LOOPBACK peer -> 403.
	if got := open.authorize(mk("https://evil.example", "127.0.0.1:4444")); got != http.StatusForbidden {
		t.Fatalf("foreign origin + empty token from loopback = %d, want 403 (CSWSH)", got)
	}

	// Same-origin page (served by this API) still allowed on loopback.
	if got := open.authorize(mk("http://127.0.0.1:8011", "127.0.0.1:4444")); got != http.StatusOK {
		t.Fatalf("same-origin loopback = %d, want 200", got)
	}

	// No Origin header (a process: curl, in-process dialer) keeps the peer
	// rule: loopback trusted.
	if got := open.authorize(mk("", "127.0.0.1:4444")); got != http.StatusOK {
		t.Fatalf("origin-less loopback process = %d, want 200", got)
	}

	// Foreign origin WITH a valid token is allowed (explicitly authorized).
	sec := &wsServer{token: "s3cret"}
	r := mk("https://evil.example", "127.0.0.1:4444")
	r.URL.RawQuery = "token=s3cret"
	if got := sec.authorize(r); got != http.StatusOK {
		t.Fatalf("foreign origin + valid token = %d, want 200", got)
	}

	// End-to-end: a real dial carrying a foreign Origin header is refused.
	broker := events.NewBroker()
	s := New(":0", log.Default())
	s.SetWSAPI(broker, fakeSnapshot{}, "")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	hdr := http.Header{}
	hdr.Set("Origin", "https://evil.example")
	_, resp, err := websocket.DefaultDialer.Dial(urlFor(ts), hdr)
	if err == nil {
		t.Fatalf("foreign-origin dial succeeded; want refusal")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin handshake status = %v, want 403", resp)
	}
}

// TestWSWriterClosesSocketOnWriteError (#168 r3): the writer goroutine owns
// the socket close on every exit path. After a write failure the server-side
// socket must be CLOSED (previously the error path returned without closing
// and the ctx.Done() branch never ran).
func TestWSWriterClosesSocketOnWriteError(t *testing.T) {
	// Stand up a raw upgraded pair so the test owns the server-side conn.
	srvConnCh := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConnCh <- conn
	}))
	defer ts.Close()

	client, _, err := websocket.DefaultDialer.Dial(urlFor(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srv := <-srvConnCh
	defer srv.Close()

	// Break the socket so the server's next write fails, then run the writer
	// pump with one queued frame. cancelCalled observes the error exit.
	// SO_LINGER(0) forces an RST on Close: a plain FIN leaves the server's
	// socket writable (data drains into the kernel buffer), so the write
	// would not fail deterministically.
	if tcp, ok := client.UnderlyingConn().(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = client.UnderlyingConn().Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelCalled := make(chan struct{})
	var once sync.Once
	out := make(chan []byte, 1)
	out <- []byte(`{"type":"event"}`)
	startWriterPump(srv, ctx, func() { once.Do(func() { cancel(); close(cancelCalled) }) }, out, time.Hour)

	// The write must fail and the writer must exit through cancel().
	select {
	case <-cancelCalled:
	case <-time.After(5 * time.Second):
		t.Fatalf("writer never exited on write failure")
	}

	// PROOF: the server-side socket is closed. On a closed conn every
	// subsequent operation reports "use of closed network connection"; a
	// merely broken (but open) fd would not.
	deadline := time.Now().Add(3 * time.Second)
	closed := false
	for time.Now().Before(deadline) {
		if err := srv.UnderlyingConn().SetDeadline(time.Now()); err != nil && strings.Contains(err.Error(), "closed") {
			closed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !closed {
		t.Fatalf("server socket not closed after write error (SetDeadline err=%v)", err)
	}
}

// TestWSSetWSAPINilBrokerKeeps404 (#168 r3): SetWSAPI with a nil broker must
// honor the documented opt-out — the route stays unwired/404 instead of
// leaving a half-wired wsServer that would nil-deref later.
func TestWSSetWSAPINilBrokerKeeps404(t *testing.T) {
	s := New(":0", log.Default())
	s.SetWSAPI(events.NewBroker(), fakeSnapshot{}, "") // wire it once
	s.SetWSAPI(nil, nil, "")                           // nil broker unwires again
	if s.ws != nil {
		t.Fatalf("SetWSAPI(nil, ...) left s.ws set; want nil (documented opt-out)")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("after SetWSAPI(nil) /api/ws = %d, want 404", rec.Code)
	}
}

// TestWSRebindingAliasRejected (#168 r4): both Origin and Host are
// client-controlled, so a matching pair proves nothing. DNS rebinding —
// evil.example resolving to 127.0.0.1 — puts a browser at
// http://evil.example:8011 with Origin/Host matching; that context must NOT
// get loopback trust. Same-origin applies only to canonical local hosts.
func TestWSRebindingAliasRejected(t *testing.T) {
	// Empty token config (loopback-only default) — the vulnerable setup.
	open := &wsServer{token: ""}
	// A request whose Host is the attacker's rebinding alias, arriving from a
	// LOOPBACK peer (evil.example -> 127.0.0.1), Origin matching the Host.
	mk := func(origin, host string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/ws", nil)
		r.Host = host
		r.RemoteAddr = "127.0.0.1:4444"
		r.Header.Set("Origin", origin)
		return r
	}

	// THE REBINDING REPRO: Origin == Host == evil.example:8011, loopback peer,
	// no token -> 403. The pre-r4 code treated this as same-origin -> 200.
	if got := open.authorize(mk("http://evil.example:8011", "evil.example:8011")); got != http.StatusForbidden {
		t.Fatalf("rebinding alias (Origin==Host=evil.example:8011, loopback, no token) = %d, want 403", got)
	}
	// No port spelled either way — still not canonical, still rejected.
	if got := open.authorize(mk("http://evil.example", "evil.example")); got != http.StatusForbidden {
		t.Fatalf("rebinding alias without port = %d, want 403", got)
	}
	// Non-http(s) scheme on a loopback host is not a browser page origin.
	if got := open.authorize(mk("ftp://127.0.0.1:8011", "127.0.0.1:8011")); got != http.StatusForbidden {
		t.Fatalf("ftp-scheme origin on canonical host = %d, want 403", got)
	}

	// Counter-probes (round-3 semantics unchanged):
	// Genuine same-origin page on a canonical local host -> 200.
	if got := open.authorize(mk("http://127.0.0.1:8011", "127.0.0.1:8011")); got != http.StatusOK {
		t.Fatalf("same-origin 127.0.0.1 = %d, want 200", got)
	}
	if got := open.authorize(mk("http://localhost:8011", "localhost:8011")); got != http.StatusOK {
		t.Fatalf("same-origin localhost = %d, want 200", got)
	}
	if got := open.authorize(mk("http://[::1]:8011", "[::1]:8011")); got != http.StatusOK {
		t.Fatalf("same-origin [::1] = %d, want 200", got)
	}
	// TLS-fronted same-origin on a canonical host -> 200.
	if got := open.authorize(mk("https://localhost:8011", "localhost:8011")); got != http.StatusOK {
		t.Fatalf("https same-origin localhost = %d, want 200", got)
	}
	// Origin-less processes keep the peer rule: loopback -> 200.
	noOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8011/api/ws", nil)
	noOrigin.RemoteAddr = "127.0.0.1:4444"
	if got := open.authorize(noOrigin); got != http.StatusOK {
		t.Fatalf("origin-less loopback process = %d, want 200", got)
	}
	// Foreign origin WITH a valid token -> 200 (explicitly authorized).
	sec := &wsServer{token: "s3cret"}
	r := mk("https://evil.example", "evil.example:8011")
	r.URL.RawQuery = "token=s3cret"
	if got := sec.authorize(r); got != http.StatusOK {
		t.Fatalf("foreign origin + valid token = %d, want 200", got)
	}
}
