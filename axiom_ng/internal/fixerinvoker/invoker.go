// Package fixerinvoker — #206: the mail-ingest-side fixer caller.
//
// Owner contract: the fixer is an EVENT RUNNER (no launchd, no KeepAlive);
// this invoker is its only systematic caller. It polls the repair_cases
// queue (status queued), claims one case at a time (bounded concurrency),
// invokes the fixer wrapper (scripts/fix.sh / /opt/axiom/bin/axiom-fixer)
// ONCE per attachment key, and drives the case through the existing #184
// state machine:
//
//	queued →(claim)→ in_repair →(fix.sh exit 0 + healed pdf)→ healed
//	                              →(non-zero/timeout)→ queued (retry) | failed
//
// Crash-safety (no new crash-loop class, owner nail 5):
//   - the invoker NEVER dies on a hung fixer: every invocation runs under
//     its own context timeout (a backstop ABOVE fix.sh's built-in 30-min
//     kill, so the lockdir normally does the reaping) and a failing or
//     panicking case is logged and failed/requeued, never propagated;
//   - a dead invoker loses no case: the queue lives in the DB; stale
//     in_repair cases (claim older than the runtime window) are requeued
//     on the next start — the dispatcher lease-recovery pattern. The
//     per-attachment loop guard still caps total attempts.
//
// The per-key lockdir inside fix.sh additionally serializes against
// NON-invoker concurrency (an operator running a manual repair): such an
// invocation exits 3 and is treated as an ordinary retryable failure.
package fixerinvoker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repair"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
	"github.com/jackc/pgx/v5"
)

// Config bounds one invoker run.
type Config struct {
	// Command is the fixer wrapper invoked as: Command <key> --apply.
	// Default "/opt/axiom/bin/axiom-fixer" (same wrapper as scripts/fix.sh).
	Command string
	// WorkRoot is the fixer's WORK_ROOT: the healed pdf of a key is read
	// from WorkRoot/<key>/work.pdf after a successful invocation.
	// Default ~/.local/state/axiom/runs (fixer default).
	WorkRoot string
	// Interval is the queue poll interval. Default 30s.
	Interval time.Duration
	// Timeout is the per-invocation backstop. It must sit ABOVE fix.sh's
	// own 30-min timeout so the wrapper (lockdir + timeout binary) does
	// the primary killing; this context only catches a wedged wrapper.
	// Default 35m.
	Timeout time.Duration
	// Concurrency caps parallel fixer invocations per host (owner nail 3:
	// max 1-2). Values below 1 clamp to 1, above 2 clamp to 2.
	Concurrency int
	// StaleAfter bounds how long an in_repair claim may sit before the
	// reaper requeues it (dead-invoker recovery). Default 40m (Timeout
	// plus slack so a live, merely slow invocation is never requeued
	// under a second invoker).
	StaleAfter time.Duration
}

func (c *Config) fillDefaults() {
	if c.Command == "" {
		c.Command = "/opt/axiom/bin/axiom-fixer"
	}
	if c.WorkRoot == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.WorkRoot = filepath.Join(home, ".local", "state", "axiom", "runs")
		} else {
			c.WorkRoot = "."
		}
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 35 * time.Minute
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.Concurrency > 2 {
		c.Concurrency = 2
	}
	if c.StaleAfter <= c.Timeout {
		// structural invariant: a live-but-slow invocation must never be
		// requeued under a second claim while it is still running
		c.StaleAfter = c.Timeout + 5*time.Minute
	}
}

// Deps are the invoker's outward effects. Apply is the shared custody
// sequence (repair.ApplyDeps) so tests can fake the Zotero writes.
type Deps struct {
	Rep            *repo.Repo
	Apply          repair.ApplyDeps
	QuarantineRoot string
}

// Invoker drives the repair queue.
type Invoker struct {
	cfg    Config
	deps   Deps
	logger *log.Logger
	sem    chan struct{}
}

// New builds an invoker (cfg defaults are filled here).
func New(cfg Config, deps Deps, logger *log.Logger) *Invoker {
	cfg.fillDefaults()
	if logger == nil {
		logger = log.New(os.Stderr, "fixer-invoker ", log.LstdFlags)
	}
	return &Invoker{cfg: cfg, deps: deps, logger: logger, sem: make(chan struct{}, cfg.Concurrency)}
}

// Run polls the queue until ctx is done. It only returns on ctx
// cancellation — every per-case failure is handled, never propagated
// (owner nail 5: no crash-loop class).
func (inv *Invoker) Run(ctx context.Context) error {
	inv.logger.Printf("fixer invoker starting: cmd=%s interval=%s timeout=%s concurrency=%d workroot=%s",
		inv.cfg.Command, inv.cfg.Interval, inv.cfg.Timeout, inv.cfg.Concurrency, inv.cfg.WorkRoot)
	// Lease recovery FIRST (a previous invoker may have died mid-case).
	inv.reapStale(ctx)

	t := time.NewTicker(inv.cfg.Interval)
	defer t.Stop()
	for {
		// reap EVERY tick (one UPDATE): in-process orphans (a DB error after
		// claim, a recovered panic) must not sit in in_repair until the next
		// process restart — the reaper is the single recovery net.
		inv.reapStale(ctx)
		inv.pollOnce(ctx)
		select {
		case <-ctx.Done():
			inv.logger.Printf("fixer invoker stopped")
			return nil
		case <-t.C:
		}
	}
}

