package config

import "testing"

// #368: the dual-fed shadowing matrix is gone with the legacy spellings —
// the effective view carries ONLY canonical rows. Probe: no removed name
// owns a row, and the canonical compute-worker URL row renders the plain
// env/default truth.
func TestEffectiveHasNoLegacyRows(t *testing.T) {
	rows := map[string]bool{}
	for _, e := range Effective(Load()) {
		rows[e.Env] = true
	}
	for name := range legacyProbeNames {
		if rows[name] {
			t.Errorf("effective view still carries removed legacy row %s", name)
		}
	}
	for _, want := range []string{
		"AXIOM_STORE_DATABASE_URL", "AXIOM_REPAIR_WORKER_CMD",
		"AXIOM_COMPUTE_WORKER_URL", "AXIOM_COMPUTE_WORKER_URLS",
		"AXIOM_COMPUTE_WORKER_NAME", "AXIOM_COMPUTE_WORKER_TIMEOUT",
		"AXIOM_COMPUTE_WORKER_SOURCE_SECRET", "AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL",
		"AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL",
	} {
		if !rows[want] {
			t.Errorf("canonical row %s missing from effective view", want)
		}
	}
}

func TestEffectiveComputeWorkerURLSourceTruth(t *testing.T) {
	var e Entry
	for _, row := range Effective(Load()) {
		if row.Env == "AXIOM_COMPUTE_WORKER_URL" {
			e = row
		}
	}
	if e.Source != "default" {
		t.Fatalf("unset key renders source %q, want default", e.Source)
	}
	t.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://carrier:19542")
	for _, row := range Effective(Load()) {
		if row.Env == "AXIOM_COMPUTE_WORKER_URL" {
			e = row
		}
	}
	if e.Source != "env" || e.Value != "http://carrier:19542" {
		t.Fatalf("set key renders %v/%v, want env + value", e.Source, e.Value)
	}
}
