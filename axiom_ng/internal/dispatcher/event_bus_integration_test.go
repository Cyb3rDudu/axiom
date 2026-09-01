package dispatcher

// #167 (Epic B · B1): the event bus is observed as a PASSENGER — a full
// simulated dispatch loop (claim -> stage x2 -> complete) publishes the
// expected typed event sequence, while the existing dispatcher behavior stays
// byte-identical. These tests attach a real Broker and read the subscriber.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
)

// TestEventSequenceClaimStageComplete drives one complete job and asserts the
// subscriber receives, in order: JobClaimed, two JobStageChanged deltas (one
// per distinct stage observed across polls), then JobCompleted.
func TestEventSequenceClaimStageComplete(t *testing.T) {
	h := openDispatchDB(t)
	h.truncateFixtures(t)
	jobID := h.seedJob(t, "EVT1", 3)

	fp := newFakeProcessor(t)
	fp.statuses = []string{"running", "running", "completed"}
	fp.stages = []string{"convert", "extract"} // distinct stage per in-progress poll
	fp.result = `{"contract_version":"1.0","job_id":"` + jobID + `","status":"completed"}`

	broker := events.NewBroker()
	sub := events.NewSubscription()
	broker.Subscribe(sub, 64)

	d := newDispatcher(t, h, fp, Config{RunnerName: "runner-evt1"})
	d.SetEventBroker(broker)
	runFor(t, d, context.Background(), 8*time.Second)

	// The simulated loop must end the job completed (passenger: behavior intact).
	if got := h.jobStatus(t, jobID); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}

	// Read the full event sequence the subscriber witnessed.
	var got []events.Event
	done := make(chan struct{})
	time.AfterFunc(3*time.Second, func() { close(done) }) // test clock
	for {
		e, _, ok := sub.Next(done)
		if !ok {
			break
		}
		got = append(got, e)
		if _, completed := e.(events.JobCompleted); completed {
			break
		}
	}

	// Expected: claim -> stage x2 -> complete.
	if len(got) != 4 {
		t.Fatalf("event count = %d (%v), want 4", len(got), describe(got))
	}
	claim, ok := got[0].(events.JobClaimed)
	if !ok {
		t.Fatalf("event[0] = %T, want JobClaimed", got[0])
	}
	if claim.JobID != jobID {
		t.Errorf("JobClaimed.JobID = %q, want %q", claim.JobID, jobID)
	}
	if claim.AttachmentFilename != "x.pdf" {
		t.Errorf("JobClaimed.AttachmentFilename = %q, want x.pdf", claim.AttachmentFilename)
	}
	if claim.DocumentTitle != "Test" {
		t.Errorf("JobClaimed.DocumentTitle = %q, want Test", claim.DocumentTitle)
	}
	if claim.RunnerName == "" {
		t.Errorf("JobClaimed.RunnerName empty")
	}

	st1, ok := got[1].(events.JobStageChanged)
	if !ok {
		t.Fatalf("event[1] = %T, want JobStageChanged", got[1])
	}
	if st1.Stage != "convert" {
		t.Errorf("event[1].Stage = %q, want convert", st1.Stage)
	}
	st2, ok := got[2].(events.JobStageChanged)
	if !ok {
		t.Fatalf("event[2] = %T, want JobStageChanged", got[2])
	}
	if st2.Stage != "extract" {
		t.Errorf("event[2].Stage = %q, want extract", st2.Stage)
	}
	comp, ok := got[3].(events.JobCompleted)
	if !ok {
		t.Fatalf("event[3] = %T, want JobCompleted", got[3])
	}
	if comp.JobID != jobID {
		t.Errorf("JobCompleted.JobID = %q, want %q", comp.JobID, jobID)
	}
}

// describe is a test-only compaction of a sequence for readable failures.
func describe(seq []events.Event) []string {
	out := make([]string, 0, len(seq))
	for _, e := range seq {
		out = append(out, strings.SplitN(e.(interface{ String() string }).String(), " ", 2)[0])
	}
	return out
}
