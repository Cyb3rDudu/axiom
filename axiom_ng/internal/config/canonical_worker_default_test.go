package config

import (
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library/repair"
)

// The config↔repair weld (F08 #302, review round 1): Load's default for
// FixerCommand must BE repair.CanonicalWorkerCommand — the LocalExecutor
// resolution, the install shim and the docs all key off the constant, so
// a drifted literal in config would silently change what gets spawned.
// The repair import is TEST-ONLY: config stays import-light in
// production, and repair does not import config (no cycle).

// TestRepairWorkerCommandDefault pins the default against both the
// canonical path literal and the repair constant (t.Setenv(key, "")
// neutralizes ambient overrides — repairWorkerCmd treats "" as unset).
func TestRepairWorkerCommandDefault(t *testing.T) {
	t.Setenv("AXIOM_REPAIR_WORKER_CMD", "")
	t.Setenv("AXIOM_FIXER_CMD", "")

	cfg := Load()
	if cfg.FixerCommand != "/opt/axiom/bin/axiom-repair-worker" {
		t.Fatalf("FixerCommand default = %q, want the canonical worker path", cfg.FixerCommand)
	}
	if cfg.FixerCommand != repair.CanonicalWorkerCommand {
		t.Fatalf("weld drifted: config default %q != repair.CanonicalWorkerCommand %q",
			cfg.FixerCommand, repair.CanonicalWorkerCommand)
	}
}

// TestRepairWorkerCommandPrecedence pins the resolution order and the
// deprecation witness: the canonical AXIOM_REPAIR_WORKER_CMD wins and is
// NOT witnessed; the legacy AXIOM_FIXER_CMD feeds FixerCommand only when
// the canonical var is unset, and that read is counted (ADR 0001 §6).
// deprecate has no cross-package reset — the before/after snapshots keep
// the assertions deterministic against any counter state other tests in
// this binary left behind (config tests run sequentially in-package).
func TestRepairWorkerCommandPrecedence(t *testing.T) {
	deprecate.SetSilent(true)
	t.Cleanup(func() { deprecate.SetSilent(false) })

	// legacy alone: feeds FixerCommand, witnessed
	t.Setenv("AXIOM_REPAIR_WORKER_CMD", "")
	t.Setenv("AXIOM_FIXER_CMD", "/opt/legacy/axiom-fixer")
	before := deprecate.Counts()["AXIOM_FIXER_CMD"]
	cfg := Load()
	if cfg.FixerCommand != "/opt/legacy/axiom-fixer" {
		t.Fatalf("legacy AXIOM_FIXER_CMD must feed FixerCommand, got %q", cfg.FixerCommand)
	}
	if after := deprecate.Counts()["AXIOM_FIXER_CMD"]; after != before+1 {
		t.Fatalf("legacy read must be witnessed exactly once: count %d → %d", before, after)
	}

	// canonical wins: the legacy var is present but never read, witness
	// unchanged
	t.Setenv("AXIOM_REPAIR_WORKER_CMD", "/opt/canonical/axiom-repair-worker")
	before = deprecate.Counts()["AXIOM_FIXER_CMD"]
	cfg = Load()
	if cfg.FixerCommand != "/opt/canonical/axiom-repair-worker" {
		t.Fatalf("canonical AXIOM_REPAIR_WORKER_CMD must win, got %q", cfg.FixerCommand)
	}
	if after := deprecate.Counts()["AXIOM_FIXER_CMD"]; after != before {
		t.Fatalf("legacy var must not be read when canonical is set: count %d → %d", before, after)
	}
}
