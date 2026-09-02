// WebSocket live endpoint (/api/ws, Epic B · B2, #168).
//
// A read-only, subscribable view over the B1 event bus (internal/events).
// Protocol (JSON frames, one per WebSocket text message):
//
//	client -> server (first frame only):
//	  {"type":"subscribe","topic":"jobs"}   // topics: jobs | outbox | all
//	  {"type":"subscribe","topic":"jobs","job_id":"doc-1"} // optional job filter
//
//	server -> client:
//	  {"type":"event","topic":"jobs","seq":1,"snapshot":true,"payload":{...}}
//	  {"type":"gap","topic":"jobs","seq":2,"drops":3}
//
// Ordering is connection-local via a monotonic `seq`. Snapshot frames carry
// snapshot:true, always on topic "jobs", and arrive FIRST (the DB's in-flight
// jobs); live frames carry snapshot:false. Snapshot and live share the same
// seq stream, so a client sees gapless, duplicate-free ordering.
//
// Backpressure follows the B1 pattern end to end:
//   - the subscription is pre-filtered (WithMatch on the B1 bus) and bounded,
//     so only events this subscriber asked for occupy its queue; a consumer
//     that drains slowly sees those lost events surfaced as `gap` frames.
//   - the final hop to the socket is a bounded send queue with a write
//     deadline; a connection that falls behind is CLOSED (the reconnect
//     contract), never left to leak memory or block the bus or the dispatcher.
//   - a client that disconnects (silently or not) tears the loop down via a
//     read-pump that cancels the connection context, releasing the bus
//     subscription — no leak on a vanished peer.
package server

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/gorilla/websocket"
)

// wsSnapshotSource is the DB-backed "current state" reader for the snapshot
// half of snapshot-then-live. *repo.Repo satisfies it.
type wsSnapshotSource interface {
	ActiveJobs(ctx context.Context) ([]repo.Job, error)
}

// wsServer is the configuration for the /api/ws endpoint. nil on the Server
// keeps the route 404 (the repair-API pattern — unwired is indistinguishable
// from absent).
type wsServer struct {
	broker   *events.Broker
	snapshot wsSnapshotSource
	token    string // non-empty => required on a non-loopback WS handshake
	log      *log.Logger
}

// wsQueue bounds the per-connection bus subscription and the send queue to the
// socket. Small enough that a slow client degrades to gap/close instead of
// unbounded memory.
const wsQueue = 64

// Heartbeat / teardown timing for the write/read pumps.
const (
	wsWriteWait  = 5 * time.Second  // max time a single socket write may take
	wsPongWait   = 60 * time.Second // how long we wait for a peer pong before teardown
	wsPingPeriod = 30 * time.Second // heartbeat to prove the peer is alive
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// frame type discriminators.
const (
	frameEvent = "event"
	frameGap   = "gap"
	// frameSubscribed is a control frame acknowledging that the bus
	// subscription is live (no seq — it precedes the first snapshot seq).
	frameSubscribed = "subscribed"
)

// SetWSAPI wires the /api/ws live endpoint. Calling it with a nil broker (or
// never calling it) keeps the route answering 404, indistinguishable from no
// route (the sourceSecret/repair-API pattern). token is the optional shared
// WS secret required on a non-loopback handshake.
func (s *Server) SetWSAPI(broker *events.Broker, snapshot wsSnapshotSource, token string) {
	s.ws = &wsServer{broker: broker, snapshot: snapshot, token: token, log: s.log}
}

// clientFrame is the client->server subscribe frame.
type clientFrame struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
	JobID string `json:"job_id,omitempty"`
}

// outFrame is a server->client frame. Payload is the raw event dict.
type outFrame struct {
	Type     string `json:"type"`
	Topic    string `json:"topic"`
	Seq      int64  `json:"seq"`
	Snapshot bool   `json:"snapshot,omitempty"`
	Drops    int64  `json:"drops,omitempty"`
	Payload  any    `json:"payload,omitempty"`
}

// handleWS upgrades the request to a WebSocket, authenticates, then serves
// the subscribe->snapshot->live stream for the connection's lifetime.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws := s.ws
	if ws == nil {
		http.NotFound(w, r) // unwired: indistinguishable from no route
		return
	}
	// Auth first (cheap, before upgrading). authorize is 3-valued: 200 =
	// allowed, 404 = the feature is not enabled for this peer (loopback-only
	// with no token configured — same disguise as unwired), 403 = a configured
	// token was wrong or missing.
	switch st := ws.authorize(r); st {
	case http.StatusOK:
	case http.StatusNotFound:
		http.NotFound(w, r)
		return
	default:
		http.Error(w, "unauthorized", st)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrade already wrote the HTTP response
	}

	// The connection wants exactly one subscribe frame as its first message.
	conn.SetReadLimit(4096)
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return
	}
	var sub clientFrame
	if err := conn.ReadJSON(&sub); err != nil || sub.Type != "subscribe" {
		_ = conn.Close()
		return
	}

	s.serveSubscribed(ws, conn, sub)
}

