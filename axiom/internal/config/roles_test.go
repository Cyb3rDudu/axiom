package config

import "testing"

// R4 (#134) role URLs: query runner + ingest fallback.

func TestRunnerRoleDefaults(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_URL", "http://carrier:8012") // primary at the GPU box

	c := Load()
	// Query and ingest-fallback stay LOCAL even when the ingest primary is
	// remote — that independence is the whole role model (retrieval survives
	// a Carrier outage; #128 local chunking is the emergency ingest).
	if c.QueryRunnerURL != "http://localhost:8012" {
		t.Errorf("QueryRunnerURL = %q, want local default", c.QueryRunnerURL)
	}
	if c.IngestFallbackURL != "http://localhost:8012" {
		t.Errorf("IngestFallbackURL = %q, want local default", c.IngestFallbackURL)
	}
	if c.ProcessorURL != "http://carrier:8012" {
		t.Errorf("ProcessorURL = %q, want override", c.ProcessorURL)
	}
}

func TestRunnerRoleOverrides(t *testing.T) {
	t.Setenv("AXIOM_QUERY_RUNNER_URL", "http://dedicated-query:8012")
	t.Setenv("AXIOM_INGEST_FALLBACK_URL", "http://second-mac:8012")

	c := Load()
	if c.QueryRunnerURL != "http://dedicated-query:8012" {
		t.Errorf("QueryRunnerURL = %q, want override", c.QueryRunnerURL)
	}
	if c.IngestFallbackURL != "http://second-mac:8012" {
		t.Errorf("IngestFallbackURL = %q, want override", c.IngestFallbackURL)
	}
}

func TestRunnerRoleBadURLIsConfigurationInput(t *testing.T) {
	// A malformed URL is not a config-load error (env strings pass through);
	// it surfaces at client construction/startup probes — the sidecar must
	// not crash-loop on config, search degrades per R3 and the log says why.
	t.Setenv("AXIOM_QUERY_RUNNER_URL", "not a url")
	c := Load()
	if c.QueryRunnerURL != "not a url" {
		t.Errorf("QueryRunnerURL = %q, want pass-through for startup diagnostics", c.QueryRunnerURL)
	}
}