func (inv *Invoker) reapStale(ctx context.Context) {
	n, err := inv.deps.Rep.RequeueStaleRepairCases(ctx, inv.cfg.StaleAfter)
	if err != nil {
		inv.logger.Printf("requeue-stale: %v", err)
		return
	}
	if n > 0 {
		inv.logger.Printf("requeued %d stale in_repair case(s) (dead invoker recovery)", n)
	}
}

func (inv *Invoker) pollOnce(ctx context.Context) {
	cases, err := inv.deps.Rep.ListRepairQueue(ctx)
	if err != nil {
		inv.logger.Printf("queue: %v", err)
		return
	}
	for i := range cases {
		c := cases[i]
		select {
		case inv.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func() {
			defer func() { <-inv.sem }()
			defer func() {
				if r := recover(); r != nil {
					inv.logger.Printf("case %s: panic recovered: %v", c.ID, r)
				}
			}()
			inv.processCase(ctx, c.ID)
		}()
	}
}

// processCase takes ONE case item→claim → fixer run → status transition.
// The item resolves BEFORE the claim: attachment-gone blocks a case while
// it is still queued (BlockRepairCase refuses in_repair by design nail —
// a mid-flight case is never touched from outside).
func (inv *Invoker) processCase(ctx context.Context, caseID string) {
	item, err := inv.deps.Rep.RepairCaseItem(ctx, caseID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// attachment/document gone at the source — park, don't re-serve
			// forever (W3a rule mirrored from the queue listing)
			if berr := inv.deps.Rep.BlockRepairCase(ctx, caseID, "attachment-gone"); berr != nil {
				inv.logger.Printf("case %s: attachment-gone block: %v", caseID, berr)
			}
			return
		}
		inv.logger.Printf("case %s: item: %v", caseID, err)
		return
	}

	// claim: queued → in_repair (loop guard escalation included — a case
	// past max attempts is blocked_for_dudu by the claim itself)
	if _, err := inv.deps.Rep.ClaimRepairCase(ctx, caseID); err != nil {
		inv.logger.Printf("case %s: claim: %v", caseID, err)
		return
	}

	inv.logger.Printf("case %s: invoking fixer for key %s", caseID, item.AttachmentKey)
	rc, out, runErr := inv.runFixer(ctx, item)

	if rc == 0 && runErr == nil {
		inv.handleSuccess(ctx, caseID, item, out)
		return
	}
	inv.handleFailure(ctx, caseID, rc, runErr)
}

// fixerArgs builds the wrapper arguments for one repair item (#220: EPUB
// cases route through fix.sh's --format epub arm with the local source
// path; PDF cases stay byte-identical to the pre-#205 shape).
func fixerArgs(item *repo.RepairItem) []string {
	args := []string{item.AttachmentKey, "--apply"}
	if strings.Contains(item.ContentType, "epub") {
		args = append(args, "--format", "epub",
			"--source", strings.TrimPrefix(item.LocalPath, "file://"))
	}
	return args
}

// repairArtifactName is the healed file the wrapper must leave under
// WorkRoot/<key>/ after a successful run — work.epub for EPUB cases.
func repairArtifactName(item *repo.RepairItem) string {
	if strings.Contains(item.ContentType, "epub") {
		return "work.epub"
	}
	return "work.pdf"
}

// runFixer executes Command <key> --apply under the backstop timeout.
// It returns (exit code, captured output tail, error) — the output feeds
// the HALT-terminal classification in handleSuccess (#253).
// The fixer runs in its OWN process group and the backstop kills the whole
// group: a wedged wrapper's python child must not survive as an orphan on
// the same key (fix.sh's stale-lock recovery would let the immediate retry
// spawn a SECOND agent on the same working directory).
func (inv *Invoker) runFixer(ctx context.Context, item *repo.RepairItem) (int, string, error) {
	cctx, cancel := context.WithTimeout(ctx, inv.cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, inv.cfg.Command, fixerArgs(item)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second // insurance: if a child ever double-forked, don't hang cmd.Run past the group kill
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// negative pid = the whole process group
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return cmd.Process.Kill()
	}
	// bound the captured output: a chatty 30-min run must not balloon RSS —
	// keep the LAST bytes (tail), errors point at the end of the log anyway
	buf := &tailBuffer{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = buf, buf
	err := cmd.Run()
	out := buf.String()
	if cctx.Err() == context.DeadlineExceeded {
		return -1, out, fmt.Errorf("timeout nach %s (backstop; fix.sh hätte bei 30m töten müssen): %s",
			inv.cfg.Timeout, lastLines([]byte(out)))
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), out, fmt.Errorf("fixer exit %d: %s", ee.ExitCode(), lastLines([]byte(out)))
		}
		return -1, out, fmt.Errorf("fixer spawn: %v: %s", err, lastLines([]byte(out)))
	}
	return 0, out, nil
}

