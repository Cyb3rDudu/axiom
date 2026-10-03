package config

import (
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/deprecate"
)

// F10 #304, ADR 0001 §4: the dispatcher-side compute-worker pointing vars
// read the canonical AXIOM_COMPUTE_WORKER_* spelling; the legacy spellings
// (AXIOM_PROCESSOR_URL(S)/NAME/TIMEOUT/SOURCE_*, AXIOM_RUNNER_HEALTH_
// INTERVAL) keep the 0.1.x contract working through the deprecation
// witness. Remote carrier deployments change nothing during 0.2.x.

func TestComputeWorkerURLCanonicalWins(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://carrier:19542")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy:8012")
	before := deprecate.Counts()["AXIOM_PROCESSOR_URL"]
	cfg := Load()
	if cfg.ProcessorURL != "http://carrier:19542" {
		t.Fatalf("canonical AXIOM_COMPUTE_WORKER_URL must win, got %q", cfg.ProcessorURL)
	}
	if after := deprecate.Counts()["AXIOM_PROCESSOR_URL"]; after != before {
		t.Fatalf("a shadowed legacy var must NOT be witnessed (before %d after %d)", before, after)
	}
}

func TestComputeWorkerURLLegacyFeedsAndWitnesses(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_URL", "")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy:8012")
	before := deprecate.Counts()["AXIOM_PROCESSOR_URL"]
	cfg := Load()
	if cfg.ProcessorURL != "http://legacy:8012" {
		t.Fatalf("legacy AXIOM_PROCESSOR_URL must feed ProcessorURL, got %q", cfg.ProcessorURL)
	}
	if after := deprecate.Counts()["AXIOM_PROCESSOR_URL"]; after != before+1 {
		t.Fatalf("legacy AXIOM_PROCESSOR_URL use must be witnessed exactly once per Load (before %d after %d)", before, after)
	}
}

func TestIngestCandidatesCanonicalPluralWins(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_URLS", "http://carrier:19542, http://mac:8012/")
	t.Setenv("AXIOM_PROCESSOR_URLS", "")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://ignored:8012")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "")
	want := []string{"http://carrier:19542", "http://mac:8012"}
	if got := Load().IngestCandidates(); !equalStrings(got, want) {
		t.Fatalf("canonical plural precedence: got %v want %v", got, want)
	}
}

func TestIngestCandidatesLegacyPluralFold(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_URLS", "")
	t.Setenv("AXIOM_PROCESSOR_URLS", "http://carrier:19542, http://mac:8012/")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://ignored:8012")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "")
	before := deprecate.Counts()["AXIOM_PROCESSOR_URLS"]
	want := []string{"http://carrier:19542", "http://mac:8012"}
	if got := Load().IngestCandidates(); !equalStrings(got, want) {
		t.Fatalf("legacy plural fold: got %v want %v", got, want)
	}
	if after := deprecate.Counts()["AXIOM_PROCESSOR_URLS"]; after != before+1 {
		t.Fatalf("legacy plural use must be witnessed (before %d after %d)", before, after)
	}
}

func TestComputeWorkerNameAndTimeoutAliases(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_NAME", "")
	t.Setenv("AXIOM_PROCESSOR_RUNNER_NAME", "carrier-gpu0")
	t.Setenv("AXIOM_COMPUTE_WORKER_TIMEOUT", "")
	t.Setenv("AXIOM_PROCESSOR_TIMEOUT", "45s")
	cfg := Load()
	if cfg.ProcessorRunnerName != "carrier-gpu0" {
		t.Fatalf("legacy AXIOM_PROCESSOR_RUNNER_NAME must feed the name, got %q", cfg.ProcessorRunnerName)
	}
	if cfg.ProcessorRequestTimeout.String() != "45s" {
		t.Fatalf("legacy AXIOM_PROCESSOR_TIMEOUT must feed the budget, got %s", cfg.ProcessorRequestTimeout)
	}
	for _, legacy := range []string{"AXIOM_PROCESSOR_RUNNER_NAME", "AXIOM_PROCESSOR_TIMEOUT"} {
		if deprecate.Counts()[legacy] < 1 {
			t.Fatalf("legacy %s use must be witnessed", legacy)
		}
	}
}

func TestComputeWorkerHealthIntervalAlias(t *testing.T) {
	t.Setenv("AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL", "")
	t.Setenv("AXIOM_RUNNER_HEALTH_INTERVAL", "15s")
	if got := Load().RunnerHealthInterval; got.String() != "15s" {
		t.Fatalf("legacy AXIOM_RUNNER_HEALTH_INTERVAL must feed the interval, got %s", got)
	}
	if deprecate.Counts()["AXIOM_RUNNER_HEALTH_INTERVAL"] < 1 {
		t.Fatal("legacy AXIOM_RUNNER_HEALTH_INTERVAL use must be witnessed")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
