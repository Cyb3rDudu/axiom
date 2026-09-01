// Package events defines the dispatcher's typed, observer-only event bus
// (Epic B · B1, #167). The dispatcher publishes a running job's lifecycle as
// typed events; subscribers (B2 WebSocket, B3 live view) consume them. The
// bus is purely additive: no dispatcher behavior depends on it, and publish
// never blocks the dispatcher path.
package events

import (
	"fmt"
	"sync"
	"time"
)

// Event is the marker interface implemented by every typed bus event.
type Event interface {
	event()
}

// --- Typed events -----------------------------------------------------------

// JobClaimed is published when a dispatcher claims a job for a runner. Fields
// carry the book/runner identity B3 needs to render a live row.
type JobClaimed struct {
	JobID              string
	WorkerID           string
	RunnerName         string
	AttachmentFilename string
	DocumentTitle      string
}

func (e JobClaimed) event() {}

// JobStageChanged is published when polling /v1/jobs/{id} observes a stage
// transition (poll-delta; the runner's own progress protocol is #236).
// ProgressHint is a lighter hint (e.g. the stage name), not a progress contract.
type JobStageChanged struct {
	JobID        string
	Stage        string
	ProgressHint string
}

func (e JobStageChanged) event() {}

// JobCompleted is published when a job reaches a durable completed state.
// Took is the wall time from claim to completion.
type JobCompleted struct {
	JobID string
	Took  time.Duration
}

func (e JobCompleted) event() {}

// JobFailed is published when a job reaches a terminal failed state.
type JobFailed struct {
	JobID     string
	ErrorCode string
}

func (e JobFailed) event() {}

// OutboxDrained is published after an outbox drain batch processes rows.
type OutboxDrained struct {
	Count int
}

func (e OutboxDrained) event() {}

// Strings render one line each for debugging / logs.
func (e JobClaimed) String() string {
	return fmt.Sprintf("job_claimed job=%s worker=%s runner=%s attachment=%s title=%s",
		e.JobID, e.WorkerID, e.RunnerName, e.AttachmentFilename, e.DocumentTitle)
}

func (e JobStageChanged) String() string {
	return fmt.Sprintf("job_stage_changed job=%s stage=%s hint=%s", e.JobID, e.Stage, e.ProgressHint)
}

func (e JobCompleted) String() string {
	return fmt.Sprintf("job_completed job=%s took=%s", e.JobID, e.Took)
}

func (e JobFailed) String() string {
	return fmt.Sprintf("job_failed job=%s code=%s", e.JobID, e.ErrorCode)
}

func (e OutboxDrained) String() string {
	return fmt.Sprintf("outbox_drained count=%d", e.Count)
}

// JobID returns the affected job id ("" for events without one).
func JobID(e Event) string {
	switch v := e.(type) {
	case JobClaimed:
		return v.JobID
	case JobStageChanged:
		return v.JobID
	case JobCompleted:
		return v.JobID
	case JobFailed:
		return v.JobID
	default:
		return ""
	}
}

// --- Broker ---------------------------------------------------------------

// defaultQueueSize bounds each subscriber's queue before the oldest event is
// dropped and a gap marker is set. Small so a slow subscriber degrades to gap
// markers instead of unbounded memory; large enough for B3's live view to keep
// a couple of stage frames between drains.
const defaultQueueSize = 64

// Subscription is a bounded, non-blocking queue of events for one subscriber.
// It is consumed by Next and is owned by a single consumer goroutine.
type Subscription struct {
	mu    sync.Mutex
	buf   []Event // FIFO queue
	drops int64   // total events dropped by overflow (the gap marker)
	sig   chan struct{}
}

// Broker is the in-process pub/sub hub. Subscribe before the emitter runs.
// Publish is non-blocking: a subscriber that falls behind drops its oldest
// events (counting a gap marker) rather than ever blocking the dispatcher.
type Broker struct {
	mu   sync.Mutex
	subs map[*Subscription]int // subscription -> queue capacity
}

// NewBroker returns a broker that gives each subscription defaultQueueSize.
func NewBroker() *Broker {
	return &Broker{subs: make(map[*Subscription]int)}
}

// NewSubscription returns a subscription not yet attached to any broker.
func NewSubscription() *Subscription {
	return &Subscription{sig: make(chan struct{}, 1)}
}

// Subscribe registers sub with a bounded queue of the given capacity (0 uses
// the package default). Call once per Subscription. Existing queued events are
// preserved up to capacity.
func (b *Broker) Subscribe(sub *Subscription, capacity int) {
	if capacity <= 0 {
		capacity = defaultQueueSize
	}
	if sub == nil {
		return
	}
	sub.mu.Lock()
	if sub.buf == nil {
		sub.buf = make([]Event, 0, capacity)
	}
	sub.mu.Unlock()
	b.mu.Lock()
	b.subs[sub] = capacity
	b.mu.Unlock()
}

// Unsubscribe removes a subscriber; its buffered events are dropped.
func (b *Broker) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	delete(b.subs, sub)
	b.mu.Unlock()
}

// Publish delivers e to every subscriber. It never blocks: for each subscriber
// it appends to a bounded queue, dropping the oldest event (and counting a gap)
// when full. An event that arrives with no subscribers is a no-op.
func (b *Broker) Publish(e Event) {
	if e == nil {
		return
	}
	b.mu.Lock()
	for sub := range b.subs {
		sub.offer(e)
	}
	b.mu.Unlock()
}

// offer appends e, dropping the oldest event on overflow and bumping the gap
// counter. Non-blocking: bounded work only, no waiting on the consumer.
func (s *Subscription) offer(e Event) {
	s.mu.Lock()
	if len(s.buf) < cap(s.buf) {
		s.buf = append(s.buf, e)
	} else {
		// Full: drop the oldest, keep the newest, count the loss.
		copy(s.buf, s.buf[1:])
		s.buf[len(s.buf)-1] = e
		s.drops++
	}
	// Wake the consumer (best-effort; the channel is size 1 so a pending
	// wake coalesces instead of blocking).
	select {
	case s.sig <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}

// Next blocks until an event is available. It returns the event plus the
// subscription's cumulative dropped count — a subscriber compares it against
// its last-seen value to SEE how many events were skipped while it was slow
// (the gap marker). ok is false when ctx is closed or the subscription has no
// producer and its queue is empty and no gap is pending.
func (s *Subscription) Next(done <-chan struct{}) (e Event, drops int64, ok bool) {
	for {
		s.mu.Lock()
		if len(s.buf) > 0 {
			e = s.buf[0]
			s.buf = s.buf[1:len(s.buf):len(s.buf)]
			drops = s.drops
			s.mu.Unlock()
			return e, drops, true
		}
		s.mu.Unlock()

		select {
		case <-done:
			return nil, 0, false
		case <-s.sig:
		}
	}
}

// Dropped returns the cumulative number of events dropped for this subscriber
// (the running gap marker). Safe to call outside Next for a live count.
func (s *Subscription) Dropped() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drops
}
