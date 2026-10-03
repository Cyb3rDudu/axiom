package dispatcher

import (
	"testing"
)

// TestSoloLoopbackSourceGuard (production finding 2026-09-05): when all
// ingest candidates are loopback, a configured non-loopback source base
// (a stale LAN IP after a network change) is overridden to loopback —
// zero-byte connect timeouts on every source download otherwise.
func TestSoloLoopbackSourceGuard(t *testing.T) {
	if !loopbackBaseURL("http://127.0.0.1:8011") || !loopbackBaseURL("http://localhost:8011") {
		t.Fatal("loopback spellings must be recognized")
	}
	if loopbackBaseURL("http://192.168.0.107:8011") {
		t.Fatal("LAN address is not loopback")
	}
	// The guard predicate over the plain client + chain shapes is
	// exercised in the dispatcher integration tests; the URL predicate
	// is the decision core and pinned here.
}
