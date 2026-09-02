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
// snapshot:true and arrive FIRST (the DB's in-flight jobs); live frames carry
// snapshot:false. Snapshot and live share the same seq stream, so a client
// sees gapless, duplicate-free ordering.
//
// Backpressure follows the B1 pattern end to end:
//   - the bus subscription is a bounded queue; a consumer that drains slowly
//     sees its lost events surfaced as `gap` frames (the subscription's drops).
//   - the final hop to the socket is a bounded send queue with a write
//     deadline; a connection that falls behind (producer beats the TCP socket)
//     is CLOSED (the reconnect contract), never left to leak memory or block
//     the bus or the dispatcher.
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
	token    string // non-empty => required on the WS handshake (header/query)
	log      *log.Logger
}

// wsConnBound bounds both the per-connection bus subscription and the send
// queue to the socket. Small enough that a slow client degrades to gap/close
// instead of unbounded memory.
const wsQueue = 64

var wsUpgrader = websocket.Upgrader{
	// The API is not cross-origin browser-embedded; allow any origin. Auth is
	// enforced by the token gate below, not by Origin (Co-).

	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsEvent is the event frame type-discriminator.
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
// WS secret: when non-empty it is required on a non-loopback handshake.
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
		// Unwired: 404, indistinguishable from no route (sourceSecret pattern).
		http.NotFound(w, r)
		return
	}
	// Auth first (cheap, before the upgrade): a configured token is required
	// on a non-loopback handshake; on loopback the peer is trusted (house rule).
	if !ws.tokenAllows(r) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the HTTP response; nothing more to send.
		return
	}

	// Read the subscribe frame. A client must subscribe exactly once as the
	// first message.
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
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return
	}

	s.serveSubscribed(ws, conn, sub)
}

// serveSubscribed runs the snapshot-then-live loop for one authenticated,
// subscribed connection. It blocks for the connection's lifetime.
func (s *Server) serveSubscribed(ws *wsServer, conn *websocket.Conn, sub clientFrame) {
	// Topic filter: which events this connection wants.
	topic := sub.Topic
	switch topic {
	case "jobs", "outbox", "all":
	default:
		topic = "all"
	}

	// Subscribe to the bus BEFORE reading the snapshot so no live event that
	// fires mid-snapshot is lost: it lands in the bounded subscription queue
	// and is delivered after the snapshot (gapless by construction).
	subs := events.NewSubscription()
	ws.broker.Subscribe(subs, wsQueue)
	defer ws.broker.Unsubscribe(subs)

	// WriterPump: consume the bounded send queue and write to the socket with a
	// deadline. A full queue or a timed-out write closes the connection (the
	// reconnect contract) — the producer never blocks on a slow socket.
	out := make(chan []byte, wsQueue)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for frame := range out {
			if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				_ = conn.Close()
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				_ = conn.Close()
				return
			}
		}
		_ = conn.Close()
	}()

	// A single connection-local monotonic seq shared by snapshot and live frames.
	var seq atomic.Int64

	// send enqueues a frame to the writerPump. Non-blocking: a full send queue
	// means the socket cannot keep up — close (reconnect contract), never block.
	send := func(f outFrame) bool {
		b, err := json.Marshal(f)
		if err != nil {
			return true // serialization failure of a frame we control: skip
		}
		select {
		case out <- b:
			return true
		default:
			return false
		}
	}

	// Acknowledge the subscription to the client ONCE the bus subscription is
	// live: after the ack, any published event the topic/job filter matches is
	// guaranteed to reach this connection (the client uses it as a barrier).
	// It carries no seq — a control frame, before the snapshot's first seq.
	if b, err := json.Marshal(outFrame{Type: frameSubscribed, Topic: topic}); err == nil {
		select {
		case out <- b:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(out) // stop the writerPump

	// --- snapshot: the DB's in-flight jobs -------------------------------
	if ws.snapshot != nil {
		jobs, err := ws.snapshot.ActiveJobs(ctx)
		if err == nil {
			for _, j := range jobs {
				if sub.JobID != "" && sub.JobID != j.ID {
					continue // job-filtered
				}
				seq.Add(1)
				if !send(outFrame{
					Type: frameEvent, Topic: topic, Seq: seq.Load(),
					Snapshot: true, Payload: jobPayload(j),
				}) {
					return
				}
			}
		}
	}

	// --- live: drain the bus subscription --------------------------------
	done := ctx.Done()
	lastDrops := int64(0)
	for {
		e, drops, ok := subs.Next(done)
		if !ok {
			return // context cancelled / connection gone
		}
		// Gap markers from the bus's bounded subscription (B1 pattern): any
		// events the slow consumer missed are surfaced as an explicit gap.
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

		evTopic := eventTopic(e)
		if topic != "all" && evTopic != topic {
			continue // topic filter
		}
		if sub.JobID != "" && events.JobID(e) != sub.JobID {
			continue // job filter
		}
		seq.Add(1)
		if !send(outFrame{
			Type: frameEvent, Topic: evTopic, Seq: seq.Load(),
			Snapshot: false, Payload: eventPayload(e),
		}) {
			return
		}
	}
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

// tokenAllows reports whether a handshake is authorized: a configured token
// is required on a non-loopback peer; on loopback the peer is trusted (the
// house rule — loopback ops are unchanged). Token is read from ?token= or the
// Authorization: Bearer header.
func (ws *wsServer) tokenAllows(r *http.Request) bool {
	if ws.token == "" || loopbackPeer(r) {
		return true
	}
	return tokenFrom(r) == ws.token
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
