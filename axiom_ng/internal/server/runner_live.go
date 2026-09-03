// Runner live view (Epic B · B3, #169): "which runner is chunking what right
// now — and how far along?" A server-side state deriver subscribes to the B1
// bus, folds the job lifecycle events into per-JOB state and derives the
// per-runner view from it, re-publishing each change as
// events.RunnerStateChanged on the same bus — so the view flows through the
// EXISTING #168 machinery (WS topic `runners` with the WithMatch pre-filter,
// backpressure and gap semantics inherited) and the REST snapshot
// (/api/runners/live) serializes the very same struct.
//
// Derivation rules (all session-scoped, no persistence):
//   - The dispatcher allows MULTIPLE parallel workers per runner
//     (Concurrency > 1), so state is tracked PER JOB and the runner view is
//     derived: busy while any job is active, displaying the most recently
//     claimed active one (#169 review — a single slot per runner corrupted
//     the view under real parallel load).
//   - JobClaimed     -> the job joins the runner's active set; the view shows
//     it (most recent claim wins the display).
//   - JobStageChanged-> stage + progress hint of THAT job (the hint is the
//     stage name today; the N/M counters land with #236 and flow through
//     unchanged); the view is published when the displayed job changes.
//   - JobCompleted   -> the job leaves the active set; completed counter +1
//     and the idle tail is updated. If other jobs remain active the runner
//     stays busy (showing the next most recent); only the LAST active job
//     ending flips the view to idle.
//   - JobFailed      -> the same, without bumping the counter (the counter is
//     jobs-completed, not jobs-ended).
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

// runnerJob is one active job on a runner.
type runnerJob struct {
	jobID     string
	title     string
	filename  string
	stage     string
	hint      string
	workerID  string
	claimedMs int64 // claim order = slice order; the tail is the most recent
}

// tailInfo is the idle tail: the last job that ended on a runner.
type tailInfo struct {
	jobID string
	title string
	atMs  int64
}

// RunnerLive derives and serves the per-runner live state.
type RunnerLive struct {
	broker *events.Broker
	log    *log.Logger

	mu        sync.Mutex
	active    map[string][]*runnerJob // runner -> active jobs, claim order
	byJob     map[string]string       // job_id -> runner_name
	completed map[string]int64        // runner -> session counter
	tail      map[string]tailInfo     // runner -> last ended job
	gpu       map[string]string       // runner -> GPU label from the name stamp
	known     map[string]bool         // runners ever seen (Snapshot iteration)
	ready     chan struct{}           // closed once the bus subscription is live
}

// NewRunnerLive builds the deriver. Start subscribes it to the bus.
func NewRunnerLive(broker *events.Broker, log *log.Logger) *RunnerLive {
	return &RunnerLive{
		broker:    broker,
		log:       log,
		active:    make(map[string][]*runnerJob),
		byJob:     make(map[string]string),
		completed: make(map[string]int64),
		tail:      make(map[string]tailInfo),
		gpu:       make(map[string]string),
		known:     make(map[string]bool),
		ready:     make(chan struct{}),
	}
}

// WaitReady blocks until the deriver's bus subscription is live — callers that
// immediately publish (main before the dispatcher starts, tests) cannot lose
// the first events to the startup race.
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

// derive folds one bus event into the per-job state and returns the derived
// runner view to publish (nil when nothing visible changed).
func (v *RunnerLive) derive(e events.Event) *events.RunnerStateChanged {
	v.mu.Lock()
	defer v.mu.Unlock()

	switch ev := e.(type) {
	case events.JobClaimed:
		runner := ev.RunnerName
		v.known[runner] = true
		v.gpu[runner] = GPULabel(runner)
		v.active[runner] = append(v.active[runner], &runnerJob{
			jobID:     ev.JobID,
			title:     ev.DocumentTitle,
			filename:  ev.AttachmentFilename,
			workerID:  ev.WorkerID,
			claimedMs: time.Now().UnixMilli(),
		})
		v.byJob[ev.JobID] = runner
		st := v.view(runner)
		return &st

	case events.JobStageChanged:
		runner, ok := v.byJob[ev.JobID]
		if !ok {
			return nil
		}
		j := v.findJob(runner, ev.JobID)
		if j == nil {
			return nil
		}
		j.stage = ev.Stage
		j.hint = ev.ProgressHint
		// Publish only when the DISPLAYED job changed — a stage delta on a
		// background job does not move the runner view.
		jobs := v.active[runner]
		if jobs[len(jobs)-1].jobID != ev.JobID {
			return nil
		}
		st := v.view(runner)
		return &st

	case events.JobCompleted:
		return v.finish(true, ev.JobID)

	case events.JobFailed:
		return v.finish(false, ev.JobID)
	}
	return nil
}

// findJob locates an active job on a runner by id.
func (v *RunnerLive) findJob(runner, jobID string) *runnerJob {
	for _, j := range v.active[runner] {
		if j.jobID == jobID {
			return j
		}
	}
	return nil
}

// finish removes the ended job from its runner's active set, records the idle
// tail and — only when a COMPLETED job — bumps the session counter. The
// derived view always changes (counter/tail), so it is always published: if
// other jobs remain active the runner stays busy (displaying the most recent
// of them); the last one ending flips it to idle.
func (v *RunnerLive) finish(completed bool, jobID string) *events.RunnerStateChanged {
	runner, ok := v.byJob[jobID]
	if !ok {
		return nil
	}
	j := v.findJob(runner, jobID)
	if j == nil {
		return nil
	}
	jobs := v.active[runner]
	for i, x := range jobs {
		if x.jobID == jobID {
			v.active[runner] = append(jobs[:i], jobs[i+1:]...)
			break
		}
	}
	delete(v.byJob, jobID)
	if completed {
		v.completed[runner]++
	}
	v.tail[runner] = tailInfo{jobID: j.jobID, title: j.title, atMs: time.Now().UnixMilli()}
	st := v.view(runner)
	return &st
}

// view derives the runner-level state from the per-job model: busy while any
// job is active (displaying the most recently claimed one), idle otherwise
// with the last-ended-job tail. Counters and tail persist across states.
func (v *RunnerLive) view(runner string) events.RunnerStateChanged {
	st := events.RunnerStateChanged{
		RunnerName:    runner,
		GPU:           v.gpu[runner],
		JobsCompleted: v.completed[runner],
	}
	if t := v.tail[runner]; t.jobID != "" {
		st.LastJobID = t.jobID
		st.LastTitle = t.title
		st.LastEndedAtMs = t.atMs
	}
	jobs := v.active[runner]
	if len(jobs) == 0 {
		st.State = "idle"
		return st
	}
	cur := jobs[len(jobs)-1]
	st.State = "busy"
	st.WorkerID = cur.workerID
	st.JobID = cur.jobID
	st.DocumentTitle = cur.title
	st.AttachmentFilename = cur.filename
	st.Stage = cur.stage
	st.ProgressHint = cur.hint
	return st
}

// Snapshot returns the current per-runner states for the REST endpoint and
// the WS runners-topic snapshot (structurally identical by construction).
func (v *RunnerLive) Snapshot() []events.RunnerStateChanged {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]events.RunnerStateChanged, 0, len(v.known))
	for runner := range v.known {
		out = append(out, v.view(runner))
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
