// Package config loads axiom-ng runtime configuration from environment
// variables. axiom-ng runs as a sidecar next to the Zotero source management
// on the same host, so default addresses target the local Zotero local API,
// local Postgres+pgvector and local OpenSearch.
package config

import (
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
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

	// WSSecret (#168 B2) is the optional shared token for the /api/ws live
	// endpoint. Empty = loopback-only (no cross-network WS), the house rule;
	// on a non-loopback bind a configured token is required on the handshake.
	WSSecret string

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

	// ProcessorURLs is the ORDERED ingest-runner candidate list (#207):
	// AXIOM_PROCESSOR_URLS, comma-separated, preference order. Precedence:
	// the plural list wins and defines the COMPLETE chain when set (operator
	// spelled out their runners); otherwise the singular ProcessorURL becomes
	// the list head and the legacy IngestFallbackURL is appended as last
	// candidate when set and distinct (backward compatibility — single-entry
	// setups behave exactly as before). See IngestCandidates.
	ProcessorURLs []string
	// RunnerHealthInterval bounds the periodic candidate health probe that
	// keeps dead runners out of the submit path (AXIOM_RUNNER_HEALTH_INTERVAL,
	// Go duration; default 60s, <=0 disables the background probe). Startup
	// probing is best-effort and never blocks the RAG.
	RunnerHealthInterval time.Duration

	// SearchSparseArm enables the learned-lexical (rank_features) recall
	// arm on POST /api/search (R5 #135). Default OFF per the R7 benchmark:
	// no quality gain on the gold suite (MRR -0.027) at +~1.3s p95 on the
	// committed run (RETRIEVAL_BENCHMARK.md: 7.18s -> 8.49s; the 64-clause
	// rank_feature bool-should is expensive on this index; tuning lever:
	// sparseTopK, index shards). Re-enable for rare-token workloads
	// (Normnummern, Akronym-Queries) after tuning.
	SearchSparseArm bool

	// SearchGraphArm enables the graph-expansion candidate source on
	// POST /api/search (R6 #136). Default OFF — R7 measures first.
	SearchGraphArm bool

	// SearchRerank runs the cross-encoder on /api/search (default true;
	// AXIOM_SEARCH_RERANK=false is the latency-only profile, R7 matrix).
	SearchRerank bool
	// SearchFrontmatterFilter drops detected TOC/preface/references chunks
	// from the candidate pool before rerank (#160; default true — the
	// benchmark verdict wants them gone; false is the matrix/degradation
	// lever).
	SearchFrontmatterFilter bool
	// SearchMaxPerBook caps chunks per document in the final ranking with
	// rank-order refill (#160; default 2, 0 disables).
	SearchMaxPerBook int

	// DispatcherEnabled gates the claim/process dispatcher loop. It never runs
	// unless explicitly turned on; tests construct the dispatcher directly.
	DispatcherEnabled bool
	// DispatcherWorkerID is this process's stable worker identity for leases.
	DispatcherWorkerID string
	// DispatcherConcurrency caps parallel claim/process slots. 0 (or unset)
	// DERIVES the slot count from the Σ live runner capacities (#248:
	// local-first default — lanes follow runner reality); an explicit value
	// is clamped to that Σ.
	DispatcherConcurrency int
	// DispatcherProfile is the processing profile JSON frozen at claim time.
	DispatcherProfile string
	// DispatcherLeaseDuration is the per-claim lease length.
	DispatcherLeaseDuration time.Duration
	// DispatcherPreflightEnabled (#175): when set, the dispatcher sends a
	// claimed job's source PDF to the runner's /v1/pdf/preflight before full
	// processing; a quality-red gate skips the job and marks the attachment
	// as a repair-case candidate.
	DispatcherPreflightEnabled bool

	// FixerInvokerEnabled gates the mail-ingest fixer caller (#206): polls
	// the repair queue and invokes the fixer wrapper once per attachment
	// key. Opt-in like the dispatcher — never runs unless explicitly on.
	FixerInvokerEnabled bool
	// FixerCommand is the fixer wrapper (Command <key> --apply).
	FixerCommand string
	// FixerConcurrency caps parallel fixer runs per host (owner nail: 1-2).
	FixerConcurrency int

	// ArtifactRoot is the durable derived-artifact root (AXIOM_ARTIFACT_ROOT).
	ArtifactRoot string

	// ZoteroWriteKeyFile holds the local-API write key (#184). The key NEVER
	// lives in the repo; missing file = repair API disabled.
	ZoteroWriteKeyFile string
	// QuarantineRoot is the RAG-managed quarantine folder for broken
	// originals (#184 design nail).
	QuarantineRoot string

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
	// Quarantine originals are the audit + rollback basis of #184 — the
	// default root must be DURABLE user state, not volatile /tmp (review W2).
	// /tmp stays the degradation path when no home dir is resolvable.
	quarantineDefault := "/tmp/axiom_quarantine"
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		quarantineDefault = home + "/.axiom-ng/quarantine"
	}
	cfg := Config{
		ZoteroBaseURL:              env("AXIOM_ZOTERO_BASE", defaultZoteroBase),
		ZoteroLibraryID:            env("AXIOM_ZOTERO_LIBRARY", defaultLibraryID),
		DatabaseURL:                env("AXIOM_DATABASE_URL", ""),
		OpenSearchURL:              envEmptyDisables("AXIOM_OPENSEARCH_URL", "http://127.0.0.1:9200"),
		OpenSearchUsername:         env("AXIOM_OPENSEARCH_USERNAME", ""),
		OpenSearchPassword:         env("AXIOM_OPENSEARCH_PASSWORD", ""),
		ProcessorSourceSecret:      env("AXIOM_PROCESSOR_SOURCE_SECRET", ""),
		WSSecret:                   env("AXIOM_WS_SECRET", ""),
		ProcessorSourceBaseURL:     env("AXIOM_PROCESSOR_SOURCE_BASE_URL", ""),
		ProcessorURL:               env("AXIOM_PROCESSOR_URL", defaultLocalRunner),
		QueryRunnerURL:             env("AXIOM_QUERY_RUNNER_URL", defaultLocalRunner),
		IngestFallbackURL:          env("AXIOM_INGEST_FALLBACK_URL", defaultLocalRunner),
		ProcessorURLs:              parseURLList(env("AXIOM_PROCESSOR_URLS", "")),
		RunnerHealthInterval:       envDur("AXIOM_RUNNER_HEALTH_INTERVAL", 60*time.Second),
		SearchSparseArm:            envBoolDefault("AXIOM_SEARCH_SPARSE_ARM", false),
		SearchGraphArm:             envBoolDefault("AXIOM_SEARCH_GRAPH_ARM", false),
		SearchRerank:               envBoolDefault("AXIOM_SEARCH_RERANK", true),
		SearchFrontmatterFilter:    envBoolDefault("AXIOM_SEARCH_FRONTMATTER_FILTER", true),
		SearchMaxPerBook:           envInt("AXIOM_SEARCH_MAX_PER_BOOK", 2),
		ProcessorRequestTimeout:    envDur("AXIOM_PROCESSOR_TIMEOUT", 300*time.Second),
		ProcessorRunnerName:        env("AXIOM_PROCESSOR_RUNNER_NAME", ""),
		DispatcherEnabled:          envBool("AXIOM_DISPATCHER_ENABLED"),
		DispatcherWorkerID:         env("AXIOM_DISPATCHER_WORKER_ID", "axiom-ng"),
		DispatcherConcurrency:      envInt("AXIOM_DISPATCHER_CONCURRENCY", 0), // 0 = derive from Σ live runner capacities (#248)
		DispatcherProfile:          env("AXIOM_DISPATCHER_PROFILE", defaultProfile),
		DispatcherLeaseDuration:    envDur("AXIOM_DISPATCHER_LEASE", 5*time.Minute),
		DispatcherPreflightEnabled: envBool("AXIOM_DISPATCHER_PREFLIGHT"),
		FixerInvokerEnabled:        envBool("AXIOM_FIXER_INVOKER_ENABLED"),
		FixerCommand:               env("AXIOM_FIXER_CMD", "/opt/axiom/bin/axiom-fixer"),
		FixerConcurrency:           envInt("AXIOM_FIXER_CONCURRENCY", 1),
		ArtifactRoot:               env("AXIOM_ARTIFACT_ROOT", ""),
		ZoteroWriteKeyFile:         env("AXIOM_ZOTERO_WRITE_KEY_FILE", os.Getenv("HOME")+"/.axiom-ng/write-api-key"),
		QuarantineRoot:             env("AXIOM_QUARANTINE_ROOT", quarantineDefault),
		APIPort:                    envInt("AXIOM_API_PORT", defaultAPIPort),
		BindAddr:                   env("AXIOM_BIND_ADDR", defaultBindAddr),
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

// parseURLList splits a comma-separated URL list, trimming whitespace and
// trailing slashes per entry (#207). Empty entries are dropped.
func parseURLList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimRight(strings.TrimSpace(part), "/")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// IngestCandidates returns the ordered ingest-runner candidate list
// (#207): if the plural AXIOM_PROCESSOR_URLS list is set it defines the
// COMPLETE chain (plural wins over both legacy variables — the operator
// spelling out a list knows their runners); otherwise the singular
// ProcessorURL becomes the list head and the legacy IngestFallbackURL is
// appended when set and distinct. Never returns empty — the local-runner
// default is the floor.
func (c Config) IngestCandidates() []string {
	// list is already normalized (trailing slashes stripped) by the load-time
	// parseURLList; a fresh copy avoids mutating the source slice.
	list := slices.Clone(c.ProcessorURLs)
	if len(list) == 0 {
		list = parseURLList(c.ProcessorURL)
		if fb := strings.TrimRight(strings.TrimSpace(c.IngestFallbackURL), "/"); fb != "" && !slices.Contains(list, fb) {
			list = append(list, fb)
		}
	}
	if len(list) == 0 {
		list = []string{strings.TrimRight(defaultLocalRunner, "/")}
	}
	return list
}

// envEmptyDisables treats an explicitly SET-but-empty value as intentional
// (returns "" — disabled) and only falls back when the variable is UNSET.
func envEmptyDisables(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// envBool reads a bool env defaulting to off — case-insensitive everywhere
// (envBool used to accept only lowercase "true"/"yes", so
// AXIOM_DISPATCHER_ENABLED=TRUE silently meant off).
func envBool(key string) bool { return envBoolDefault(key, false) }

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

// envBoolDefault reads a bool env with an explicit default (for flags that
// default ON — envBool's zero default reads as "off").
func envBoolDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
