// quiet_test.go — the goleak-equivalent witness (#298 DoD "nach Stop ist der
// Prozess innerlich ruhig"). A dependency was deliberately NOT added for
// this: runtime.NumGoroutine polling plus a goroutine-stack dump on failure
// provides the same guarantee (and the same failure diagnostics) with the
// standard library alone.
package composition

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// goroutineBaseline snapshots the current goroutine count.
func goroutineBaseline(t *testing.T) int {
	t.Helper()
	n := runtime.NumGoroutine()
	t.Logf("goroutine baseline: %d", n)
	return n
}

// assertGoroutinesSettled polls until the goroutine count returns to (or
// below) the baseline taken before the composition started, within budget.
// Transient stragglers (a WS write pump inside its 5s write deadline, a
// health probe finishing) settle inside the window; a true leak (a claim
// loop, an outbox drainer, a ticker) does not. On failure it dumps every
// goroutine stack so the leak is readable from the test output.
func assertGoroutinesSettled(t *testing.T, baseline int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			t.Logf("goroutines settled: %d <= baseline %d", n, baseline)
			return
		}
		if time.Now().After(deadline) {
			var stacks string
			if p := pprof.Lookup("goroutine"); p != nil {
				buf := &strings.Builder{}
				_ = p.WriteTo(buf, 1)
				stacks = buf.String()
			}
			t.Fatalf("goroutines did not settle after Stop: %d > baseline %d — leaked goroutines:\n%s",
				n, baseline, stacks)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertProcessExited asserts the command terminated (the no-zombie half of
// the selective-start DoD — a half-started process must not linger).
func assertProcessExited(cmd *os.ProcessState, name string) error {
	if cmd.Exited() {
		return nil
	}
	return fmt.Errorf("%s: process state is not a clean exit: %s", name, cmd.String())
}