// serveSubscribed runs the snapshot-then-live stream for one authenticated,
// subscribed connection for the connection's lifetime. A client that
// disconnects (silently or not) cancels the loop via a read-pump, releasing the
// bus subscription — no leak on a vanished peer (#168 review).
func (s *Server) serveSubscribed(ws *wsServer, conn *websocket.Conn, sub clientFrame) {
	topic := sub.Topic
	switch topic {
	case "jobs", "outbox", "all":
	default:
		topic = "all" // invalid topic -> everything
	}

	// Context tied to both the connection (read-pump detects close) and the
	// writer (a stalled socket cancels it). When it is cancelled the live loop
	// returns and the deferred Unsubscribe runs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ReadPump: keeps gorilla processing ping/pong/close control frames and
	// cancels ctx the moment the peer is gone (a read error on a closed or
	// silent socket). This is the mechanism that reclaims the bus subscription
	// for a client that vanishes without another message.
	startReadPump(conn, cancel)

	// Pre-filtered bus subscription: pull the topic + job filter IN FRONT of
	// the bounded queue so irrelevant events never occupy (or overflow) it and
	// gap markers stay honest (#168 review).
	subs := events.NewSubscription().WithMatch(subscribeFilter(topic, sub.JobID))
	ws.broker.Subscribe(subs, wsQueue)
	defer ws.broker.Unsubscribe(subs)

	// WriterPump: writes queued frames and heartbeat pings to the socket. A
	// write that cannot finish in time closes the connection (reconnect
	// contract) and cancels the loop; it never blocks the bus or the live loop.
	out := make(chan []byte, wsQueue)
	startWriterPump(conn, ctx, cancel, out, wsPingPeriod)

	// send enqueues a frame to the writer. Non-blocking: a full send queue
	// means the socket cannot keep up — close (reconnect contract).
	send := func(f outFrame) bool {
		b, err := json.Marshal(f)
		if err != nil {
			return true
		}
		select {
		case out <- b:
			return true
		default:
			cancel() // socket too slow: tear down rather than grow memory
			return false
		}
	}

	// Acknowledge the subscription once the bus subscription is live: after
	// the subscribed ack any matching event is guaranteed to reach this
	// connection. No seq — it precedes the first snapshot seq.
	if b, err := json.Marshal(outFrame{Type: frameSubscribed, Topic: topic}); err == nil {
		select {
		case out <- b:
		default:
		}
	}

	// A single connection-local monotonic seq shared by snapshot and live.
	var seq atomic.Int64

	// Snapshot: in-flight jobs. Only a jobs-relevant subscription (jobs|all)
	// gets a job snapshot; snapshot frames always carry topic "jobs".
	if (topic == "jobs" || topic == "all") && ws.snapshot != nil {
		jobs, err := ws.snapshot.ActiveJobs(ctx)
		if err == nil {
			for _, j := range jobs {
				if sub.JobID != "" && sub.JobID != j.ID {
					continue
				}
				seq.Add(1)
				if !send(outFrame{
					Type: frameEvent, Topic: "jobs", Seq: seq.Load(),
					Snapshot: true, Payload: jobPayload(j),
				}) {
					return
				}
			}
		}
	}

	// Live: drain the (already filtered) bus subscription.
	done := ctx.Done()
	lastDrops := int64(0)
	for {
		e, drops, ok := subs.Next(done)
		if !ok {
			return
		}
		// Gap markers: drops the slow consumer missed, surfaced explicitly.
		// The subscription is pre-filtered, so drops count only relevant ones.
		if drops > lastDrops {
			seq.Add(int64(drops - lastDrops))
			if !send(outFrame{
				Type: frameGap, Topic: topic, Seq: seq.Load(),
				Drops: drops - lastDrops,
			}) {
				return
			}
			lastDrops = drops
		}
		if sub.JobID != "" && events.JobID(e) != sub.JobID {
			continue // defense in depth for the job filter
		}
		seq.Add(1)
		if !send(outFrame{
			Type: frameEvent, Topic: eventTopic(e), Seq: seq.Load(),
			Snapshot: false, Payload: eventPayload(e),
		}) {
			return
		}
	}
}

