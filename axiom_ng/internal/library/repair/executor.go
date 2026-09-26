// executor.go — the F08 (#302) execution seam. The Library owns the
// repair orchestration (queue, custody, wave-gate policy); the WORKER —
// the isolated axiom-repair-worker, one short-lived process per case —
// sits behind RepairExecutor. v1 binding: the local process adapter
// (the #206/#298 process-group-hardened exec, moved verbatim from
// fixerinvoker.runFixerCmd). Remote/Kubernetes executors are interface
// scope only (out of scope for F08).
package repair

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
)

// RepairRequest is ONE case's execution: exactly what the isolated
// axiom-repair-worker needs — the Zotero key plus the invocation
// contract fix.sh established (#205/#220/#284). The orchestrator builds
// it from the case item (metadata policy); the executor turns it into
// the worker's CLI.
type RepairRequest struct {
	// AttachmentKey is the worker's invocation key (one process per key).
	AttachmentKey string
	// Format routes the repair arm (#220): "pdf" (default) or "epub".
	Format string
	// SourcePath is the local source for the EPUB arm (file:// stripped).
	SourcePath string
	// Language is the tesseract code for OCR-class PDF cases (#284);
	// empty = the worker's internal default (deu).
	Language string
	// OCRForce rasterizes a broken text layer away (#284 force mode).
	OCRForce bool
	// Budget is the wedge-guard backstop for THIS invocation: the whole
	// process group is killed at its expiry (orphan prevention, never a
	// tempo limit — #293 layering: the ORCHESTRATOR hands the wrapper its
	// own earlier kill via Env when the class calls for it).
	Budget time.Duration
	// Env is the OPTIONAL full child environment (os.Environ() plus the
	// class-coupled wrapper vars, e.g. AXIOM_FIX_SH_TIMEOUT for OCR-class
	// wedge-guards). nil = inherit the parent environment — the exact
	// pre-F08 layering contract: normal-class runs pass nothing, the
	// wrapper's internal 30-min default applies.
	Env []string
}

// RepairResult reports one worker execution. Output is the captured
// tail (bounded) — the HALT-terminal classification reads the worker's
// JSON report from it (#253).
type RepairResult struct {
	ExitCode int
	Output   string
}

// RepairExecutor executes exactly one repair request. The interface is
// the Library's execution boundary (F08 #302): the orchestrator never
// spawns processes itself — local, remote or fake, every binding lands
// behind Execute.
type RepairExecutor interface {
	Execute(ctx context.Context, req RepairRequest) (RepairResult, error)
}

// CanonicalWorkerCommand is the default axiom-repair-worker path (ADR
// 0001 §2/§4). Legacy installs carry only bin/axiom-fixer until they
// re-run install_dist.sh — the local adapter falls back to it (witnessed).
const (
	CanonicalWorkerCommand = "/opt/axiom/bin/axiom-repair-worker"
	LegacyWorkerCommand    = "/opt/axiom/bin/axiom-fixer"
)

// LocalExecutor is the v1 RepairExecutor: runs the axiom-repair-worker
// wrapper as a local child process. The exec is process-group-hardened
// (the backstop kills the WHOLE group — a wedged wrapper's python child
// must not survive as an orphan on the same key, where fix.sh's
// stale-lock recovery would let the immediate retry spawn a SECOND agent
// on the same working directory).
type LocalExecutor struct {
	// Command is the worker wrapper. Empty (or the canonical default)
	// resolves: canonical axiom-repair-worker if installed, else the
	// legacy axiom-fixer shim (witnessed), else the canonical path.
	Command string
	// CanonicalPath/LegacyPath override the resolution targets (the /opt
	// constants by default; tests stage temp dirs).
	CanonicalPath string
	LegacyPath    string
	// TestName overrides the deprecation-witness label for the legacy
	// fallback (tests isolate the witness). Empty = "axiom-fixer".
	TestName string
}

// Execute runs Command <key> --apply … under req.Budget.
func (e LocalExecutor) Execute(ctx context.Context, req RepairRequest) (RepairResult, error) {
	cmd := e.command()
	cctx, cancel := context.WithTimeout(ctx, req.Budget)
	defer cancel()
	c := exec.CommandContext(cctx, cmd, workerArgs(req)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.WaitDelay = 10 * time.Second // insurance: if a child ever double-forked, don't hang Run past the group kill
	c.Cancel = func() error {
		if c.Process != nil {
			// negative pid = the whole process group
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		return c.Process.Kill()
	}
	if req.Env != nil {
		c.Env = req.Env
	}
	// bound the captured output: a chatty 30-min run must not balloon RSS —
	// keep the LAST bytes (tail), errors point at the end of the log anyway
	buf := &tailBuffer{max: 1 << 20}
	c.Stdout, c.Stderr = buf, buf
	err := c.Run()
	out := buf.String()
	if cctx.Err() == context.DeadlineExceeded {
		return RepairResult{ExitCode: -1, Output: out}, fmt.Errorf("timeout nach %s (backstop): %s",
			req.Budget, lastLines([]byte(out)))
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return RepairResult{ExitCode: ee.ExitCode(), Output: out}, fmt.Errorf("worker exit %d: %s", ee.ExitCode(), lastLines([]byte(out)))
		}
		return RepairResult{ExitCode: -1, Output: out}, fmt.Errorf("worker spawn: %v: %s", err, lastLines([]byte(out)))
	}
	return RepairResult{ExitCode: 0, Output: out}, nil
}

// command resolves the invocation target: the configured command wins;
// the canonical default falls back to the legacy axiom-fixer shim when
// the canonical binary is not installed yet (operators change nothing
// during 0.2.x — the swap is optional). Every legacy fallback is counted
// by the deprecation witness (ADR 0001 §6; the 0.3.x removal decision
// reads these counters, not gut feeling).
func (e LocalExecutor) command() string {
	canonical, legacy := e.CanonicalPath, e.LegacyPath
	if canonical == "" {
		canonical = CanonicalWorkerCommand
	}
	if legacy == "" {
		legacy = LegacyWorkerCommand
	}
	if e.Command != "" && e.Command != canonical {
		return e.Command
	}
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	if _, err := os.Stat(legacy); err == nil {
		name := e.TestName
		if name == "" {
			name = "axiom-fixer"
		}
		deprecate.Use(name)
		return legacy
	}
	return canonical // nothing installed: spawn fails loudly with the canonical path
}

// workerArgs builds the wrapper arguments for one request (#220: EPUB
// cases route through fix.sh's --format epub arm with the local source
// path; PDF cases stay byte-identical to the pre-#205 shape; #284:
// OCR-class PDF cases append the language and force mode).
func workerArgs(req RepairRequest) []string {
	args := []string{req.AttachmentKey, "--apply"}
	if req.Format == "epub" {
		args = append(args, "--format", "epub", "--source", req.SourcePath)
		return args
	}
	if req.Language != "" {
		args = append(args, "--lang", req.Language)
	}
	if req.OCRForce {
		args = append(args, "--ocr-mode", "force")
	}
	return args
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

// lastLines bounds worker output in log/error lines.
func lastLines(out []byte) string {
	const max = 400
	s := string(out)
	if len(s) > max {
		s = "…" + s[len(s)-max:]
	}
	return s
}
