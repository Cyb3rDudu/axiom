package fixerinvoker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The unit tier (no DB): config clamping, output bounding, and the
// runFixer exit-code/timeout mapping against tiny real shell scripts —
// the same surface fix.sh exposes (exit N, output, hanging).

func unitCfg(t *testing.T, script string, timeout time.Duration) Config {
	t.Helper()
	cfg := Config{Command: script, Timeout: timeout, Interval: time.Millisecond}
	cfg.fillDefaults()
	return cfg
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-fixer.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigClamps(t *testing.T) {
	cfg := Config{Concurrency: 0}
	cfg.fillDefaults()
	if cfg.Concurrency != 1 || cfg.Command != "/opt/axiom/bin/axiom-fixer" ||
		cfg.Timeout != 35*time.Minute || cfg.StaleAfter != 40*time.Minute || cfg.Interval != 30*time.Second {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	cfg = Config{Concurrency: 5}
	cfg.fillDefaults()
	if cfg.Concurrency != 2 {
		t.Fatalf("concurrency must clamp to 2, got %d", cfg.Concurrency)
	}
	// StaleAfter must sit above Timeout: a live-but-slow invocation must
	// never be requeued under a second invoker.
	cfg = Config{Timeout: time.Hour}
	cfg.fillDefaults()
	if cfg.StaleAfter <= cfg.Timeout {
		t.Fatalf("StaleAfter (%s) must be > Timeout (%s)", cfg.StaleAfter, cfg.Timeout)
	}
}

func TestRunFixerExitCodeMapping(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, writeScript(t, "echo boom; exit 7\n"), time.Minute)}
	rc, err := inv.runFixer(context.Background(), "K1")
	if rc != 7 || !strings.Contains(err.Error(), "fixer exit 7") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit mapping: rc=%d err=%v", rc, err)
	}
}

func TestRunFixerTimeoutKills(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, writeScript(t, "sleep 30\n"), 150*time.Millisecond)}
	start := time.Now()
	rc, err := inv.runFixer(context.Background(), "K1")
	if rc != -1 || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout must map to rc=-1 + timeout reason, got rc=%d err=%v", rc, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("hung fixer must be killed by the backstop, took %s", time.Since(start))
	}
}

func TestRunFixerSpawnError(t *testing.T) {
	inv := &Invoker{cfg: unitCfg(t, "/nonexistent/fixer-xyz", time.Minute)}
	rc, err := inv.runFixer(context.Background(), "K1")
	if rc != -1 || err == nil || !strings.Contains(err.Error(), "spawn") {
		t.Fatalf("spawn error: rc=%d err=%v", rc, err)
	}
}

func TestLastLinesBoundsOutput(t *testing.T) {
	if got := lastLines([]byte(strings.Repeat("x", 1000))); len(got) > 404 {
		t.Fatalf("output not bounded: %d", len(got))
	}
	if got := lastLines([]byte("short")); got != "short" {
		t.Fatalf("short output mangled: %q", got)
	}
}