// startReadPump runs the read loop that (a) keeps gorilla processing control
// frames and (b) cancels cancel() on the first read error — i.e. as soon as
// the client is gone or its pong window lapses. It runs until ctx is done.
func startReadPump(conn *websocket.Conn, cancel context.CancelFunc) {
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(wsPongWait))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel() // client closed / silent peer reaped
				return
			}
		}
	}()
}

// startWriterPump writes frames from out and heartbeat pings. On the first
// write it cannot finish in time (or ctx is cancelled) it cancels cancel() and
// closes the connection. It returns when ctx is done or out is closed.
func startWriterPump(conn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, out <-chan []byte, pingPeriod time.Duration) {
	go func() {
		ping := time.NewTicker(pingPeriod)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case frame := <-out:
				if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
					cancel()
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
					cancel()
					return
				}
			case <-ping.C:
				if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
					cancel()
					return
				}
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					cancel()
					return
				}
			}
		}
	}()
}

// authorize returns the HTTP status for a handshake: 200 = allowed; 404 =
// the feature is not enabled for this peer (a non-loopback peer with no token
// configured — same disguise as unwired); 403 = a configured token was wrong
// or missing. Loopback peers are always allowed (the house rule).
func (ws *wsServer) authorize(r *http.Request) int {
	if loopbackPeer(r) {
		return http.StatusOK // loopback trusted; token not required
	}
	if ws.token == "" {
		// No WS secret configured: WS is loopback-only. A remote peer must not
		// be able to tell this apart from the route being absent.
		return http.StatusNotFound
	}
	if tokenFrom(r) != ws.token {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// loopbackPeer reports whether the request's remote address is loopback.
func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tokenFrom extracts the WS token from the ?token= query param or the
// Authorization: Bearer header (WS clients often cannot set headers on the
// upgrade; query-param is the interoperable primary).
func tokenFrom(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	bearer := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(bearer) > len(prefix) && bearer[:len(prefix)] == prefix {
		return bearer[len(prefix):]
	}
	return ""
}

// subscribeFilter returns the broker-side predicate narrowing a subscription
// to the events this connection asked for. nil = everything (unfiltered). The
// filter runs before enqueueing in the bounded queue, so irrelevant events
// cannot displace relevant ones (#168 review).
func subscribeFilter(topic, jobID string) func(events.Event) bool {
	if topic == "all" && jobID == "" {
		return nil
	}
	return func(e events.Event) bool {
		if jobID != "" && events.JobID(e) != jobID {
			return false
		}
		if topic == "all" {
			return true
		}
		return eventTopic(e) == topic
	}
}

// eventTopic maps a typed bus event to its WS topic.
func eventTopic(e events.Event) string {
	switch e.(type) {
	case events.OutboxDrained:
		return "outbox"
	default:
		return "jobs"
	}
}

// eventPayload renders a typed bus event as the JSON payload dict.
func eventPayload(e events.Event) map[string]any {
	switch v := e.(type) {
	case events.JobClaimed:
		return map[string]any{
			"kind": "job_claimed", "job_id": v.JobID, "worker_id": v.WorkerID,
			"runner_name": v.RunnerName, "attachment_filename": v.AttachmentFilename,
			"document_title": v.DocumentTitle,
		}
	case events.JobStageChanged:
		return map[string]any{
			"kind": "job_stage_changed", "job_id": v.JobID, "stage": v.Stage,
			"progress_hint": v.ProgressHint,
		}
	case events.JobCompleted:
		return map[string]any{
			"kind": "job_completed", "job_id": v.JobID, "took_ns": int64(v.Took),
		}
	case events.JobFailed:
		return map[string]any{
			"kind": "job_failed", "job_id": v.JobID, "error_code": v.ErrorCode,
		}
	case events.OutboxDrained:
		return map[string]any{"kind": "outbox_drained", "count": v.Count}
	default:
		return map[string]any{"kind": "unknown"}
	}
}

// jobPayload renders an in-flight repo.Job as the snapshot payload.
func jobPayload(j repo.Job) map[string]any {
	p := map[string]any{
		"job_id": j.ID, "status": j.Status, "attempt": j.Attempt,
		"max_attempts": j.MaxAttempts,
		"source_id":    j.SourceID,
		"document_id":  j.DocumentID,
	}
	if j.ErrorCode != nil && *j.ErrorCode != "" {
		p["error_code"] = *j.ErrorCode
	}
	return p
}
