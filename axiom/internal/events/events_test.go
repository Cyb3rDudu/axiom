package events

import (
	"sync"
	"testing"
	"time"
)

// TestBrokerFanout verifies a published event reaches every subscriber.
func TestBrokerFanout(t *testing.T) {
	b := NewBroker()
	a, c := NewSubscription(), NewSubscription()
	b.Subscribe(a, 4)
	b.Subscribe(c, 4)
	b.Publish(JobClaimed{JobID: "j1", WorkerID: "w", RunnerName: "r"})

	for _, sub := range []*Subscription{a, c} {
		e, _, ok := sub.Next(nil)
		if !ok {
			t.Fatal("expected event")
		}
		if got := JobID(e); got != "j1" {
			t.Fatalf("JobID=%q, want j1 (%T)", got, e)
		}
	}
}

// TestBrokerSlowSubscriberGap is the gap proof from #167: a slow subscriber
// (artificially tiny queue) must receive a gap marker instead of the publisher
// blocking, and the dispatcher publish duration stays within a small cap.
func TestBrokerSlowSubscriberGap(t *testing.T) {
	b := NewBroker()
	slow := NewSubscription()
	b.Subscribe(slow, 2) // tiny: after 2 buffered, everything older is dropped

	// Publish more than the queue can hold while the subscriber never drains.
	// Each Publish must return promptly (non-blocking) regardless.
	start := time.Now()
	for i := range 10 {
		b.Publish(JobStageChanged{JobID: "j", Stage: stage(i)})
	}
	pubDur := time.Since(start)
	// Publish-duration cap: 10 bounds against a never-draining subscriber must
	// be sub-millisecond-class, well under a generous 100ms ceiling.
	if pubDur > 100*time.Millisecond {
		t.Fatalf("publish blocked: 10 publishes took %s (cap 100ms)", pubDur)
	}

	e, drops, ok := slow.Next(nil)
	if !ok {
		t.Fatal("expected an event")
	}
	if _, isStage := e.(JobStageChanged); !isStage {
		t.Fatalf("got %T, want JobStageChanged", e)
	}
	// 10 publishes, capacity 2: at least 8 were dropped before the first read.
	if drops < 8 {
		t.Fatalf("gap marker too small: got %d drops, want >= 8", drops)
	}
	// A second read sees a further drop (the gap keeps counting on overflow).
	if _, d2, ok := slow.Next(nil); ok && d2 < drops {
		t.Fatalf("gap marker went backwards: %d -> %d", drops, d2)
	}
}

func stage(i int) string { return "stage-" + string(rune('0'+i)) }

// TestBrokerNoSubscribersIsNoop verifies publishing into an empty broker is a
// cheap no-op (observer discipline: the bus never influences the emitter).
func TestBrokerNoSubscribersIsNoop(t *testing.T) {
	b := NewBroker()
	b.Publish(JobClaimed{JobID: "x"}) // must not panic, must not block
	b.Publish(nil)
}

// TestEventTypesStringAndJobID covers the typed events' Stringers and JobID.
func TestEventTypesStringAndJobID(t *testing.T) {
	cases := []Event{
		JobClaimed{JobID: "j", WorkerID: "w", RunnerName: "r", AttachmentFilename: "a.pdf", DocumentTitle: "Book"},
		JobStageChanged{JobID: "j", Stage: "convert", ProgressHint: "convert"},
		JobCompleted{JobID: "j", Took: time.Second},
		JobFailed{JobID: "j", ErrorCode: "PROCESS_FAILED"},
		OutboxDrained{Count: 3},
	}
	for _, e := range cases {
		_, outbox := e.(OutboxDrained)
		if JobID(e) == "" && !outbox {
			t.Errorf("%T: empty JobID", e)
		}
		if s := e.(interface{ String() string }).String(); s == "" {
			t.Errorf("%T: empty String()", e)
		}
	}
}

