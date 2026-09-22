package deprecate

import (
	"bytes"
	"log"
	"sync"
	"testing"
)

// captureLog redirects the standard logger for one fn call (restores
// the PREVIOUS writer — SetOutput(nil) leaves a nil writer behind and
// the next log.Printf in the process panics, review M1 #296).
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// DoD #296 (a): exactly-once-per-process semantics — 100 calls of one
// legacy name produce ONE warning line; a second name warns again
// exactly once (once per name, not once globally).
func TestWarnsExactlyOncePerProcessPerName(t *testing.T) {
	reset()
	var lines int64
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		lines++
		mu.Unlock()
		return len(p), nil
	}))
	defer log.SetOutput(prev)
	for i := 0; i < 100; i++ {
		Use("axiom-ng")
	}
	Use("axiom-fixer")
	Use("axiom-fixer")
	mu.Lock()
	got := lines
	mu.Unlock()
	if got != 2 {
		t.Fatalf("warning lines = %d, want 2 (once per name, not per call)", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// DoD #296: every call counts — the counter is the utilization basis
// for the removal decision, so 100 calls must read 100, not 1
// (warn-once ≠ count-once, documented in ADR 0001 §6).
func TestCountsEveryCall(t *testing.T) {
	reset()
	SetSilent(true)
	for i := 0; i < 100; i++ {
		Use("axiom-ng")
	}
	Use("axiom-fixer")
	c := Counts()
	if c["axiom-ng"] != 100 || c["axiom-fixer"] != 1 {
		t.Fatalf("counts = %v, want axiom-ng:100 axiom-fixer:1", c)
	}
}

// DoD #296 (b): silenceable for tests — no log output, counting
// continues.
func TestSilentSuppressesWarningOnly(t *testing.T) {
	reset()
	SetSilent(true)
	defer SetSilent(false)
	out := captureLog(t, func() { Use("axiom-ng") })
	if out != "" {
		t.Fatalf("silent mode still logged %q", out)
	}
	if Counts()["axiom-ng"] != 1 {
		t.Fatalf("silent mode must keep counting")
	}
}

// DoD #296 (c): no effect on existing behavior — Counts on an unused
// witness returns an empty, non-nil map (health serializes `{}`).
func TestCountsEmptyWhenUnused(t *testing.T) {
	reset()
	if c := Counts(); c == nil || len(c) != 0 {
		t.Fatalf("counts = %v, want empty non-nil map", c)
	}
}

// Mutation sonde: a name that was never warned still counts — guards
// against an implementation that conflates the warned-set with the
// counter map.
func TestWarnSetIndependentOfCounts(t *testing.T) {
	reset()
	SetSilent(true)
	Use("x")
	Use("x")
	if len(warned) != 1 || counts["x"] != 2 {
		t.Fatalf("warned=%v counts=%v, want 1 warned name, 2 uses", warned, counts)
	}
}
