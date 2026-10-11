package config

import (
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/library/repair"
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

// TestRepairWorkerCommandLegacyGone — #368: the legacy AXIOM_FIXER_CMD
// no longer feeds FixerCommand; the refusal surfaces name the canonical
// successor.
func TestRepairWorkerCommandLegacyGone(t *testing.T) {
	t.Setenv("AXIOM_REPAIR_WORKER_CMD", "")
	t.Setenv("AXIOM_FIXER_CMD", "/opt/legacy/axiom-fixer")
	if got := Load().FixerCommand; got != "/opt/axiom/bin/axiom-repair-worker" {
		t.Fatalf("legacy AXIOM_FIXER_CMD must not feed FixerCommand, got %q", got)
	}
	if got := Load().FixerCommand; got != repair.CanonicalWorkerCommand {
		t.Fatalf("weld drifted: %q != repair.CanonicalWorkerCommand %q", got, repair.CanonicalWorkerCommand)
	}
}
