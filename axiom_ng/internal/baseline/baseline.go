// Package baseline — the frozen v0.1.18 compatibility baseline (#295).
//
// Every 0.2.0 step runs against this suite: it witnesses the API surface,
// the schema, and the fachliche behavior of the freeze bits (tag v0.1.18,
// commit 4704656; deployed binary generation bf77410). A step may ADD
// fields/routes/migrations — it must consciously update the fixture in the
// same PR (the gate goes red otherwise); silently changing existing meaning
// is impossible to land.
//
// Three run modes (all in one package):
//
//   - derived, no env:   API inventory from the chi router (TestInventory*),
//     comparator mutation probes (TestSearchGoldenComparator*,
//     TestIngestProgressProbe*). These run in plain `go test ./...` and CI.
//   - fingerprint:       needs AXIOM_BASELINE_DSN (fresh CI DB after
//     db.Migrate, or axiom_dev on the dev host). Regenerates the schema
//     fingerprint and diffs it against the committed artifact.
//   - live:              needs AXIOM_BASELINE_LIVE=1 and the dev environment
//     in --release mode (scripts/dev/dev-up.sh --release). Runs health,
//     capabilities, search/passage/KG golden probes, the full-path ingest
//     probe, and the repair/custody fake-worker probe against the freeze
//     bits over HTTP.
//
// Fixtures live in fixtures/ and are the baseline artifacts. Actuals are
// written to $AXIOM_BASELINE_ACTUAL (set by `make golden-baseline`) so two
// consecutive runs can be proven byte-identical (determinism DoD).
package baseline

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Freeze identity — stamped into every artifact header. The deployed RAG
// binary inside the v0.1.18 release identifies as commit bf77410 (59
// commits after v0.1.17; bf77410..v0.1.18 touches no runtime code).
const (
	FreezeTag      = "v0.1.18"
	FreezeCommit   = "4704656"
	FreezeGen      = "bf77410"
	FreezeRAGBuild = "axiom-ng v0.1.17-59-gbf77410 (commit bf77410, release build)"
)

var actualDir = os.Getenv("AXIOM_BASELINE_ACTUAL")

func liveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("AXIOM_BASELINE_LIVE") != "1" {
		t.Skip("AXIOM_BASELINE_LIVE != 1 — live golden probes need the dev env in --release mode")
	}
}

// canonicalJSON re-serializes v so that equal values always produce equal
// bytes: map keys sorted, no HTML escaping, stable indentation. Health's
// checks map iterates in random order at the source — canonicalization is
// what makes the snapshot byte-comparable across runs.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// goldenCompare asserts that got equals the committed fixture name, after
// canonicalization. With -update (or BASELINE_UPDATE=1) it (re)writes the
// fixture instead — the deliberate, review-visible baseline change.
func goldenCompare(t *testing.T, name string, got any) {
	t.Helper()
	wantPath := filepath.Join("fixtures", name)
	gotBytes, err := canonicalJSON(got)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", name, err)
	}
	if actualDir != "" {
		if err := os.MkdirAll(actualDir, 0o755); err != nil {
			t.Fatalf("actual dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(actualDir, name), gotBytes, 0o644); err != nil {
			t.Fatalf("write actual %s: %v", name, err)
		}
	}
	if update := os.Getenv("BASELINE_UPDATE") == "1" || *flagUpdate; update {
		if err := os.WriteFile(wantPath, gotBytes, 0o644); err != nil {
			t.Fatalf("update fixture %s: %v", name, err)
		}
		t.Logf("fixture UPDATED: %s", wantPath)
		return
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("fixture %s missing (run with BASELINE_UPDATE=1 to freeze): %v", name, err)
	}
	if !bytes.Equal(want, gotBytes) {
		t.Fatalf("baseline drift in %s:\n--- frozen ---\n%s\n+++ actual +++\n%s",
			name, want, gotBytes)
	}
}

var flagUpdate = flag.Bool("update", false, "update golden fixtures instead of comparing")

// hashLines returns the sha256 over the given canonical bytes, hex-encoded.
func hashLines(b []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
