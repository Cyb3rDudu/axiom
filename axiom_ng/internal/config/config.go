// Package config loads axiom-ng runtime configuration from environment
// variables. axiom-ng runs as a sidecar next to the Zotero source management
// on the same host, so default addresses target the local Zotero local API,
// local Postgres+pgvector and local OpenSearch.
package config

import (
	"os"
	"strconv"
)

// Config holds the addresses and knobs the orchestration layer needs.
type Config struct {
	// ZoteroBaseURL is the base of the Zotero local JSON API.
	ZoteroBaseURL string
	// ZoteroLibraryID is the library prefix, e.g. users/0 for the local user.
	ZoteroLibraryID string

	// DatabaseURL is the Postgres+pgvector DSN.
	DatabaseURL string
	// OpenSearchURL is the OpenSearch endpoint.
	OpenSearchURL string

	// ProcessorURL is the base URL of the document processor sidecar.
	ProcessorURL string

	// APIPort is the port the axiom-ng REST API listens on.
	APIPort int
	// BindAddr is the interface the API binds to. Defaults to loopback so a
	// LAN client cannot reach the unauthenticated sync/job endpoints unless
	// explicitly overridden.
	BindAddr string
}

// defaults for a local sidecar setup.
const (
	defaultZoteroBase = "http://localhost:23119/api"
	defaultLibraryID  = "users/0"
	defaultAPIPort    = 8011
	defaultBindAddr   = "127.0.0.1"
)

// Load reads configuration from the environment, applying local sidecar
// defaults where a value is absent.
func Load() Config {
	return Config{
		ZoteroBaseURL:   env("AXIOMNG_ZOTERO_BASE", defaultZoteroBase),
		ZoteroLibraryID: env("AXIOMNG_ZOTERO_LIBRARY", defaultLibraryID),
		DatabaseURL:     env("AXIOMNG_DATABASE_URL", ""),
		OpenSearchURL:   env("AXIOMNG_OPENSEARCH_URL", "http://localhost:9200"),
		ProcessorURL:    env("AXIOMNG_PROCESSOR_URL", "http://localhost:8012"),
		APIPort:         envInt("AXIOMNG_API_PORT", defaultAPIPort),
		BindAddr:        env("AXIOMNG_BIND_ADDR", defaultBindAddr),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
