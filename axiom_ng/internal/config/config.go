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
	// OpenSearchURL is the OpenSearch endpoint. Unset defaults to the local
	// mothership; explicitly SET to an empty string disables the L5 outbox
	// drainer (rows stay pending, no error).
	OpenSearchURL string
	// OpenSearchUsername/OpenSearchPassword are optional basic-auth
	// credentials for the outbox drainer; empty means anonymous.
	OpenSearchUsername string
	OpenSearchPassword string

	// ProcessorSourceSecret is the shared HMAC secret for remote source
	// delivery (dispatcher signs, /api/processor/source verifies). Empty
	// disables the feature on BOTH sides (endpoint 404s, no source_url sent).
	ProcessorSourceSecret string
	// ProcessorSourceBaseURL is the externally reachable base URL of
	// axiom-ng that remote processors use to pull sources (Tailnet/LAN).
	// Unset defaults to the loopback API — fine for co-located runners.
	ProcessorSourceBaseURL string

	// ProcessorURL is the base URL of the document processor sidecar.
	ProcessorURL string
	// ProcessorRequestTimeout bounds the RESULT fetch and (as the submit
	// floor) the synchronous remote source download inside POST /v1/process
	// (AXIOMNG_PROCESSOR_TIMEOUT, Go duration). Remote deployments raise it
	// to cover the runner's download budget. All other call types have
	// fixed per-type budgets in the processor client.
	ProcessorRequestTimeout time.Duration

	// ProcessorRunnerName is the human identity of the processor this
	// dispatcher drives (AXIOMNG_PROCESSOR_RUNNER_NAME, e.g. "carrier-gpu0").
	// It lands in the phases log line and in ingest_jobs.processor_name at
	// claim time — the TC2 scale proof needs "which runner ran which book"
	// answerable from SQL. Empty defaults to the ProcessorURL host.
	ProcessorRunnerName string

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

	// ArtifactRoot is the durable derived-artifact root (AXIOMNG_ARTIFACT_ROOT).
	ArtifactRoot string

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
	// The profile NAME alone does not enable features — the runner reads the
	// explicit booleans (ProcessingOptions defaults are all false; benchmark
	// finding 2026-08-14). full-rag-v1 therefore materializes every feature.
	defaultProfile = `{"profile":"full-rag-v1","extract_entities":true,"extract_relationships":true,"compute_dense_embeddings":true,"compute_sparse_embeddings":true,"extract_images":true}`
)

// Load reads configuration from the environment, applying local sidecar
// defaults where a value is absent.
func Load() Config {
	cfg := Config{
		ZoteroBaseURL:           env("AXIOMNG_ZOTERO_BASE", defaultZoteroBase),
		ZoteroLibraryID:         env("AXIOMNG_ZOTERO_LIBRARY", defaultLibraryID),
		DatabaseURL:             env("AXIOMNG_DATABASE_URL", ""),
		OpenSearchURL:           envEmptyDisables("AXIOMNG_OPENSEARCH_URL", "http://127.0.0.1:9200"),
		OpenSearchUsername:      env("AXIOMNG_OPENSEARCH_USERNAME", ""),
		OpenSearchPassword:      env("AXIOMNG_OPENSEARCH_PASSWORD", ""),
		ProcessorSourceSecret:   env("AXIOMNG_PROCESSOR_SOURCE_SECRET", ""),
		ProcessorSourceBaseURL:  env("AXIOMNG_PROCESSOR_SOURCE_BASE_URL", ""),
		ProcessorURL:            env("AXIOMNG_PROCESSOR_URL", "http://localhost:8012"),
		ProcessorRequestTimeout: envDur("AXIOMNG_PROCESSOR_TIMEOUT", 300*time.Second),
		ProcessorRunnerName:     env("AXIOMNG_PROCESSOR_RUNNER_NAME", ""),
		DispatcherEnabled:       envBool("AXIOMNG_DISPATCHER_ENABLED"),
		DispatcherWorkerID:      env("AXIOMNG_DISPATCHER_WORKER_ID", "axiom-ng"),
		DispatcherConcurrency:   envInt("AXIOMNG_DISPATCHER_CONCURRENCY", 1),
		DispatcherProfile:       env("AXIOMNG_DISPATCHER_PROFILE", defaultProfile),
		DispatcherLeaseDuration: envDur("AXIOMNG_DISPATCHER_LEASE", 5*time.Minute),
		ArtifactRoot:            env("AXIOMNG_ARTIFACT_ROOT", ""),
		APIPort:                 envInt("AXIOMNG_API_PORT", defaultAPIPort),
		BindAddr:                env("AXIOMNG_BIND_ADDR", defaultBindAddr),
	}
	// The source-endpoint base defaults to the local API port (co-located
	// runners); remote deployments override with their Tailnet/LAN address.
	if cfg.ProcessorSourceBaseURL == "" {
		cfg.ProcessorSourceBaseURL = "http://127.0.0.1:" + strconv.Itoa(cfg.APIPort)
	}
	return cfg
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envEmptyDisables treats an explicitly SET-but-empty value as intentional
// (returns "" — disabled) and only falls back when the variable is UNSET.
func envEmptyDisables(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
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
