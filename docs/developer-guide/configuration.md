# Configuration

Every axiom knob is read from an `AXIOM_*` environment variable at startup.
This page is the **single, machine-maintainable reference** for all of them.
The two code bases each read their own set — the Go orchestrator
(`axiom_ng`) and the Python runner (`axiom_ng_runner`) — so the table is
organized by *where the variable is consumed* (`set by`).

> **Single source:** this table is meant to be regenerated from code. Each
> variable's name, default, and consumer live in exactly one place in the source
> (`axiom_ng/internal/config/config.go` for the Go set,
> `axiom_ng_runner/config.py` for the Python set). A completeness grep against
> those two files is the DoD check for this page — nothing here should exist
> without a code backing, and no code variable should be missing.

## Conventions

- `set by` says which process reads the variable: **Go** = the dispatcher
  (`axiom_ng`), **Runner** = the processor (`axiom_ng_runner`).
- `Default` shows the value applied when the variable is *unset* on a local
  sidecar setup.
- A few pairs look alike but mean different things — those are called out under
  [Near-miss pairs](#near-miss-pairs).

## Go — the orchestrator (`axiom_ng`)

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_ZOTERO_BASE` | `http://localhost:23119/api` | The Zotero local JSON API base. |
| `AXIOM_ZOTERO_LIBRARY` | `users/0` | Library prefix (local user's library). |
| `AXIOM_DATABASE_URL` | — | PostgreSQL + pgvector DSN. |
| `AXIOM_OPENSEARCH_URL` | `http://127.0.0.1:9200` | OpenSearch endpoint. **Explicitly set to empty disables** the outbox drainer (rows stay pending, no error). |
| `AXIOM_OPENSEARCH_USERNAME` | — | Optional basic-auth user for the outbox drainer; empty = anonymous. |
| `AXIOM_OPENSEARCH_PASSWORD` | — | Optional basic-auth password. |
| `AXIOM_PROCESSOR_SOURCE_SECRET` | — | Shared HMAC secret for remote source delivery (dispatcher signs, `/api/processor/source` verifies). Empty disables the feature on both sides. |
| `AXIOM_PROCESSOR_SOURCE_BASE_URL` | `http://127.0.0.1:<APIPort>` | Externally reachable base URL remote processors use to pull sources. `<APIPort>` resolves to the configured `AXIOM_API_PORT` (8011 by default); the URL defaults to loopback (co-located runners). |
| `AXIOM_PROCESSOR_URLS` | — | Ordered ingest-runner candidate list, comma-separated, preference order (#207). When set it defines the COMPLETE chain and wins over both legacy variables. A periodic health probe (`AXIOM_RUNNER_HEALTH_INTERVAL`) keeps dead candidates out of the submit path; submit-time failover (transport/5xx → next candidate, 4xx → error) stays as the safety net. |
| `AXIOM_RUNNER_HEALTH_INTERVAL` | `60s` | Interval of the ingest-candidate health probe (#207). `<=0` disables the background probe (startup remains best-effort) — a candidate demoted by submit-time failover is then only restored by a successful submit on it, so a preferred runner that recovered is not asked first again until restart. |
| `AXIOM_PROCESSOR_URL` | `http://localhost:8012` | Primary ingest-role processor URL (legacy — still read; see `AXIOM_PROCESSOR_URLS` for the ordered list). |
| `AXIOM_INGEST_FALLBACK_URL` | `http://localhost:8012` | Emergency ingest runner when `AXIOM_PROCESSOR_URL` is unreachable (legacy failover pair; folded into the candidate list only when `AXIOM_PROCESSOR_URLS` is unset). |
| `AXIOM_QUERY_RUNNER_URL` | `http://localhost:8012` | Query-role runner for `/v1/embed` + `/v1/rerank` (R4). Defaults to the local runner so retrieval survives a remote outage. |
| `AXIOM_PROCESSOR_TIMEOUT` | `300s` | Bounds the **result** fetch and (as the submit floor) the synchronous remote source download inside `POST /v1/process`. Remote deployments raise it to cover the runner's download budget. |
| `AXIOM_PROCESSOR_RUNNER_NAME` | processor-URL host | Human identity of the processor this dispatcher drives; lands in the phase log line and `ingest_jobs.runner_name` at claim time. |
| `AXIOM_DISPATCHER_ENABLED` | off | Gates the claim/process dispatcher loop; it never runs unless explicitly `1|true|yes`. |
| `AXIOM_DISPATCHER_WORKER_ID` | `axiom-ng` | This process's stable worker identity for leases (literal code default). Left at default, two dispatchers share one identity — set it per process when running multiple. |
| `AXIOM_DISPATCHER_CONCURRENCY` | `1` | Parallel claim/process slots. |
| `AXIOM_DISPATCHER_PROFILE` | `full-rag-v1` | Processing profile JSON frozen at claim time; the `full-rag-v1` default materializes **every** feature boolean as `true` (entities, relationships, dense + sparse embeddings, images). The profile *name* alone does not toggle features — the explicit booleans do. |
| `AXIOM_DISPATCHER_LEASE` | `5m` | Per-claim lease length. |
| `AXIOM_ARTIFACT_ROOT` | — | Durable derived-artifact root. |
| `AXIOM_API_PORT` | `8011` | Port the `axiom_ng` REST API listens on. |
| `AXIOM_ALLOW_DEBUG_BIND` | off | Explicit opt-out that lets a **debug** build bind a production port (8011, 8013–8015). Release builds always bind; unset/wrong values keep the guard active. |
| `AXIOM_BIND_ADDR` | `127.0.0.1` | Interface the API binds to. Loopback default keeps the unauthenticated sync/job endpoints off the LAN. |
| `AXIOM_SEARCH_SPARSE_ARM` | off | Enables the **sparse** recall arm (`rank_feature` clauses) on `POST /api/search`. Default off per the retrieval quality benchmark (no quality gain, +~1.3 s p95 local). |
| `AXIOM_SEARCH_GRAPH_ARM` | off | Enables the knowledge-**graph** expansion arm on `POST /api/search`. Default off — the quality benchmark measured it as slightly negative (+ high latency). |
| `AXIOM_SEARCH_RERANK` | on | Runs the cross-encoder **reranker** on `POST /api/search`. Set `false` for the latency-only profile; rerank latency is steerable via a remote runner / overfetch. |
| `AXIOM_SEARCH_FRONTMATTER_FILTER` | on | Removes detected TOC, preface, and reference chunks from search candidates before reranking. |
| `AXIOM_SEARCH_MAX_PER_BOOK` | `2` | Caps final hits per document with rank-order refill; `0` disables the cap. |
| `AXIOM_ZOTERO_WRITE_KEY_FILE` | `~/.axiom-ng/write-api-key` | Local Zotero write-key file. A missing or too-short key keeps the repair API unregistered. |
| `AXIOM_QUARANTINE_ROOT` | `~/.axiom-ng/quarantine` | Durable quarantine root for originals before repair mutations; falls back to `/tmp/axiom_quarantine` when no home directory resolves. |

## Runner — the processor (`axiom_ng_runner`)

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_PROCESSOR_BIND_ADDR` | `127.0.0.1` | Runner bind address. `0.0.0.0` for remote access. |
| `AXIOM_PROCESSOR_PORT` | `8537` | Runner HTTP port. |
| `AXIOM_PROCESSOR_WORK_ROOT` | `/tmp/axiom_processor_work` | Temporary job state. |
| `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` | — | Local host paths the runner may read. Empty = local source delivery is impossible. |
| `AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS` | `1` | Max parallel processing jobs (≥1). Marker+models are VRAM-heavy. |
| `AXIOM_PROCESSOR_RESULT_RETENTION` | `3600` | Seconds before unacknowledged results expire. |
| `AXIOM_PROCESSOR_COMPUTE` | `reference` | `reference` or `real` (GPU/ML pipeline). |
| `AXIOM_PROCESSOR_LOG_LEVEL` | `INFO` | Runner log level. |
| `AXIOM_PROCESSOR_SOURCE_TIMEOUT` | `120` | Runner-side source-download budget (seconds) for one `source_url` pull. |
| `AXIOM_PROCESSOR_MAX_QUERY_TEXTS` | `16` | Hard cap for `/v1/embed` batch size. |
| `AXIOM_PROCESSOR_RERANK_MAX_TEXTS` | `64` | Hard cap for `/v1/rerank` candidate count. |

> The runner reads its variables under a shared processor prefix in
> `config.py`; each is listed above by its full name.

## Near-miss pairs

These look almost identical but belong to **different processes / scopes**.
Confusing them is the most common config error:

| Pair | Owned by | Scoped to |
| --- | --- | --- |
| `AXIOM_PROCESSOR_TIMEOUT` | Go (dispatcher) | **Result-fetch** budget, and as a floor, the submit call's **synchronous source download**. |
| `AXIOM_PROCESSOR_SOURCE_TIMEOUT` | Runner | Runner-side **source_download** budget for pulling one `source_url` (default 120s). |
| `AXIOM_PROCESSOR_URLS` | Go (dispatcher) | **Ordered ingest chain** (#207): comma-separated, preference order; plural wins over the legacy pair below. |
| `AXIOM_PROCESSOR_URL` | Go (dispatcher) | **Primary ingest** runner (role: processing) — legacy singular; becomes the head of the candidate list when the plural is unset. |
| `AXIOM_INGEST_FALLBACK_URL` | Go (dispatcher) | **Emergency ingest** runner — legacy; appended as last candidate when distinct. |
| `AXIOM_QUERY_RUNNER_URL` | Go (dispatcher) | **Query** runner (role: embed/rerank for search). |

> **Precedence (#207):** `AXIOM_PROCESSOR_URLS` (complete chain, wins over
> everything) → `AXIOM_PROCESSOR_URL` + `AXIOM_INGEST_FALLBACK_URL` (legacy
> pair, folded into a two-entry chain) → default local runner. The singular
> variables are deliberately still read — no deprecation yet; a future
> version may deprecate them consciously.
>
> `AXIOM_PROCESSOR_URL` / `AXIOM_QUERY_RUNNER_URL` / `AXIOM_INGEST_FALLBACK_URL`
> all default to `http://localhost:8012` (the local always-on runner). The
> *role* they fill is what differs, not the address.
>
> **Why 8012 and not 8537?** The Go-side URLs point at the conventional
> **always-on** local sidecar port (`8012`), while the runner's own
> `AXIOM_PROCESSOR_PORT` defaults to `8537` — the dev/direct-start port. A
> dispatcher-driven deployment keeps a runner pinned on 8012; a
> manually-started runner listens on 8537 unless told otherwise.

## Machine-maintainability note

This table is deliberately shaped to be **recomputed from code**: a tool or a
CI step can diff `config.go`'s `Load()` and the runner package's
`load_settings()` — across the files that read them, e.g.
`config.py` **and** `axiom_ng_runner/__init__.py` (where
`AXIOM_PROCESSOR_COMPUTE` is re-read) — against this table and flag (a) a code
variable missing here, or (b) a table row without a code backing. The grep
targets the package(s), not a single file.

Next: [Testing](testing.md) · [Architecture Overview](architecture.md)