// TestBrokerFilteredSubscriptionIsolation is the #168 backpressure fix on B1:
// a subscription with a topic filter never enqueues (or drops/gaps on)
// events it did not ask for, so an irrelevant flood cannot displace the events
// it wants and cannot fabricate a false gap marker.
func TestBrokerFilteredSubscriptionIsolation(t *testing.T) {
	b := NewBroker()
	// A jobs-only subscription with a tiny queue: outbox events must never
	// occupy (or overflow) it, even under a heavy outbox flood.
	jobsOnly := NewSubscription().WithMatch(func(e Event) bool {
		_, isOutbox := e.(OutboxDrained)
		return !isOutbox
	})
	b.Subscribe(jobsOnly, 2)

	// Flood outbox events first (irrelevant to jobsOnly) — none may be
	// queued, so none may be dropped.
	for i := range 10 {
		b.Publish(OutboxDrained{Count: i})
	}
	if got := jobsOnly.Dropped(); got != 0 {
		t.Fatalf("filtered sub accumulated %d drops from irrelevant events, want 0", got)
	}

	// A matching job event must arrive with zero drops: no irrelevance was
	// cached in the queue, so there is nothing to be re-displaced.
	b.Publish(JobClaimed{JobID: "j"})
	e, drops, ok := jobsOnly.Next(nil)
	if !ok {
		t.Fatal("expected the matching job event")
	}
	if JobID(e) != "j" {
		t.Fatalf("got %v, want job j", JobID(e))
	}
	if drops != 0 {
		t.Fatalf("false gap from irrelevant flood: drops=%d, want 0", drops)
	}
}

// TestBrokerUnsubscribeStopsDelivery verifies unsubscribing removes a
// subscriber (and that a concurrent Publish doesn't race with it).
func TestBrokerUnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroker()
	sub := NewSubscription()
	b.Subscribe(sub, 4)
	b.Unsubscribe(sub)

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func(bk *Broker) { defer wg.Done(); bk.Publish(JobClaimed{JobID: "j"}) }(b)
	}
	wg.Wait()

	done := make(chan struct{})
	close(done)
	if e, _, ok := sub.Next(done); ok {
		t.Fatalf("received %T after unsubscribe", e)
	}
}

// TestNextPreservesCapacityAfterDrain is the interleaved-consumer regression
// (review Critical): a Next that re-slices with [1:len:len] pins cap==len, so a
// publish after a drain-to-empty takes the overflow branch against a zero
// buffer and panics. This is the normal B2/B3 consume pattern (drain, then
// more events arrive), so it must keep working.
func TestNextPreservesCapacityAfterDrain(t *testing.T) {
	b := NewBroker()
	sub := NewSubscription()
	b.Subscribe(sub, 8)

	b.Publish(JobClaimed{JobID: "j1"})
	e, _, ok := sub.Next(nil)
	if !ok || JobID(e) != "j1" {
		t.Fatalf("first Next: e=%v ok=%v, want j1/true", e, ok)
	}
	// Queue is now EMPTY with capacity still 8: the bug pinned cap==0 here
	// and the next offer panicked on copy(buf, buf[1:]).
	b.Publish(JobStageChanged{JobID: "j1", Stage: "convert"})
	e2, drops, ok := sub.Next(nil)
	if !ok {
		t.Fatal("second Next: no event after drain")
	}
	if st, isStage := e2.(JobStageChanged); !isStage || st.Stage != "convert" {
		t.Fatalf("second Next: e=%T %v, want JobStageChanged convert", e2, e2)
	}
	if drops != 0 {
		t.Fatalf("false gap after drain: drops=%d, want 0", drops)
	}
}

// TestNextKeepsCapacityWhileInterleaving proves no false gap markers for a
// subscriber that keeps up while publishes interleave with reads (the same
// cap==len ratchet produced phantom drops before the fix).
func TestNextKeepsCapacityWhileInterleaving(t *testing.T) {
	b := NewBroker()
	sub := NewSubscription()
	b.Subscribe(sub, 8)

	b.Publish(JobClaimed{JobID: "a"})
	b.Publish(JobStageChanged{JobID: "a", Stage: "s1"})
	if e, drops, ok := sub.Next(nil); !ok || drops != 0 || JobID(e) != "a" {
		t.Fatalf("first read: e=%v drops=%d ok=%v", e, drops, ok)
	}
	b.Publish(JobStageChanged{JobID: "a", Stage: "s2"}) // queue: s1,s2
	if e, drops, ok := sub.Next(nil); !ok || drops != 0 {
		t.Fatalf("second read: e=%v drops=%d ok=%v", e, drops, ok)
	} else if st, isStage := e.(JobStageChanged); !isStage || st.Stage != "s1" {
		t.Fatalf("FIFO broken: e=%T %v, want s1", e, e)
	}
	// Remaining queue must be exactly [s2] with zero drops.
	if e, drops, ok := sub.Next(nil); !ok || drops != 0 {
		t.Fatalf("third read: e=%v drops=%d ok=%v", e, drops, ok)
	} else if st, isStage := e.(JobStageChanged); !isStage || st.Stage != "s2" {
		t.Fatalf("FIFO broken: e=%T %v, want s2", e, e)
	}
	if got := sub.Dropped(); got != 0 {
		t.Fatalf("Dropped()=%d, want 0 for a subscriber that keeps up", got)
	}
}
