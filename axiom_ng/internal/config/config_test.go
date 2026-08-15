package config

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("AXIOM_OPENSEARCH_URL") // pin the unset branch, not the developer shell
	t.Setenv("AXIOM_ZOTERO_BASE", "")
	t.Setenv("AXIOM_ZOTERO_LIBRARY", "")
	t.Setenv("AXIOM_API_PORT", "")

	c := Load()
	if c.ZoteroBaseURL != defaultZoteroBase {
		t.Errorf("ZoteroBaseURL = %q, want %q", c.ZoteroBaseURL, defaultZoteroBase)
	}
	if c.ZoteroLibraryID != defaultLibraryID {
		t.Errorf("ZoteroLibraryID = %q, want %q", c.ZoteroLibraryID, defaultLibraryID)
	}
	if c.APIPort != defaultAPIPort {
		t.Errorf("APIPort = %d, want %d", c.APIPort, defaultAPIPort)
	}
	if c.ProcessorURL == "" || c.OpenSearchURL == "" {
		t.Errorf("default sidecar URLs must be set, got %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("AXIOM_ZOTERO_BASE", "http://remote:23119/api")
	t.Setenv("AXIOM_API_PORT", "9999")

	c := Load()
	if c.ZoteroBaseURL != "http://remote:23119/api" {
		t.Errorf("ZoteroBaseURL = %q, want override", c.ZoteroBaseURL)
	}
	if c.APIPort != 9999 {
		t.Errorf("APIPort = %d, want 9999", c.APIPort)
	}
}

func TestOpenSearchURLSetEmptyDisables(t *testing.T) {
	// Explicitly SET to empty string = intentional disable (the L5 outbox
	// drainer never starts; rows stay pending, no error).
	t.Setenv("AXIOM_OPENSEARCH_URL", "")
	if c := Load(); c.OpenSearchURL != "" {
		t.Errorf("OpenSearchURL = %q, want empty (explicitly disabled)", c.OpenSearchURL)
	}
}

func TestOpenSearchURLUnsetGoesDefault(t *testing.T) {
	// UNSET (as opposed to set-empty) falls back to the local mothership.
	old, had := os.LookupEnv("AXIOM_OPENSEARCH_URL")
	os.Unsetenv("AXIOM_OPENSEARCH_URL")
	if had {
		defer os.Setenv("AXIOM_OPENSEARCH_URL", old)
	}
	if c := Load(); c.OpenSearchURL != "http://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default http://127.0.0.1:9200", c.OpenSearchURL)
	}
}

func TestProcessorSourceDefaults(t *testing.T) {
	// Source delivery off by default: empty secret disables the endpoint and
	// the dispatcher signs nothing; the base falls back to the local API port.
	os.Unsetenv("AXIOM_PROCESSOR_SOURCE_BASE_URL")
	os.Unsetenv("AXIOM_PROCESSOR_SOURCE_SECRET")
	os.Unsetenv("AXIOM_API_PORT")
	if c := Load(); c.ProcessorSourceSecret != "" {
		t.Errorf("ProcessorSourceSecret = %q, want empty default", c.ProcessorSourceSecret)
	} else if c.ProcessorSourceBaseURL != "http://127.0.0.1:8011" {
		t.Errorf("ProcessorSourceBaseURL = %q, want loopback API default", c.ProcessorSourceBaseURL)
	}
}

func TestProcessorSourceOverrides(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_SOURCE_BASE_URL", "http://100.79.104.120:8011")
	t.Setenv("AXIOM_PROCESSOR_SOURCE_SECRET", "shared-secret")
	c := Load()
	if c.ProcessorSourceBaseURL != "http://100.79.104.120:8011" {
		t.Errorf("ProcessorSourceBaseURL = %q, want tailnet override", c.ProcessorSourceBaseURL)
	}
	if c.ProcessorSourceSecret != "shared-secret" {
		t.Errorf("ProcessorSourceSecret = %q, want override", c.ProcessorSourceSecret)
	}
}

func TestProcessorRequestTimeoutOverride(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_TIMEOUT", "300s")
	if c := Load(); c.ProcessorRequestTimeout != 5*time.Minute {
		t.Errorf("ProcessorRequestTimeout = %v, want 5m", c.ProcessorRequestTimeout)
	}
	// Default matches the result-budget table (300s) when unset.
	os.Unsetenv("AXIOM_PROCESSOR_TIMEOUT")
	if c := Load(); c.ProcessorRequestTimeout != 300*time.Second {
		t.Errorf("ProcessorRequestTimeout = %v, want default 300s", c.ProcessorRequestTimeout)
	}
}

func TestDefaultProfileEnablesAllFeatures(t *testing.T) {
	// Benchmark finding 2026-08-14: the bare profile name does NOT enable
	// features (runner reads explicit booleans, defaults false) — the
	// default must materialize every full-rag-v1 feature as true.
	t.Setenv("AXIOM_DISPATCHER_PROFILE", "")
	c := Load()
	var p struct {
		Profile                 string `json:"profile"`
		ExtractEntities         bool   `json:"extract_entities"`
		ExtractRelationships    bool   `json:"extract_relationships"`
		ComputeDenseEmbeddings  bool   `json:"compute_dense_embeddings"`
		ComputeSparseEmbeddings bool   `json:"compute_sparse_embeddings"`
		ExtractImages           bool   `json:"extract_images"`
	}
	if err := json.Unmarshal([]byte(c.DispatcherProfile), &p); err != nil {
		t.Fatalf("default profile is not valid JSON: %v", err)
	}
	if p.Profile != "full-rag-v1" {
		t.Errorf("profile = %q", p.Profile)
	}
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"extract_entities", p.ExtractEntities},
		{"extract_relationships", p.ExtractRelationships},
		{"compute_dense_embeddings", p.ComputeDenseEmbeddings},
		{"compute_sparse_embeddings", p.ComputeSparseEmbeddings},
		{"extract_images", p.ExtractImages},
	} {
		if !f.ok {
			t.Errorf("default profile feature %s = false, want true", f.name)
		}
	}
}

func TestProcessorRunnerName(t *testing.T) {
	t.Setenv("AXIOM_PROCESSOR_RUNNER_NAME", "carrier-gpu0")
	if got := Load().ProcessorRunnerName; got != "carrier-gpu0" {
		t.Fatalf("explicit runner name = %q, want carrier-gpu0", got)
	}
	// Unset derives from the processor URL host (G4: fallback lives in
	// Load, testable) so a bare single-runner deployment still gets a
	// usable identity; an explicit env always wins.
	t.Setenv("AXIOM_PROCESSOR_RUNNER_NAME", "")
	t.Setenv("AXIOM_PROCESSOR_URL", "http://192.168.1.2:19542")
	if got := Load().ProcessorRunnerName; got != "192.168.1.2:19542" {
		t.Fatalf("URL-host fallback = %q, want 192.168.1.2:19542", got)
	}
	// Unparseable URL keeps the name empty (no invented identity).
	t.Setenv("AXIOM_PROCESSOR_URL", "://bad")
	if got := Load().ProcessorRunnerName; got != "" {
		t.Fatalf("bad URL fallback = %q, want empty", got)
	}
}