// tailBuffer keeps at most the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }

func (inv *Invoker) handleSuccess(ctx context.Context, caseID string, item *repo.RepairItem, out string) {
	artifact := filepath.Join(inv.cfg.WorkRoot, item.AttachmentKey, repairArtifactName(item))
	pdf, err := os.ReadFile(artifact)
	if err != nil || len(pdf) == 0 {
		// #253: exit 0 without an artifact is the fixer's honest verdict
		// report. A HALT verdict terminally parks the case (reason
		// no-healable-defect-evidenced / needs-evidence) — never the old
		// endless requeue. Anything unparsable keeps the retry policy.
		if reason, ok := haltTerminalReason(out); ok {
			if terr := inv.deps.Rep.MarkRepairFailed(ctx, caseID, reason); terr != nil {
				inv.logger.Printf("case %s: halt-terminal: %v", caseID, terr)
			}
			inv.logger.Printf("case %s: HALT terminally parked (%s)", caseID, reason)
			return
		}
		inv.failOrRequeue(ctx, caseID, fmt.Sprintf("fixer exit 0 aber kein geheiltes Artefakt unter %s", artifact))
		return
	}
	if _, err := repair.Apply(ctx, inv.deps.Apply, inv.deps.QuarantineRoot, repair.ApplyCase{
		CaseID:        caseID,
		AttachmentID:  item.AttachmentID,
		AttachmentKey: item.AttachmentKey,
		DocumentKey:   item.DocumentKey,
		Title:         item.Title,
		Creators:      item.Creators,
		Year:          item.Year,
		Publisher:     item.Publisher,
		SrcPath:       strings.TrimPrefix(item.LocalPath, "file://"),
		ContentType:   item.ContentType,
	}, pdf); err != nil {
		// repair.Apply already marked the case failed with the step named.
		inv.logger.Printf("case %s: apply: %v", caseID, err)
		return
	}
	inv.logger.Printf("case %s: healed (new attachment uploaded, awaiting preflight GREEN)", caseID)
}

func (inv *Invoker) handleFailure(ctx context.Context, caseID string, rc int, runErr error) {
	inv.failOrRequeue(ctx, caseID, fmt.Sprintf("fixer: %v", runErr))
}

// haltTerminalReason extracts the fixer's report verdict from the captured
// output (the repair_agent prints its JSON report as the LAST thing on
// stdout) and classifies a HALT terminally (#253):
//
//   - verdict != "halt" (or no parsable report)  → not terminal ("", false)
//   - halt with missing evidence markers on a
//     principally repairable class               → needs-evidence: …
//   - every other halt                           → no-healable-defect-evidenced: …
//
// The reason is surfaced verbatim for the outcome API (#252).
func haltTerminalReason(out string) (string, bool) {
	// the report is the last JSON object in the output — find the last '{'
	// whose suffix parses (logs precede it)
	for i := strings.LastIndex(out, "{"); i >= 0; i = strings.LastIndex(out[:i], "{") {
		var report struct {
			Verdict  string   `json:"verdict"`
			Unproven []string `json:"unproven"`
		}
		if err := json.Unmarshal([]byte(out[i:]), &report); err != nil {
			continue
		}
		if report.Verdict != "halt" {
			return "", false
		}
		for _, u := range report.Unproven {
			lu := strings.ToLower(u)
			if strings.Contains(lu, "nicht erreichbar") ||
				strings.Contains(lu, "nicht prüfbar") ||
				strings.Contains(lu, "offene stelle") {
				return "needs-evidence: " + firstLine(u), true
			}
		}
		return "no-healable-defect-evidenced: " + report.Verdict, true
	}
	return "", false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (inv *Invoker) failOrRequeue(ctx context.Context, caseID, reason string) {
	status, err := inv.deps.Rep.FailOrRequeueRepairCase(ctx, caseID, reason, 0)
	if err != nil {
		inv.logger.Printf("case %s: fail-or-requeue: %v", caseID, err)
		return
	}
	switch status {
	case repo.RepairQueued:
		inv.logger.Printf("case %s: failed → requeued for retry (%s)", caseID, reason)
	case repo.RepairFailed:
		inv.logger.Printf("case %s: failed terminally, parked for dudu (%s)", caseID, reason)
	}
}

// lastLines bounds fixer output in log/error lines.
func lastLines(out []byte) string {
	const max = 400
	s := string(out)
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}
