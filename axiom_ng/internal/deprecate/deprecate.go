// Package deprecate is the central legacy-usage witness (ADR 0001 §6,
// #296). Legacy entrypoints and env vars stay functional through 0.2.x;
// their use is warned about exactly once per process and counted on
// every call. The counters are exported via /api/health (deprecations)
// and are the data basis for the 0.3.x+ removal decision — not gut
// feeling. F02 ships the mechanism only; the legacy entrypoints
// themselves arrive with F05/F08/F10 and call Use from their aliases.
package deprecate

import (
	"log"
	"sync"
)

var (
	mu     sync.Mutex
	warned = map[string]bool{}
	counts = map[string]int{}
	silent bool
)

// SetSilent suppresses the warning line (tests and fixtures); counting
// continues either way.
func SetSilent(v bool) {
	mu.Lock()
	defer mu.Unlock()
	silent = v
}

// Use records one use of legacy identifier name: one warning line per
// process and name (once-semantics, not per call), counter increments
// on every call.
func Use(name string) {
	mu.Lock()
	first := !warned[name]
	warned[name] = true
	counts[name]++
	s := silent
	mu.Unlock()
	if first && !s {
		log.Printf("axiom: deprecated name %q used (ADR 0001: docs/adr/0001-canonical-naming.md)", name)
	}
}

// Counts returns a copy of the per-name usage counters (sorted keys not
// required — the health JSON canonicalizes).
func Counts() map[string]int {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}

// reset clears all state (test isolation only) — including silent, so
// a test that silenced the witness cannot leak into the next one
// (reproducible under go test -shuffle=on).
func reset() {
	mu.Lock()
	defer mu.Unlock()
	warned = map[string]bool{}
	counts = map[string]int{}
	silent = false
}
