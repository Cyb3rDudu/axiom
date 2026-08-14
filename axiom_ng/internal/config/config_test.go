package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	os.Unsetenv("AXIOMNG_OPENSEARCH_URL") // pin the unset branch, not the developer shell
	t.Setenv("AXIOMNG_ZOTERO_BASE", "")
	t.Setenv("AXIOMNG_ZOTERO_LIBRARY", "")
	t.Setenv("AXIOMNG_API_PORT", "")

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
	t.Setenv("AXIOMNG_ZOTERO_BASE", "http://remote:23119/api")
	t.Setenv("AXIOMNG_API_PORT", "9999")

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
	t.Setenv("AXIOMNG_OPENSEARCH_URL", "")
	if c := Load(); c.OpenSearchURL != "" {
		t.Errorf("OpenSearchURL = %q, want empty (explicitly disabled)", c.OpenSearchURL)
	}
}

func TestOpenSearchURLUnsetGoesDefault(t *testing.T) {
	// UNSET (as opposed to set-empty) falls back to the local mothership.
	old, had := os.LookupEnv("AXIOMNG_OPENSEARCH_URL")
	os.Unsetenv("AXIOMNG_OPENSEARCH_URL")
	if had {
		defer os.Setenv("AXIOMNG_OPENSEARCH_URL", old)
	}
	if c := Load(); c.OpenSearchURL != "http://127.0.0.1:9200" {
		t.Errorf("OpenSearchURL = %q, want default http://127.0.0.1:9200", c.OpenSearchURL)
	}
}

func TestProcessorSourceDefaults(t *testing.T) {
	// Source delivery off by default: empty secret disables the endpoint and
	// the dispatcher signs nothing; the base falls back to the local API port.
	os.Unsetenv("AXIOMNG_PROCESSOR_SOURCE_BASE_URL")
	os.Unsetenv("AXIOMNG_PROCESSOR_SOURCE_SECRET")
	os.Unsetenv("AXIOMNG_API_PORT")
	if c := Load(); c.ProcessorSourceSecret != "" {
		t.Errorf("ProcessorSourceSecret = %q, want empty default", c.ProcessorSourceSecret)
	} else if c.ProcessorSourceBaseURL != "http://127.0.0.1:8011" {
		t.Errorf("ProcessorSourceBaseURL = %q, want loopback API default", c.ProcessorSourceBaseURL)
	}
}

func TestProcessorSourceOverrides(t *testing.T) {
	t.Setenv("AXIOMNG_PROCESSOR_SOURCE_BASE_URL", "http://100.79.104.120:8011")
	t.Setenv("AXIOMNG_PROCESSOR_SOURCE_SECRET", "shared-secret")
	c := Load()
	if c.ProcessorSourceBaseURL != "http://100.79.104.120:8011" {
		t.Errorf("ProcessorSourceBaseURL = %q, want tailnet override", c.ProcessorSourceBaseURL)
	}
	if c.ProcessorSourceSecret != "shared-secret" {
		t.Errorf("ProcessorSourceSecret = %q, want override", c.ProcessorSourceSecret)
	}
}

func TestProcessorRequestTimeoutOverride(t *testing.T) {
	t.Setenv("AXIOMNG_PROCESSOR_TIMEOUT", "300s")
	if c := Load(); c.ProcessorRequestTimeout != 5*time.Minute {
		t.Errorf("ProcessorRequestTimeout = %v, want 5m", c.ProcessorRequestTimeout)
	}
	// Default keeps the tight 30s when unset.
	os.Unsetenv("AXIOMNG_PROCESSOR_TIMEOUT")
	if c := Load(); c.ProcessorRequestTimeout != 30*time.Second {
		t.Errorf("ProcessorRequestTimeout = %v, want default 30s", c.ProcessorRequestTimeout)
	}
}
