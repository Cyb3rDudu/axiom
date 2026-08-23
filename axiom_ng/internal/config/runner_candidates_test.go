package config

import (
	"reflect"
	"testing"
	"time"
)

// #207: ordered ingest-runner candidate list — parsing, precedence, and the
// legacy fallback fold.

func TestIngestCandidatesPluralWins(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_URLS", "http://carrier:19542, http://mac:8012/")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://ignored:8012")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "http://ignored-fb:8012")
	// dudus Heim-Setup: Carrier first, lokal second — unterwegs ohne Carrier
	// läuft dieselbe Datei weiter (toter Carrier wird vom Health-Monitor
	// übergangen).
	want := []string{"http://carrier:19542", "http://mac:8012"}
	if got := Load().IngestCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plural precedence: got %v want %v", got, want)
	}
}

func TestIngestCandidatesSingularPlusLegacyFallback(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_URLS", "")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://carrier:19542")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "http://mac:8012")
	want := []string{"http://carrier:19542", "http://mac:8012"}
	if got := Load().IngestCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("singular+fallback: got %v want %v", got, want)
	}
}

func TestIngestCandidatesSingleEntryDedupesFallback(t *testing.T) {
	// Ein-Eintrag-Setup = heutiges Verhalten: ein Kandidat, nichts doppelt.
	t.Setenv("AXIOM_PROCESSOR_URL", "http://mac:8012/")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "http://mac:8012")
	want := []string{"http://mac:8012"}
	if got := Load().IngestCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe: got %v want %v", got, want)
	}
}

func TestIngestCandidatesDefaultIsLocalRunner(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_URLS", "")
	t.Setenv("AXIOM_PROCESSOR_URL", "")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "")
	want := []string{"http://localhost:8012"}
	if got := Load().IngestCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default: got %v want %v", got, want)
	}
}

func TestRunnerHealthInterval(t *testing.T) {
	t.Setenv("AXIOM_RUNNER_HEALTH_INTERVAL", "15s")
	if got := Load().RunnerHealthInterval; got != 15*time.Second {
		t.Fatalf("interval override = %v, want 15s", got)
	}
	t.Setenv("AXIOM_RUNNER_HEALTH_INTERVAL", "")
	if got := Load().RunnerHealthInterval; got != 60*time.Second {
		t.Fatalf("interval default = %v, want 60s", got)
	}
}
