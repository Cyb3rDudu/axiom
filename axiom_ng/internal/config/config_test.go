package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
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
