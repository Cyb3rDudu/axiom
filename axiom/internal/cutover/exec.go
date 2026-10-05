// exec.go — the window's process and probe surface: plan-declared
// commands (stop/restart/stage) executed with a bounded timeout and
// their output teed into the run directory, and HTTP checks with a
// retry budget (a service that just restarted may need seconds).
package cutover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runCommand executes one plan command (sh -c), tees combined output
// to logPath (run-dir relative), and enforces the timeout. A non-zero
// exit fails the gate — the window never continues over a failed stop
// or restart.
func runCommand(ctx context.Context, command, logPath string, timeout time.Duration) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	logf, err := openAppend(logPath)
	if err != nil {
		return fmt.Errorf("command log: %w", err)
	}
	defer logf.Close()
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("command timed out after %s: %s (log: %s)", timeout, firstLine(command), logPath)
		}
		return fmt.Errorf("command failed (%v): %s (log: %s)", err, firstLine(command), logPath)
	}
	return nil
}

// runCheck polls one HTTP check until it satisfies the expectation or
// the retry budget is spent.
func runCheck(ctx context.Context, c Check) error {
	if c.Kind != "http" {
		return fmt.Errorf("check: kind %q is not \"http\"", c.Kind)
	}
	expect := c.ExpectStatus
	if expect == 0 {
		expect = http.StatusOK
	}
	budget := time.Duration(c.RetryS) * time.Second
	if budget <= 0 {
		budget = 60 * time.Second
	}
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
		if err != nil {
			return fmt.Errorf("check %s: %w", c.URL, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if rerr != nil {
				err = rerr
			} else if resp.StatusCode != expect {
				err = fmt.Errorf("status %d (want %d)", resp.StatusCode, expect)
			} else if c.ExpectContains != "" && !strings.Contains(string(body), c.ExpectContains) {
				err = fmt.Errorf("body does not contain %q", c.ExpectContains)
			} else {
				return nil
			}
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("check %s failed within %s: %v", c.URL, budget, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// runChecks runs a check list in order.
func runChecks(ctx context.Context, checks []Check) error {
	for _, c := range checks {
		if err := runCheck(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// openAppend opens (creating) a log file for command output.
func openAppend(path string) (*os.File, error) {
	if err := mkdirFor(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
