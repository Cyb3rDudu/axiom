// Package config loads axiom-ng runtime configuration from environment
// variables. axiom-ng runs as a sidecar next to the Zotero source management
// on the same host, so default addresses target the local Zotero local API,
// local Postgres+pgvector and local OpenSearch.
package config

import (
	"os"
	"strconv"
	"time"
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

	// DispatcherEnabled gates the claim/process dispatcher loop. It never runs
	// unless explicitly turned on; tests construct the dispatcher directly.
	DispatcherEnabled bool
	// DispatcherWorkerID is this process's stable worker identity for leases.
	DispatcherWorkerID string
	// DispatcherConcurrency caps parallel claim/process slots.
	DispatcherConcurrency int
	// DispatcherProfile is the processing profile JSON frozen at claim time.
	DispatcherProfile string
	// DispatcherLeaseDuration is the per-claim lease length.
	DispatcherLeaseDuration time.Duration

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
	defaultProfile    = `{"profile":"full-rag-v1"}`
)

// Load reads configuration from the environment, applying local sidecar
// defaults where a value is absent.
func Load() Config {
	return Config{
		ZoteroBaseURL:           env("AXIOMNG_ZOTERO_BASE", defaultZoteroBase),
		ZoteroLibraryID:         env("AXIOMNG_ZOTERO_LIBRARY", defaultLibraryID),
		DatabaseURL:             env("AXIOMNG_DATABASE_URL", ""),
		OpenSearchURL:           env("AXIOMNG_OPENSEARCH_URL", "http://localhost:9200"),
		ProcessorURL:            env("AXIOMNG_PROCESSOR_URL", "http://localhost:8012"),
		DispatcherEnabled:       envBool("AXIOMNG_DISPATCHER_ENABLED"),
		DispatcherWorkerID:      env("AXIOMNG_DISPATCHER_WORKER_ID", "axiom-ng"),
		DispatcherConcurrency:   envInt("AXIOMNG_DISPATCHER_CONCURRENCY", 1),
		DispatcherProfile:       env("AXIOMNG_DISPATCHER_PROFILE", defaultProfile),
		DispatcherLeaseDuration: envDur("AXIOMNG_DISPATCHER_LEASE", 5*time.Minute),
		APIPort:                 envInt("AXIOMNG_API_PORT", defaultAPIPort),
		BindAddr:                env("AXIOMNG_BIND_ADDR", defaultBindAddr),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
