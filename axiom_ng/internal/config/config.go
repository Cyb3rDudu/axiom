// Package config loads axiom-ng runtime configuration from environment
// variables. axiom-ng runs as a sidecar next to the Zotero source management
// on the same host, so default addresses target the local Zotero local API,
// local Postgres+pgvector and local OpenSearch.
package config

import (
	"net/url"
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
	// (AXIOM_PROCESSOR_TIMEOUT, Go duration). Remote deployments raise it
	// to cover the runner's download budget. All other call types have
	// fixed per-type budgets in the processor client.
	ProcessorRequestTimeout time.Duration

	// ProcessorRunnerName is the human identity of the processor this
	// dispatcher drives (AXIOM_PROCESSOR_RUNNER_NAME, e.g. "carrier-gpu0").
	// It lands in the phases log line and in ingest_jobs.runner_name at
	// claim time — the TC2 scale proof needs "which runner ran which book"
	// answerable from SQL. Empty defaults to the ProcessorURL host.
	ProcessorRunnerName string

	// QueryRunnerURL is the query-side runner (R4 #134): embedding + rerank
	// for POST /api/search. Defaults to the LOCAL always-on runner — the
	// role model guarantees retrieval survives a Carrier outage. Override
	// (AXIOM_QUERY_RUNNER_URL) points retrieval compute at a dedicated
	// runner without code changes.
	QueryRunnerURL string
	// IngestFallbackURL is the emergency ingest runner used when
	// ProcessorURL is unreachable (transport error or 5xx). Defaults to the
	// local runner (#128 proof: complete, ~11x slower). Failover is logged.
	IngestFallbackURL string

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

	// ArtifactRoot is the durable derived-artifact root (AXIOM_ARTIFACT_ROOT).
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
	// defaultLocalRunner is the local always-on processor port (R4 roles:
	// query default AND ingest fallback).
	defaultLocalRunner = "http://localhost:8012"
	defaultLibraryID   = "users/0"
	defaultAPIPort     = 8011
	defaultBindAddr    = "127.0.0.1"
	// The profile NAME alone does not enable features — the runner reads the
	// explicit booleans (ProcessingOptions defaults are all false; benchmark
	// finding 2026-08-14). full-rag-v1 therefore materializes every feature.
	defaultProfile = `{"profile":"full-rag-v1","extract_entities":true,"extract_relationships":true,"compute_dense_embeddings":true,"compute_sparse_embeddings":true,"extract_images":true}`
)

// Load reads configuration from the environment, applying local sidecar
// defaults where a value is absent.
func Load() Config {
	cfg := Config{
		ZoteroBaseURL:           env("AXIOM_ZOTERO_BASE", defaultZoteroBase),
		ZoteroLibraryID:         env("AXIOM_ZOTERO_LIBRARY", defaultLibraryID),
		DatabaseURL:             env("AXIOM_DATABASE_URL", ""),
		OpenSearchURL:           envEmptyDisables("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200"),
		OpenSearchUsername:      env("AXIOM_OPENSEARCH_USERNAME", ""),
		OpenSearchPassword:      env("AXIOM_OPENSEARCH_PASSWORD", ""),
		ProcessorSourceSecret:   env("AXIOM_PROCESSOR_SOURCE_SECRET", ""),
		ProcessorSourceBaseURL:  env("AXIOM_PROCESSOR_SOURCE_BASE_URL", ""),
		ProcessorURL:            env("AXIOM_PROCESSOR_URL", "http://localhost:8012"),
		QueryRunnerURL:          env("AXIOM_QUERY_RUNNER_URL", defaultLocalRunner),
		IngestFallbackURL:       env("AXIOM_INGEST_FALLBACK_URL", defaultLocalRunner),
		ProcessorRequestTimeout: envDur("AXIOM_PROCESSOR_TIMEOUT", 300*time.Second),
		ProcessorRunnerName:     env("AXIOM_PROCESSOR_RUNNER_NAME", ""),
		DispatcherEnabled:       envBool("AXIOM_DISPATCHER_ENABLED"),
		DispatcherWorkerID:      env("AXIOM_DISPATCHER_WORKER_ID", "axiom-ng"),
		DispatcherConcurrency:   envInt("AXIOM_DISPATCHER_CONCURRENCY", 1),
		DispatcherProfile:       env("AXIOM_DISPATCHER_PROFILE", defaultProfile),
		DispatcherLeaseDuration: envDur("AXIOM_DISPATCHER_LEASE", 5*time.Minute),
		ArtifactRoot:            env("AXIOM_ARTIFACT_ROOT", ""),
		APIPort:                 envInt("AXIOM_API_PORT", defaultAPIPort),
		BindAddr:                env("AXIOM_BIND_ADDR", defaultBindAddr),
	}
	// The source-endpoint base defaults to the local API port (co-located
	// runners); remote deployments override with their Tailnet/LAN address.
	if cfg.ProcessorSourceBaseURL == "" {
		cfg.ProcessorSourceBaseURL = "http://127.0.0.1:" + strconv.Itoa(cfg.APIPort)
	}
	// Runner identity fallback (#122): an unset name derives from the
	// processor URL host so a bare single-runner deployment still gets a
	// usable identity in log+SQL. Explicit env always wins.
	if cfg.ProcessorRunnerName == "" {
		if u, err := url.Parse(cfg.ProcessorURL); err == nil && u.Host != "" {
			cfg.ProcessorRunnerName = u.Host
		}
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
