# Configuration

Every axiom knob is read from an `AXIOM_*` environment variable at startup —
and, since F13 (#307), optionally from the persistent runtime configuration
file `config.sqlite` under the state root. This page is the **single,
machine-maintainable reference** for all of them. The two code bases each
read their own set — the Go orchestrator (`axiom`) and the Python worker
(`axiom_compute_worker`) — so the table is organized by *where the variable
is consumed* (`set by`).

> **Single source:** this table is meant to be regenerated from code. Each
> variable's name, default, and consumer live in exactly one place in the source
> (`axiom/internal/config/config.go` for the Go set,
> `axiom-compute-worker/config.py` for the Python set). A completeness grep against
> those two files is the DoD check for this page — nothing here should exist
> without a code backing, and no code variable should be missing.

## The resolution chain (F13 #307)

The Go orchestrator's RUNTIME SURFACES — `serve` (every role and the compat boot), `doctor`, `config`, and the legacy KG-mode preambles — resolve the shared `AXIOM_*` vocabulary through one chain:

```
--set KEY=VALUE flag  >  environment variable  >  config.sqlite row  >  default
```

- Each stage overrides only what it sets; unset stages fall through.
- Scope: mode-specific knobs outside the shared vocabulary (e.g. `AXIOM_RETENTION_*`) and the debug-bind opt-out stay direct environment reads; the standalone backfill tool binaries under `cmd/` read the environment directly.
- **Dual-fed pairs resolve per field.** The legacy/canonical spelling
  pairs (`AXIOM_COMPUTE_WORKER_*` vs `AXIOM_PROCESSOR_*`,
  `AXIOM_REPAIR_WORKER_CMD` vs `AXIOM_FIXER_CMD`) are ONE knob each: the
  chain binds the logical knob — a `config.sqlite` row on either
  spelling never out-ranks an environment value on the other, and a
  `--set` on either spelling wins the whole field. Spelling precedence
  (canonical over legacy) breaks ties *within* one stage only; when
  `--set` names both spellings, the canonical-named flag wins.
- **Env-only is the fully supported path**: without a `config.sqlite`, every
  boot resolves exactly as before (the container story — no file needed, none
  created).
- The file layer and the flag layer apply with **environment semantics**: a
  value stored in the file behaves exactly as if the operator had exported it
  (same parsers, same dual-fed canonical/legacy rules, same deprecation
  witnesses on legacy spellings).
- The file and flag surfaces refuse values carrying inline credentials
  (DSN/URL userinfo-with-password, credential query or keyword parameters) —
  and by design also **free-text values containing a credential-shaped
  fragment** (e.g. a filter listing `password=…`): the rule is
  render-symmetric, whatever `config get --effective` would redact is never
  a legal stored form; such values keep riding the environment.
  The credential vocabulary is the keys the loaders honor
  (`password`/`sslpassword`/`passfile`); other query parameters
  (e.g. a bearer `?token=…`) are ordinary values — writable and
  rendered — until a loader ever treats one as a credential.
- One resolution per process: `axiom serve`, `doctor`, and `config` resolve
  once at entry; child processes inherit the resolved environment.
- The **KG legacy mode flags** do not read `--set`; they resolve through the
  chain without flags.

### `config.sqlite` — the persistent runtime configuration

One small SQLite file per host installation (default `~/.axiom/config.sqlite`,
override with `AXIOM_CONFIG_PATH`), created atomically on first write
(`config set` / `config import-env`), WAL journal, restrictive permissions.

| Table | Content |
| --- | --- |
| `settings` | Non-secret overrides. Key = the canonical `AXIOM_*` name (one vocabulary across env, file, and `--set`); value in its exact environment spelling. |
| `secret_refs` | Secret **references**, never values: a row records that a secret key's value is expected from the process environment (`source=env`). The value itself stays in the OS secret store / environment. |
| `config_schema_migrations` | The version ledger (own migrations, embedded in the binary). |

Hard rules with teeth:

- **Runtime configuration only — never domain data.** The store refuses files
  carrying any table outside the vocabulary above (a Library/Store schema
  smuggled in, or `AXIOM_CONFIG_PATH` pointed at a domain database, fails
  loudly at read time).
- **No secret values, ever.** `config set` refuses secret keys; `import-env`
  stores secrets as references; the inspection tests prove zero secret bytes
  in the file, its WAL sidecar, and every output line.
- **No inline credentials on the file/flag surfaces.** `config set` and
  `--set` refuse URL values carrying userinfo (`scheme://user:pass@host`):
  the credential-free URL is the storable form, the credential rides the
  environment (or OS secret store) — the one surface where credentials
  legitimately live. `import-env` therefore also refuses to import such a
  value until the credential is stripped from it.
- A **present-but-invalid file** (unknown key, unparseable value, secret
  value) refuses startup with a diagnosis — never a silent fall-back to
  defaults.

### CLI surface

| Command | Effect |
| --- | --- |
| `axiom config get --effective [--json]` | One row per key: effective value (secrets redacted, DSN credential-free) and the source column `flag`/`env`/`file`/`default`. |
| `axiom config set KEY VALUE` | Write one override into `config.sqlite` — validated FIRST: unknown key, type violation, value range, vocabulary violation (e.g. an unknown storage driver), secret keys, and inline-credential URLs each exit **1** with the diagnosis and touch nothing. Argument-shape errors exit **2**. |
| `axiom config validate` | Full consistency pass: env values parse, file rows valid, secret references still have their env source, and the derived role set wires (composition check, network-free). Exit 0 only when everything holds. |
| `axiom config import-env` | One-shot import of the current environment into `config.sqlite`: non-secrets as `settings` rows, secrets as `secret_refs` (values never enter the file). Credential-carrying values (inline DSN/URL credentials — a legal environment shape) are SKIPPED loudly, left env-only. Everything else is validated first — a value the file surface would refuse (type violation) refuses the whole import, nothing written. The import lands in ONE transaction. Idempotent — a replay writes the same state. The effective configuration does not move: the environment still owns every imported key until it is cleared; the file row takes over then. |
| `axiom config unset <KEY>` | Removes one override or secret reference (idempotent; unknown keys refused like `set`). With no `config.sqlite` present it is a no-op success — nothing is created. The write subcommands (`set`, `unset`, `import-env`) take no `--set` (exit 2). |
| `axiom <command> --set KEY=VALUE …` | One-shot override riding `serve`, `doctor`, and `config` — the flag follows the subcommand and is repeatable (`axiom serve --set AXIOM_API_PORT=8012 all`); the CLI-flag stage of the chain. Same validation teeth as `config set`; secret keys and inline-credential URLs are refused (command lines are visible in history and `ps`). |

`axiom doctor` reports the file as its own `config-file` check: absent = ok
(the env-only bootstrap), present = shape summary, broken = fail with the
cfg-dependent checks skipped honestly.

## Conventions

- `set by` says which process reads the variable: **Go** = the dispatcher
  (`axiom_ng`), **Runner** = the processor (`axiom_compute_worker`).
- `Default` shows the value applied when the variable is *unset* on a local
  sidecar setup.
- A few pairs look alike but mean different things — those are called out under
  [Near-miss pairs](#near-miss-pairs).

## Go — the orchestrator (`axiom`)

Rows below lead with the canonical spelling; where a legacy alias still feeds
the same knob it is named in the row and tracked in the
[deprecation schedule](../operations/deprecations.md).

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_ZOTERO_BASE` | `http://localhost:23119/api` | The Zotero local JSON API base. |
| `AXIOM_ZOTERO_LIBRARY` | `users/0` | Library prefix (local user's library). |
| `AXIOM_DATABASE_URL` | — | PostgreSQL + pgvector DSN. |
| `AXIOM_OPENSEARCH_URL` | `http://127.0.0.1:9200` | OpenSearch endpoint. **Explicitly set to empty disables** the outbox drainer (rows stay pending, no error). |
| `AXIOM_OPENSEARCH_USERNAME` | — | Optional basic-auth user for the outbox drainer; empty = anonymous. |
| `AXIOM_OPENSEARCH_PASSWORD` | — | Optional basic-auth password. |
| `AXIOM_COMPUTE_WORKER_SOURCE_SECRET` | — | Shared HMAC secret for remote source delivery (dispatcher signs, `/api/processor/source` verifies). Empty disables the feature on both sides. Canonical spelling (F10 #304); legacy `AXIOM_PROCESSOR_SOURCE_SECRET` still feeds it (warn-once alias). |
| `AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL` | `http://127.0.0.1:<APIPort>` | Externally reachable base URL remote compute workers use to pull sources. Canonical spelling (F10 #304); legacy `AXIOM_PROCESSOR_SOURCE_BASE_URL` still feeds it (warn-once alias). `<APIPort>` resolves to the configured `AXIOM_API_PORT` (8011 by default); the URL defaults to loopback (co-located runners). |
| `AXIOM_COMPUTE_WORKER_URLS` | — | Ordered ingest-runner candidate list, comma-separated, preference order (#207). Canonical spelling (F10 #304); the legacy `AXIOM_PROCESSOR_URLS` below still feeds it — warned once and counted in `/api/health/deprecations`. When set it defines the COMPLETE chain and wins over both legacy variables. A periodic health probe (`AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL`) keeps dead candidates out of the submit path; submit-time failover (transport/5xx → next candidate, 4xx → error) stays as the safety net. |
| `AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL` | `60s` | Interval of the ingest-candidate health probe (#207). Canonical spelling (F10 #304); legacy `AXIOM_RUNNER_HEALTH_INTERVAL` still feeds it (warn-once alias). `<=0` disables the background probe (startup remains best-effort) — a candidate demoted by submit-time failover is then only restored by a successful submit on it, so a preferred runner that recovered is not asked first again until restart. |
| `AXIOM_COMPUTE_WORKER_URL` | `http://localhost:8012` | Primary ingest-role compute-worker URL (canonical, F10 #304). |
| `AXIOM_PROCESSOR_URL` | (alias) | **Legacy** alias of `AXIOM_COMPUTE_WORKER_URL` — still read through 0.2.x (warn-once, counted; see the [deprecation schedule](../operations/deprecations.md) and ADR 0001 §4). |
| `AXIOM_INGEST_FALLBACK_URL` | `http://localhost:8012` | Emergency ingest runner appended as the last candidate when the chain is built from the singular primary (legacy failover pair; folded into the candidate list only when `AXIOM_COMPUTE_WORKER_URLS` is unset — see precedence below). |
| `AXIOM_QUERY_RUNNER_URL` | `http://localhost:8012` | Query-role runner for `/v1/embed` + `/v1/rerank` (R4). Defaults to the local runner so retrieval survives a remote outage. |
| `AXIOM_COMPUTE_WORKER_TIMEOUT` | `300s` | Bounds the **result** fetch and (as the submit floor) the synchronous remote source download inside `POST /v1/process`. Canonical spelling (F10 #304); legacy `AXIOM_PROCESSOR_TIMEOUT` still feeds it (warn-once alias). Remote deployments raise it to cover the runner's download budget. |
| `AXIOM_COMPUTE_WORKER_NAME` | processor-URL host | Human identity of the compute worker this dispatcher drives; lands in the phase log line and `ingest_jobs.runner_name` at claim time. Canonical spelling (F10 #304); legacy `AXIOM_PROCESSOR_RUNNER_NAME` still feeds it (warn-once alias). |
| `AXIOM_DISPATCHER_ENABLED` | off | Gates the claim/process dispatcher loop; it never runs unless explicitly `1 | true | yes`. |
| `AXIOM_DISPATCHER_WORKER_ID` | `axiom` | This process's stable worker identity for leases (literal code default). Left at default, two dispatchers share one identity — set it per process when running multiple. |
| `AXIOM_DISPATCHER_CONCURRENCY` | `1` | Parallel claim/process slots. |
| `AXIOM_DISPATCHER_PROFILE` | `full-rag-v1` | Processing profile JSON frozen at claim time; the `full-rag-v1` default materializes **every** feature boolean as `true` (entities, relationships, dense + sparse embeddings, images). The profile *name* alone does not toggle features — the explicit booleans do. |
| `AXIOM_DISPATCHER_LEASE` | `5m` | Per-claim lease length. |
| `AXIOM_DISPATCHER_PREFLIGHT` | off | #175 quality gate at claim: sends the claimed source PDF to the runner's `/v1/pdf/preflight` BEFORE full processing. A red verdict (e.g. textless scan no matter the labels, label-Anomalie) skips the job (`skipped`, reason `preflight:<finding>`) and marks the attachment as a repair-case candidate (the #206/#203 fixer can heal it later) instead of producing junk chunks. Default off = jobs process as today. Per-call bound: the processor client's small-call budget (15s). |
| `AXIOM_ARTIFACT_ROOT` | — | Durable derived-artifact root. Library import content stages under `<root>/library_staging/<sha256>` (hashed files, no DB BLOBs). |
| `AXIOM_LIBRARY_IMPORT_MAX_BYTES` | `536870912` (512 MiB) | Caps one Library import's content bytes (F06 #300). Values above the in-code hard cap (2 GiB) clamp to it; `0`/unset = the 512 MiB service default. |
| `AXIOM_LIBRARY_IMPORT_PROVIDERS` | — | Selects the Library import write-side provider set (F06 #300). `fake` wires the deterministic fake providers (dev-env proof until F07 ports Zotero); unset leaves the `/api/v1/library/imports` routes unwired (404 — the honest no-provider state). |
| `AXIOM_STORAGE_LIBRARY_DRIVER` | `postgres` | Library persistence engine (F12 #306): `postgres` (own pool over `AXIOM_LIBRARY_DATABASE_URL`, defaulting to the core DSN) or `sqlite` (one `library.sqlite` file, single host — WAL/FK/busy-timeout asserted at start, 0600). `zotero` import providers require the PostgreSQL profile (the Zotero mirror lives on the shared database). |
| `AXIOM_LIBRARY_DATABASE_URL` | — | The Library component's own PostgreSQL DSN (F12 #306); unset = `AXIOM_DATABASE_URL`. A separate DSN unwires the legacy sync-lane revision Mits-Schreib honestly (no mirror reachable; revision intake is the successor). Redacted like the core DSN in `config get --effective`. |
| `AXIOM_LIBRARY_SQLITE_PATH` | `~/.axiom/library.sqlite` | The `library.sqlite` file for the SQLite profile (F12 #306). One file per component: never ATTACHed, never shared with the Store or config.sqlite. |
| `AXIOM_API_PORT` | `8011` | Port the `axiom` REST API listens on. |
| `AXIOM_ALLOW_DEBUG_BIND` | off | Explicit opt-out that lets a **debug** build bind a production port (8011, 8013–8015). Release builds always bind; unset/wrong values keep the guard active. |
| `AXIOM_BIND_ADDR` | `127.0.0.1` | Interface the API binds to. Loopback default keeps the unauthenticated sync/job endpoints off the LAN. |
| `AXIOM_SEARCH_SPARSE_ARM` | off | Enables the **sparse** recall arm (`rank_feature` clauses) on `POST /api/search`. Default off per the retrieval quality benchmark (no quality gain, +~1.3 s p95 local). |
| `AXIOM_SEARCH_GRAPH_ARM` | off | Enables the knowledge-**graph** expansion arm on `POST /api/search`. Default off — the quality benchmark measured it as slightly negative (+ high latency). |
| `AXIOM_SEARCH_RERANK` | on | Runs the cross-encoder **reranker** on `POST /api/search`. Set `false` for the latency-only profile; rerank latency is steerable via a remote runner / overfetch. |
| `AXIOM_SEARCH_FRONTMATTER_FILTER` | on | Removes detected TOC, preface, and reference chunks from search candidates before reranking. |
| `AXIOM_SEARCH_MAX_PER_BOOK` | `2` | Caps final hits per document with rank-order refill; `0` disables the cap. |
| `AXIOM_ZOTERO_WRITE_KEY_FILE` | `~/.axiom/write-api-key` | Local Zotero write-key file. A missing or too-short key keeps the repair API unregistered. |
| `AXIOM_QUARANTINE_ROOT` | `~/.axiom/quarantine` | Durable quarantine root for originals before repair mutations; falls back to `/tmp/axiom_quarantine` when no home directory resolves. |
| `AXIOM_CONTEXTUAL_COLLECTIONS` | — | (#255) Comma-separated collection paths (any depth, e.g. `VWL/Lectures,ORG/Lectures`) whose member documents are projected `citation_class: contextual` — searchable at full rank, never citable, KG-excluded. Empty CSV fields are ignored as formatting slack (a trailing comma is fine). Resolved at boot against the synced collections and stabilized on `zotero_key`; an **unknown path is a loud start error** — but only once the DB has sync state; on a never-synced DB the boot degrades instead of fataling (#262): rules stay inactive (everything citable), `/api/health` shows `contextual: degraded_no_sync`, and the first successful sync activates the rules without a restart. |
| `AXIOM_CONTEXTUAL_TAGS` | — | (#255) Comma-separated literal Zotero tag names that force a document contextual (the outlier lever next to the collection rule; a tag never forces citable). Boot-validated like the paths: a tag no active document carries is a loud start error (with the same #262 never-synced degradation as the paths). |

## Runner — the processor (`axiom_compute_worker`)

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
| `AXIOM_PROCESSOR_RELATIONSHIPS_BUDGET_SECONDS` | `900` | #225 wall-clock budget for the relationships stage (mREBEL over a full book is inherently slow on MPS, #179). On expiry the stage stops BETWEEN batches and the job completes HONESTLY with the early-committed chunks/embeddings/entities (`manifest.stage_completion.relationships_reason = STAGE_BUDGET_EXCEEDED`) — never an eternal lease. `0` disables the budget. |
| `AXIOM_PROCESSOR_EPUBCHECK_CMD` | auto | #220 EPUB preflight: external epubcheck command (BSD-3, JSON mode) for the `/v1/pdf/preflight` EPUB branch. Empty/auto = `epubcheck` on PATH; when absent the gate degrades honestly to the built-in light checks (zip/OPF/spine, DRM, text-layer) and reports `epubcheck: not_available`. Deliberately NOT bundled into the runner artifact — the conda-pack artifact must not gain a Java runtime (#208). |
| `AXIOM_PROCESSOR_RERANK_MAX_TEXTS` | `64` | Hard cap for `/v1/rerank` candidate count. |

> The runner reads its variables under a shared processor prefix in
> `config.py`; each is listed above by its full name.

| `AXIOM_CONFIG_PATH` | `~/.axiom/config.sqlite` | Location of the persistent runtime configuration file (F13 #307). Unset/absent = the env-only bootstrap (the container path — nothing is created). |

## Near-miss pairs

These look almost identical but belong to **different processes / scopes**.
Confusing them is the most common config error:

| Pair | Owned by | Scoped to |
| --- | --- | --- |
| `AXIOM_COMPUTE_WORKER_TIMEOUT` | Go (dispatcher) | **Result-fetch** budget, and as a floor, the submit call's **synchronous source download**. |
| `AXIOM_PROCESSOR_SOURCE_TIMEOUT` | Runner | Runner-side **source_download** budget for pulling one `source_url` (default 120s). |
| `AXIOM_COMPUTE_WORKER_URLS` | Go (dispatcher) | **Ordered ingest chain** (#207): comma-separated, preference order; plural wins over the singular below. |
| `AXIOM_COMPUTE_WORKER_URL` | Go (dispatcher) | **Primary ingest** runner (role: processing) — becomes the head of the candidate list when the plural is unset. |
| `AXIOM_INGEST_FALLBACK_URL` | Go (dispatcher) | **Emergency ingest** runner — appended as last candidate when distinct. |
| `AXIOM_QUERY_RUNNER_URL` | Go (dispatcher) | **Query** runner (role: embed/rerank for search). |

> **Precedence (#207):** `AXIOM_COMPUTE_WORKER_URLS` (complete chain, wins over
> everything) → `AXIOM_COMPUTE_WORKER_URL` + `AXIOM_INGEST_FALLBACK_URL`
> (folded into a two-entry chain; the legacy `AXIOM_PROCESSOR_URL`/
> `AXIOM_PROCESSOR_URLS` spellings still feed the same knobs — warned once,
> counted, see the [deprecation schedule](../operations/deprecations.md)) →
> default local runner.
>
> `AXIOM_COMPUTE_WORKER_URL` / `AXIOM_QUERY_RUNNER_URL` / `AXIOM_INGEST_FALLBACK_URL`
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
`config.py` **and** `axiom-compute-worker/__init__.py` (where
`AXIOM_PROCESSOR_COMPUTE` is re-read) — against this table and flag (a) a code
variable missing here, or (b) a table row without a code backing. The grep
targets the package(s), not a single file; `AXIOM_CONFIG_PATH` lives in
`internal/config/configstore` (the store-location knob, not a `Load()` knob).

Next: [Testing](testing.md) · [Architecture Overview](architecture.md)
