// Runner live view (Epic B · B3, #169): "which runner is chunking what right
// now — and how far along?" A server-side state deriver subscribes to the B1
// bus, folds the job lifecycle events into a per-runner state and re-publishes
// each change as events.RunnerStateChanged on the same bus — so the view flows
// through the EXISTING #168 machinery (WS topic `runners` with the WithMatch
// pre-filter, backpressure and gap semantics inherited) and the REST snapshot
// (/api/runners/live) serializes the very same struct.
//
// Derivation rules (all session-scoped, no persistence):
//   - JobClaimed     -> runner becomes busy: current book/title/filename,
//     worker_id, stage empty until the first poll delta. jobID->runner is
//     remembered so later job-only events resolve to their runner.
//   - JobStageChanged-> stage + progress hint of the runner's current job
//     (the hint is the stage name today; the N/M counters land with #236 and
//     flow through unchanged).
//   - JobCompleted   -> busy -> idle: completed counter +1, the finished book
//     becomes the idle tail (last job/when).
//   - JobFailed      -> busy -> idle the same way (the tail names the failure).
//
// GPU assignment comes from the runner_name→config stamp (the #5c identity):
// the operator names runners after their hardware (e.g. "carrier-gpu0"), so
// the assignment is CONFIGURATION, never live nvidia-smi metrics (non-goal).
package server

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
)

// RunnerLive derives and serves the per-runner live state.
type RunnerLive struct {
	broker *events.Broker
	log    *log.Logger

	mu        sync.Mutex
	states    map[string]*events.RunnerStateChanged // runner_name -> state
	byJob     map[string]string                     // job_id -> runner_name
	completed map[string]int64                      // runner_name -> session counter
	last      map[string]events.RunnerStateChanged  // runner_name -> idle tail
	ready     chan struct{}                         // closed once the bus subscription is live
}

// NewRunnerLive builds the deriver. Start subscribes it to the bus.
func NewRunnerLive(broker *events.Broker, log *log.Logger) *RunnerLive {
	return &RunnerLive{
		broker:    broker,
		log:       log,
		states:    make(map[string]*events.RunnerStateChanged),
		byJob:     make(map[string]string),
		completed: make(map[string]int64),
		last:      make(map[string]events.RunnerStateChanged),
		ready:     make(chan struct{}),
	}
}

// WaitReady blocks until the deriver's bus subscription is live — callers that
// immediately publish (tests) cannot lose the first events to the startup race.
func (v *RunnerLive) WaitReady() { <-v.ready }

// Start subscribes to the bus and runs the derivation loop. It blocks until
// done is closed; run it on its own goroutine. The subscription is
// pre-filtered to JOB events only — the derived RunnerStateChanged events it
// publishes cannot loop back into the deriver.
func (v *RunnerLive) Start(done <-chan struct{}) {
	sub := events.NewSubscription().WithMatch(jobEventsOnly)
	v.broker.Subscribe(sub, 64)
	close(v.ready)
	defer v.broker.Unsubscribe(sub)
	for {
		e, _, ok := sub.Next(done)
		if !ok {
			return
		}
		if changed := v.derive(e); changed != nil {
			v.broker.Publish(*changed)
		}
	}
}

// jobEventsOnly is the deriver's bus filter: the job lifecycle it folds. The
// derived RunnerStateChanged events are excluded — no feedback loop.
func jobEventsOnly(e events.Event) bool {
	switch e.(type) {
	case events.JobClaimed, events.JobStageChanged, events.JobCompleted, events.JobFailed:
		return true
	}
	return false
}

// derive folds one bus event into the runner states and returns the changed
// state to publish (nil when the event maps to no known runner).
func (v *RunnerLive) derive(e events.Event) *events.RunnerStateChanged {
	v.mu.Lock()
	defer v.mu.Unlock()

	switch ev := e.(type) {
	case events.JobClaimed:
		st := &events.RunnerStateChanged{
			RunnerName:         ev.RunnerName,
			GPU:                GPULabel(ev.RunnerName),
			WorkerID:           ev.WorkerID,
			State:              "busy",
			JobID:              ev.JobID,
			DocumentTitle:      ev.DocumentTitle,
			AttachmentFilename: ev.AttachmentFilename,
			JobsCompleted:      v.completed[ev.RunnerName],
		}
		v.states[ev.RunnerName] = st
		v.byJob[ev.JobID] = ev.RunnerName
		return st

	case events.JobStageChanged:
		runner, ok := v.byJob[ev.JobID]
		if !ok {
			return nil
		}
		st := v.states[runner]
		if st == nil || st.State != "busy" || st.JobID != ev.JobID {
			return nil
		}
		st.Stage = ev.Stage
		st.ProgressHint = ev.ProgressHint
		return st

	case events.JobCompleted:
		return v.finish(true, ev.JobID)

	case events.JobFailed:
		return v.finish(false, ev.JobID)
	}
	return nil
}

// finish moves a runner from busy to idle. Only a COMPLETED job bumps the
// session counter; a failed job transitions to idle with its tail but does
// not count as completed (the counter is jobs-completed, not jobs-ended).
func (v *RunnerLive) finish(completed bool, jobID string) *events.RunnerStateChanged {
	runner, ok := v.byJob[jobID]
	if !ok {
		return nil
	}
	st := v.states[runner]
	if st == nil || st.State != "busy" {
		return nil
	}
	if completed {
		v.completed[runner]++
	}
	v.last[runner] = *st
	st.JobsCompleted = v.completed[runner]
	st.State = "idle"
	st.Stage = ""
	st.ProgressHint = ""
	st.LastJobID = st.JobID
	st.LastTitle = st.DocumentTitle
	st.LastEndedAtMs = time.Now().UnixMilli()
	st.JobID = ""
	st.DocumentTitle = ""
	st.AttachmentFilename = ""
	return st
}

// Snapshot returns the current per-runner states for the REST endpoint and
// the WS runners-topic snapshot (structurally identical by construction).
func (v *RunnerLive) Snapshot() []events.RunnerStateChanged {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]events.RunnerStateChanged, 0, len(v.states))
	for _, st := range v.states {
		out = append(out, *st)
	}
	return out
}

// GPULabel derives the GPU assignment from the runner name — the #5c identity
// stamp: operators name runners after their hardware ("carrier-gpu0"), so the
// assignment is configuration, not measurement. Returns "" when the name
// carries no gpu token.
func GPULabel(runnerName string) string {
	for _, part := range strings.FieldsFunc(runnerName, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	}) {
		if len(part) > 3 && strings.EqualFold(part[:3], "gpu") {
			return part
		}
	}
	return ""
}
