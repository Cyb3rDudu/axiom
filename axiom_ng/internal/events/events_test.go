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
