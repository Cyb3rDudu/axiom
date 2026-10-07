# Operator Guide: Migrating to 0.2.0

Date: F15 (#309) — the operator entry point for the 0.2.0 release (Epic #342).
Audience: whoever runs axiom — on a laptop, a single host, or several. The
page anchors the new architecture, maps every old name to its canonical
successor, walks the CLI/configuration migration from 0.1.x, points at the
deployment artifacts per topology, and collects the 0.2.0 troubleshooting
patterns. The [clean-machine walkthrough](#clean-machine-walkthrough) at the
end proves the guide carries a fresh system from zero to a search hit.

## What changed — and what did not

0.2.0 decomposes the previous single-process system into named components
and workers behind one role-based binary — without changing behavior on the
public edge:

- **Unchanged:** every public HTTP route (the unversioned compat routes and
  the versioned `/api/v1/{health,search,passage/{id}}` edge) behaves
  byte-identically through 0.2.x. The F01 golden suite (#295) froze the
  0.1.18 baseline as the witness — the frozen route inventory lives at
  `axiom/internal/baseline/fixtures/api_inventory.txt` in the repository;
  the same suite runs green against the new
  binary in all-in-one **and** split-process form.
- **Unchanged:** your environment. Every 0.1.x env var still loads; legacy
  spellings feed their canonical successors through the deprecation witness
  (warn once per process, count every use — see the
  [deprecation schedule](deprecations.md)).
- **New:** one binary, `axiom`, selects its process slice via
  `axiom serve all|api|library|store`; components have ownership boundaries;
  the Library can run on PostgreSQL **or** SQLite.

## The 0.2.0 architecture map

Three domain components, two workers, one runtime binary:

| Component | Responsibility | 0.2.0 status |
| --- | --- | --- |
| **Library** | Zotero selection/sync, document intake, the import ladder, revisions, the repair track | runs inside the runtime binary (`serve library` slice or in `all`) |
| **Store** | durable processing state (PostgreSQL + OpenSearch), revision intake, the claim loop, search | runs inside the runtime binary (`serve store` slice or in `all`) |
| **Research** | research agents on the corpus | reserved — **not** implemented in 0.2.0 |

| Worker | Role | Form |
| --- | --- | --- |
| `axiom-compute-worker` | document processing, query embedding, reranking — pure compute | separate Python service speaking the processor contract v1 over HTTP (reference backend for proof/CI, real ML backend for production) |
| `axiom-repair-worker` | PDF repair (OCR toolchain bundled) | event runner, supervised as a child of the library process — no service-manager entry of its own |
| `axiom-research-worker` | research execution | reserved — not implemented in 0.2.0 |

```mermaid
flowchart LR
    C["research client"] -->|"one public base URL<br/>/api/v1" | API["serve api<br/>the public edge"]
    API -->|"internal Library edge<br/>AXIOM_LIBRARY_URL"| LIB["serve library<br/>Zotero provider, imports,<br/>repair track"]
    API -->|"internal Store edge<br/>AXIOM_STORE_URL"| STO["serve store<br/>intake, dispatcher, search"]
    STO -->|"processor contract v1<br/>HMAC-signed source URLs"| CW["axiom-compute-worker"]
    LIB --> PG[("PostgreSQL<br/>library_* and store tables")]
    STO --> PG
    STO --> OS[("OpenSearch index")]
    LIB --> Z["Zotero local API"]
```

In `serve all` the three role boxes collapse into one process and every
edge becomes an in-process call — same software, same contracts.

### Ownership boundaries

- **One public edge.** Clients see exactly one base URL / one public port in
  every topology. The internal Library/Store edges (`AXIOM_LIBRARY_URL`,
  `AXIOM_STORE_URL` targets) are never public.
- **Only the Library speaks Zotero** (F07 #301). Zotero credentials live in
  the Library slice alone; the Store and the compute worker never see them.
- **Workers never own canonical truth** and hold no database credentials:
  the compute worker is pure HTTP compute (no PostgreSQL, no OpenSearch, no
  Zotero — it fetches document bytes over HMAC-signed, lease-scoped source
  URLs); the repair worker is a supervised child behind the Library
  single-writer.
- **Tables have owners.** The Library owns the `library_*` namespace; the
  Store owns its ledgers; no cross-component foreign keys after the data
  migration cutover. The Library's SQLite profile is one file,
  `library.sqlite`, per host — never a second writer domain.
- **No orchestration logic in the domain layer.** Kubernetes ships as
  manifests, never as imports (pinned by a lint sonde; see
  [deployment topologies](topologies.md)).

### The roles

`axiom serve <role>` selects the process slice:

| Role | Serves |
| --- | --- |
| `all` | the full stack in one process |
| `api` | the public edge only — no claim/fixer loops |
| `library` | the Library contract surface plus the repair track |
| `store` | revision intake, the claim loop, the OpenSearch outbox |

The canonical role reference — which role runs in which operating form
(all-in-one, split process, container, Kubernetes, remote compute,
launchd) and which CI witness proves each form — is the
[topology matrix](topologies.md).

## Name mapping: 0.1.x → 0.2.0 {#name-mapping}

The binding decision is [ADR 0001](../adr/0001-canonical-naming.md); this
is the operator view. Legacy names keep working through all of 0.2.x —
every use warns once per process and increments a counter exported in
`/api/health` (`deprecations`) and `/v1/capabilities` (`deprecations`).
Removal happens earliest in an **announced major**, never silently, on
counter evidence — the [criteria](deprecations.md) page carries the
schedule.

### Binaries, processes, artifacts

| Old (0.1.x) | Canonical (0.2.0) | Alias behavior through 0.2.x |
| --- | --- | --- |
| `axiom-ng` | `axiom` (`axiom serve …`) | alias binary of the same build generation; delegates, warns once, counts |
| `axiom_ng_runner` module, service "axiom-runner" | `axiom-compute-worker` (module `axiom_compute_worker`) | legacy module entrypoint keeps working; warns once, counts |
| `axiom-fixer` (`pdf_repair_agent`) | `axiom-repair-worker` | shim delegates to the canonical worker; warns once, counts |
| `axiom_ng` Go module/directory | `axiom` module/directory (#352) | import paths moved mechanically; no alias — code-path fact |
| state directory `~/.axiom-ng/` | `~/.axiom/` (#352) | one-time startup migration moves contents and leaves the legacy path as a symlink (rollback-safe, no dual state) |
| version banner `axiom-ng v…` | `axiom v…` (#352) | no alias — the alias binary prints its own deprecation witness line |
| release assets `axiom-ng-<version>-<os>-<arch>`, `axiom-runner-<gen>-*` | `axiom-<version>-<os>-<arch>`, `axiom-compute-worker-<gen>-*` | releases ship the alias names alongside the canonical ones |
| launchd label `com.axiom.runner` | `com.axiom.compute-worker` | **operator-side switch** — see [deployment](#launchd-macos) below |
| Make target `make runner` | `make compute-worker` | warns and still builds |
| default index `axiom-ng-chunks-v1` | `axiom-chunks-v1` (#352) | byte-preserving `_reindex` via `scripts/reindex_index_rename.sh` (no re-chunking, no re-embedding; RAG **and backfill/rescan tools** stopped, boot the canonical default only after the script reported DONE); rollback = `AXIOM_OS_INDEX` + restart; terminal outbox rows from a premature boot recover via `UPDATE opensearch_outbox SET status='pending', attempts=0, next_attempt_at=now(), last_error=NULL WHERE id='…';`; old index deleted after soak |

### Environment variables

Dispatcher-side spellings moved to the canonical `AXIOM_COMPUTE_WORKER_*`
block (F10 #304); the legacy spellings feed them through the witness:

| Legacy (0.1.x) | Canonical (0.2.0) |
| --- | --- |
| `AXIOM_PROCESSOR_URL` | `AXIOM_COMPUTE_WORKER_URL` |
| `AXIOM_PROCESSOR_URLS` | `AXIOM_COMPUTE_WORKER_URLS` |
| `AXIOM_PROCESSOR_RUNNER_NAME` | `AXIOM_COMPUTE_WORKER_NAME` |
| `AXIOM_PROCESSOR_TIMEOUT` | `AXIOM_COMPUTE_WORKER_TIMEOUT` |
| `AXIOM_PROCESSOR_SOURCE_SECRET` | `AXIOM_COMPUTE_WORKER_SOURCE_SECRET` |
| `AXIOM_PROCESSOR_SOURCE_BASE_URL` | `AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL` |
| `AXIOM_RUNNER_HEALTH_INTERVAL` | `AXIOM_COMPUTE_WORKER_HEALTH_INTERVAL` |
| `AXIOM_FIXER_CMD` | `AXIOM_REPAIR_WORKER_CMD` |

> **Frozen, not legacy:** the compute worker's **own** env contract
> (`AXIOM_PROCESSOR_PORT`, `AXIOM_PROCESSOR_BIND_ADDR`,
> `AXIOM_PROCESSOR_COMPUTE`, `AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS`,
> `AXIOM_PROCESSOR_WORK_ROOT`, `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS`,
> `AXIOM_PROCESSOR_RESULT_RETENTION`) is read by the Python service itself
> and is deliberately **not** renamed through 0.2.x. Full reference:
> [Configuration](../developer-guide/configuration.md).

The complete key→key precedence (canonical wins, legacy feeds) is visible
in the effective view — see below.

## Migrating CLI and configuration, step by step

0.1.x operation was env-only: one process started bare, everything
configured through `AXIOM_*` variables. 0.2.0 keeps that shape working (the
compat boot) and adds the role CLI on top. Migrate in this order:

### 1. Take stock of your current environment

```bash
axiom config get --effective --json
```

prints every resolved key with its source (`env` or `default`), secrets
redacted, DSNs sanitized. Run it under 0.2.0 first — any legacy spelling
you still use shows up in the `deprecations` counters after the process has
loaded them once. `axiom config validate` checks the env combination
consistency (parses every set value; wires the derived role set without
starting anything).

### 2. Get the binary

- From source: `make rag` builds `dist/axiom-<version>-<os>-<arch>` (plus
  the compatibility alias of the same generation).
- From releases: `scripts/install_release.sh rag <version>` (see
  [Services & Releases](services.md) for the install layout and staging
  rules).

`axiom version` prints the identity banner (agrees with `/api/health`);
`axiom version --json` for automation.

### 3. Move from bare boot to role start

```bash
axiom serve all        # what the bare 0.1.x boot did — the compat shape
axiom serve api        # split: the public edge only
axiom serve library    # split: Library slice (Zotero provider, repair track)
axiom serve store      # split: intake + dispatcher + search
```

Exit codes are a contract: `0` success, `1` runtime failure (startup
diagnosis, unhealthy doctor, invalid env), `2` usage error. The one-shot KG
maintenance modes (`-maintenance-retention`, `-consolidate-relations`, …)
keep working under both binary names.

### 4. First aid: `axiom doctor`

```bash
axiom doctor           # text report; exit 0 only when fully healthy
axiom doctor --json
```

Checks: config (env the loader would silently ignore), database
(reachability **plus the migration ledger** — green only after the first
`serve` migrated the schema), OpenSearch (an explicitly set-but-empty URL
is the documented *disabled* state, not a failure), artifact root
(configured, a directory, writable). Failures carry the diagnosis, never a
secret value.

### 5. Persistence profiles (F12 #306)

The Library can own its database or its file:

```bash
# PostgreSQL profile (default — the 0.1.x shape, core DSN shared):
export AXIOM_STORAGE_LIBRARY_DRIVER=postgres

# or a dedicated library database:
export AXIOM_LIBRARY_DATABASE_URL=postgresql://<user>:<pass>@<host>:5432/<library-db>
# (DSN keys are secret rows since DM07 — see section 7; the credential
#  home is the environment / the OS secret store)

# SQLite profile (single host!):
export AXIOM_STORAGE_LIBRARY_DRIVER=sqlite
export AXIOM_LIBRARY_SQLITE_PATH=~/.axiom/library.sqlite
```

Operating rules that matter to an operator:

- **SQLite is single-host.** The WAL lives next to the file; one host's
  process set owns it. Multi-replica or network-filesystem deployments need
  PostgreSQL (SQLite on NFS is corruption).
- **The write-along sync mirror lane** (the German term *Mits-Schrieb*
  appears in the codebase and log lines) stays wired **only** in
  shared-database shapes — decided by DATABASE identity (host+port+db,
  not string equality: the split-credentials interim points both pools
  at one database through different roles and the lane stays wired).
  A separate library database or the SQLite profile unwires it — with a
  loud log line, never silently.
- **`zotero` import providers require the PostgreSQL profile** (the
  provider's source identity lives in the mirror on the shared database);
  a component-local SQLite file refuses them loudly at start.
- Store stays profile-guarded on PostgreSQL in 0.2.0 (a SQLite store engine
  is a follow-up epic; the seams are contract-tested either way).

Details, engine rules and the repository contract suite:
[Component Persistence](../developer-guide/component-persistence.md).

### 6. Internal component addresses (split topology, F11 #305)

When the roles run as separate processes, the api process points at the
component edges, and the library/store processes expose them:

```bash
# on the api host/process:
export AXIOM_LIBRARY_URL=http://<library-host>:8211
export AXIOM_STORE_URL=http://<store-host>:8212

# on the library process:
export AXIOM_INTERNAL_LIBRARY_ADDR=0.0.0.0:8211

# on the store process:
export AXIOM_INTERNAL_STORE_ADDR=0.0.0.0:8212

# optional, both sides: budget for non-streaming internal calls (default 30s)
export AXIOM_COMPONENT_TIMEOUT=45s
```

Failures across these edges surface as typed envelopes (for example
`library/unavailable` with the component name, no host/port/dial detail
leaking to clients). The [container compose file](#container) is the
maintained reference for the full split environment.

### 7. Credentials and roles (DM07 #316)

Components own their credentials. Two PostgreSQL runtime roles —
`axiom_library` and `axiom_store` — one per component pool; worker
processes carry no database credentials at all.

**Canonical configuration** (the split credentials):

```bash
# the Store pool (canonical spelling):
export AXIOM_STORE_DATABASE_URL=postgresql://axiom_store:<pass>@<host>:5432/<db>
# the Library pool (PostgreSQL profile):
export AXIOM_LIBRARY_DATABASE_URL=postgresql://axiom_library:<pass>@<host>:5432/<db>
```

**The single DSN stays supported until the cutover.** An installation
running on `AXIOM_DATABASE_URL` alone keeps working unchanged (its use
shows on the deprecation witness in `/api/health` — that counter is the
cutover-readiness telemetry). Two rules have teeth:

- Both spellings set with **different** values is a hard error at boot —
  resolve to one. Identical values (the mid-migration overlap) pass.
- All three DSN keys are **secret rows** in `config.sqlite`:
  references only, never values (`axiom config set` refuses them). The
  credential lives in the environment / the OS secret store;
  `config get --effective` shows the credential-free projection. A
  `config.sqlite` written before DM07 may carry an
  `AXIOM_LIBRARY_DATABASE_URL` **value** row — after the upgrade boot
  refuses it loudly; remediate with
  `axiom config unset AXIOM_LIBRARY_DATABASE_URL` and feed the DSN via
  the environment.

**The roles** ship as one idempotent script:
`deploy/postgres/roles.sql`. It creates both LOGIN roles (passwordless —
passwords are set via `ALTER ROLE`, never stored in the repo), revokes
`CONNECT` from `PUBLIC`, and grants DML on exactly each component's own
tables — runtime roles are DML-only: **schema changes are window work**,
applied as the admin/deployer; a component boots clean on a current
schema because its ledger check performs no DDL when the schema is
current. Wrongly-assigned credentials fail loudly at start (the store
pool connected as `axiom_library`, or the reverse, aborts with the
diagnosis naming the DSN to check).

**Run it on a dev mirror copy** (the drill the CI also runs per push):

```bash
psql -v ON_ERROR_STOP=1 -f deploy/postgres/roles.sql <dev-mirror-db>
ALTER ROLE axiom_library PASSWORD '<dev-pw>';   -- throwaway on a mirror
ALTER ROLE axiom_store   PASSWORD '<dev-pw>';
```

The application on the **reference database is DM09 cutover-window
work, together with the administrator** — not something this section
asks you to do today.

**Workers are credential-free.** The repair worker runs on a
constructed minimal environment (an allowlist — the parent's DSNs and
secrets never ride along). The compute worker refuses to start when its
environment carries a known credential variable, naming the key, never
the value. `axiom serve api` in the split topology opens **no** database
pool and holds **no** DSN in its configuration — a DSN present in its
environment is ignored with a loud note instead of silently booting a
local stack (its `/api/health` dependency checks proxy the library/store
component health over the split edges). For full environment hygiene,
feed the api deployment no DSN at all: the shipped compose and K8s
examples keep `AXIOM_DATABASE_URL` out of the api service (a
compromised edge process cannot leak what its environment never
carried).

**What still waits for the cutover window** (ordering matters): the
Zotero mirror and the repair tables still ride the core (store) pool in
this codebase — that traffic moves onto the Library pool when the DM
track relocates the mirror (DM04) in the same window the DSNs flip
(DM09). Until then, production keeps running the single DSN; the
split-credentials shape is drilled, not deployed.

**Rollback** (from the split-credentials shape back to single-DSN):
unset `AXIOM_STORE_DATABASE_URL`/`AXIOM_LIBRARY_DATABASE_URL`, restore
`AXIOM_DATABASE_URL`, restart — the table grants and the roles
themselves are inert without the DSNs pointing at them. ONE step of
the script is NOT inert, though: `roles.sql` revoked `CONNECT` from
`PUBLIC` on the databases it ran against. If the legacy single-DSN
role connected through that default (it is neither the database owner
nor explicitly granted `CONNECT`), restoring the DSN alone is not
enough — re-grant its connect right in the same rollback:

```sql
GRANT CONNECT ON DATABASE <db> TO <legacy-role>;  -- or TO PUBLIC to restore the default
```

(optional cleanup: `DROP ROLE axiom_library, axiom_store;` once nothing
connects as them)

### 8. What stays env-only (for now)

Persistent runtime configuration (`axiom config set`, the `config.sqlite`
store, config flags joining the precedence chain) follows with F13 (#307).
Until then the environment is the configuration source; `config get
--effective` remains the read-only view.

## Deployment per topology

The same software in every supported operating form — one binary, roles per
argument. This section points at the maintained artifacts; the
[deployment topologies](topologies.md) page carries the full form × edge ×
CI-witness matrix.

### All-in-one

`axiom serve all` (or the bare compat boot) — the default. The F01 golden
suite proves it against release builds; this is the shape the
[walkthrough](#clean-machine-walkthrough) below runs first.

### Split process (OS processes)

Three `axiom serve` processes plus the compute worker as a fourth process.
Working-tree dev scripts exist for the developer host
(`scripts/dev/split-up.sh`, `split-smoke.sh`, `split-down.sh`); the
maintained environment reference for operators is the
[container compose file](#container) below (same roles, same edges, same
env). When wiring it yourself, the role table above plus the internal-edge
envs are the complete surface.

### Container

One image, roles per argument — `deploy/container`:

```bash
docker build -f deploy/container/Dockerfile -t axiom-topology .
docker compose -f deploy/container/compose.topology.yml up -d --wait
deploy/container/topology-smoke.sh          # health, intake, search, kill probe, teardown
docker compose -f deploy/container/compose.topology.yml down -v
```

The stack brings its own ephemeral pgvector/OpenSearch and a Zotero probe
stand-in; `down -v` drops all state. CI runs the same smoke on every push.

### Kubernetes

The same image as `deploy/k8s/topology.yaml` (Deployments + Services,
roles per args; Secret/ConfigMap placeholders to adapt). Validated
structurally with kubeconform in CI — a cluster is deliberately not a CI
dependency; cluster behavior is covered by the container-equivalent ride.

### launchd (macOS)

The shipped templates live in `deploy/launchd` (env files, not env
stanzas; logs under `~/.local/state/axiom/logs`; `$HOME` is not expanded by
launchd in path keys — the installer substitutes).

- **Scheduling policy (F14 #308):** every service that spawns CPU-bound
  work runs `ProcessType=Standard` — `com.axiom.rag` (its repair/OCR
  children cannot escape a Background coalition they inherit),
  `com.axiom.compute-worker` (ML inference), and the dispatcher instances.
  A Background-class CPU-bound workload measures ~9× slower wall-time on
  Apple silicon; the controlled A/B/C evidence and the reproduction probe
  (`deploy/launchd/qos-probe.sh`) are documented in
  [launchd scheduling](launchd-qos.md) and the
  [QoS diagnostics](../diagnostics/2026-09-21-ocrmypdf-background-qos.md).
- **The repair worker has no plist** — it is an event runner supervised
  inside the library process (one invocation per attachment key, per-key
  lock, timeout). Do not give it a KeepAlive service entry.
- **Label switch:** hosts still running the pre-F10 label switch once —
  `launchctl bootout gui/$(id -u)/com.axiom.runner`, then bootstrap the
  `com.axiom.compute-worker` template. Nothing migrates this for you; the
  old label simply never updates again.

### Remote compute

The compute worker is reachable over HTTP only (another host, a GPU
carrier, a container namespace): the store signs lease-scoped source URLs,
the worker fetches document bytes itself — no shared Zotero mount, no
credentials on the worker. Sizing, GPU pinning, transport rules and the
`--network=host` requirement for bulk flows:
[Deployment](deployment.md).

## v0.2.3: the mirror plane moves to the Library database (#358)

v0.2.3 completes the component split for the Zotero mirror plane (the
decision and its reasoning: [ADR 0002](../adr/0002-mirror-plane-end-state.md)).
What an operator needs to know:

**What moves.** The Zotero mirror (`zotero_*` tables) and the sync that
writes them now run on the **Library database**. The Store keeps its own
denormalized projection (`store_documents`, written at intake time) for
everything processing needs — search titles, source serving, claim
resolution, retention anchors. Nothing about endpoints or jobs changes
shape; the documents listing gains `serving` and `removed` outcome
values ([API semantics](../references/api.md)) and fresh Zotero deletes
surface once as `tombstoned` rows instead of lingering as phantoms.

**The one-time catch-up.** The Library database's mirror copy has been
frozen since the cutover. After deploying v0.2.3, run ONE full
reconcile:

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync \
  -H 'Content-Type: application/json' -d '{"full": true}'
```

The full sync re-lists every Zotero item, applies the divergence since
the freeze, reconciles Zotero-absent phantoms into tombstones, and
re-offers every document to the Store (unchanged, already-served content
is suppressed — the active-snapshot defense). The response's
`tombstoned_documents` count is the reconciliation report.

**Topology requirements.**

- The library-bearing processes (`serve library`, `serve api`, `serve
  all`) need the Library PostgreSQL profile; `AXIOM_LIBRARY_DATABASE_URL`
  must point at the Library database (it already does in the split
  topology; in the single-database shape it stays unset and both planes
  share the one database — supported).
- `AXIOM_STORAGE_LIBRARY_DRIVER=sqlite` is no longer combinable with the
  sync role: the boot refuses loudly.
- `serve store` runs without the repair wave gate (the repair queue is
  Library-side); healing coordination lives where repair is visible.

**What stays behind.** The Store database's `zotero_*` tables remain as
a frozen archive through the v0.2.3 soak — a bestand backfill (store
migration 0004) mints the projection rows from the archive so search
hydration, retention anchors and in-flight claims keep working from the
first boot (historical repair cases seed the retention flag with it).
Dropping the archive tables is documented follow-up after the soak.

The backfill and the documents listing key on mirror UUIDs: the Library
database's cutover copy and the Store archive MUST share the zotero_*
uuids (they do — the copy is physical); that identity continuity is the
load-bearing invariant of the migration.

## Releases with schema changes: the migrate phase (#362)

Every release that ships new migration files (core `internal/db/schema`,
store `internal/store/migrations/schema`, library
`internal/library/pglib/schema`) runs a three-phase rollout. The v0.2.3
rollout improvised exactly this sequence by hand — it is a documented
phase now, and the tooling below exists so it never needs improvising
again.

**Phase 1 — apply migrations (`axiom migrate`, deployer DSN).** Runtime
roles are DML-only (DM07); schema changes are window work. Run the
migration command with a DSN that may apply DDL (the deployer/admin
credential — never a component role):

```bash
AXIOM_STORE_DATABASE_URL=postgresql://<deployer>:<pass>@<host>:5432/<db> \
  axiom migrate
```

The command applies every pending component migration (core + store on
the store DSN; library on `AXIOM_LIBRARY_DATABASE_URL` when the
PostgreSQL profile is selected — the split topology migrates both
databases in one run) and prints each component's ledger before →
after:

```text
core:     0022_figure_captions.sql -> 0023_contextual_citation_class.sql (+1: [0023_contextual_citation_class.sql])
store:    0003_revision_identity_active_scope.sql -> 0004_store_documents.sql (+1: [0004_store_documents.sql])
library:  up to date (0004_zotero_mirror.sql)
```

It is idempotent — the same runners, same ledgers as the boot path. A
database with nothing pending answers `up to date` and exits 0;
failures exit non-zero. Run it BEFORE switching any service onto the
new binaries: the old binaries run unchanged against the
post-migration schema (migrations are additive), so the window has no
read-only gap.

**Phase 2 — switch the services onto the new binaries.** Ordinary
restart of the deployment; no special ordering. If Phase 1 was skipped
and a runtime role (DML-only) boots against pending migrations, the
boot fails FAST with the remedy instead of crash-looping on a raw
permission error:

```text
axiom: composition: component postgres failed to start: core: 1 pending
migration(s) [0023_contextual_citation_class.sql] for a DML-constrained
boot (role "axiom_store" lacks CREATE on the schema — DM07 runtime
roles are DML-only): run `axiom migrate` with the deployer DSN against
this database, then restart
```

That message is the whole diagnosis: apply Phase 1, restart, done.

**Phase 3 — verify.** `axiom doctor` (exit 0 only fully healthy — the
schema probe reads the migration ledgers) and a `/api/v1/health` check
per process; the release's own verification steps ride on top.

## Troubleshooting: 0.2.0 patterns

Symptom→cause→fix patterns for processing and transport classes live in
[Troubleshooting](troubleshooting.md). These are the 0.2.0 additions:

### Split-brain: two writers on one Library

**Symptom.** A second Library process (or a leftover instance after a
botched restart) refuses at start with a typed writer-lease conflict:

```text
writer lease <scope> held by <owner> (heartbeat 3s ago) — Library is
single-writer: stop the other instance or wait for the lease TTL
```

**Diagnose.** The error already names the live owner and the heartbeat age
— that is the diagnosis. `heartbeat 3s ago` means the other writer is
alive **now**; an age growing past the TTL (default 30 s, renewed at
TTL/3) means it is gone.

**Fix.** Stop the stale instance; never delete lease rows by hand. A
crashed writer's lease is takeable after one full TTL — a clean restart
waits at most that long. This refusal is the system working: two writers
would duplicate imports and corrupt custody.

### Lease errors on restart

A restart immediately after a kill may refuse with the same conflict for up
to one TTL (the dead owner's lease is still "fresh"). Wait it out — 30 s by
default at personal-library scale — or raise
`DefaultWriterLeaseTTL` only with a reason.

### Remote-class worker behind a loopback port (source base override) {#remote-class-worker}

**Symptom.** At dispatcher start a log line

```text
source base override: all ingest candidates are loopback — forcing
http://127.0.0.1:<api-port> (configured <your-base> is not loopback-stable)
```

and afterwards every source download from a worker that runs in its own
network namespace (container, VM) times out or connects to itself — it
literally cannot reach the loopback of the host.

**Cause.** The solo-loopback guard: when **every** ingest candidate URL is
loopback, the dispatcher assumes co-located workers and rewrites a
configured non-loopback source base to loopback (a production safety net —
a stale LAN base makes every source download a zero-byte timeout for truly
co-located workers). A containerized worker published on a loopback port
looks co-located to the guard but is remote-class in reality.

**Fix.** Address the worker by a **non-loopback, machine-resolvable** name
(the local network name, or the machine's LAN address) and publish its port
on all interfaces. Then the guard does not fire and the configured source
base stays as set. (The container topology never hits this: inside a
compose network nothing is loopback.)

### Clock-skew warnings

**Symptom.** A periodic log line from the dispatcher:

```text
CLOCK SKEW WARNING: host-DB offset 45s exceeds ±30s (host ahead of DB); freshness must stay DB-side (#271) — investigate host sleep/NTP/VM clock
```

**Cause.** Lease writes and fences run on the **database** clock; the host
only mints source-URL expiry values. When the host and the database drift
(maintenance sleep, an NTP step, a VM snapshot restore), freshness
decisions can silently flip. The watcher samples `SELECT now()` at start
and once a minute and warns past ±30 s — far below the 5-minute lease, so
the step is flagged long before it bites.

**Fix.** Fix the clocks (host sleep policy, NTP, the VM's clock source);
do not "fix" it by editing leases. The offset is observer-only — nothing
degrades while the warning fires; it exists so a 7-minute chronyd step
cannot pass unnoticed again.

### Baseline / golden failures (developers)

The F01 golden suite goes **red on every identity change by design** — the
freeze bits witness the public contract. If your change legitimately alters
a frozen surface: regenerate deliberately with `BASELINE_UPDATE=1` in the
same PR, never by relaxing a comparator. The split/container rides use
contract-class checks (typed shapes, no frozen fixtures); `make
golden-baseline` requires the dev environment in `--release` mode (freeze
bits, not debug builds). The documented 0.2.0 run-mode escape for build
identity is `AXIOM_BASELINE_EXPECT_BUILD`.

### Data migration and rollback (what ships when)

The 0.2.0 code runs against your existing database shape today (the
Zotero-sync lane stays wired in shared-database shapes — see persistence
profiles above). The operational cutover — adopting the existing corpus in
place, no reprocessing — is the Data Migration track (#310–#321): a
backend-neutral bundle format, Library export/import/verify, a read-only
adoption check (`VerifyAdoption` — refuses drifted state), shadow reads,
and an automated cutover with rollback. Its runbook ships with the release
train once the cutover rehearsals are done; until then there is **no
operator action** — and nothing in this guide depends on it.

The bundle format and the Library data commands HAVE shipped (DM03/DM04,
#312/#313), and so has the shadow-read comparison (DM08, #317):

```bash
# write a bundle from a source database (read-only, one snapshot):
axiom data export --component library --dsn "$SOURCE_URL" --out bundle/

# apply it (idempotent; an occupied target needs --merge):
axiom data import --component library --from bundle/ --dsn "$TARGET_URL"
# or into the SQLite profile's file:
axiom data import --component library --from bundle/ --sqlite ~/.axiom/library.sqlite

# verify: per-table counts + canonical digests, FK-invariant scans,
# semantic envelope readbacks, PRAGMA integrity on SQLite:
axiom data verify --component library --from bundle/ --dsn "$TARGET_URL" [--json]

# shadow-read (DM08): the legacy mirror copy against the imported
# copy, FULL data set, explicit normalization allowlist — every other
# deviation red; exit 0 only on zero unexpected deviations:
axiom data shadow --component library \
  --source-dsn "$LEGACY_MIRROR_URL" --dsn "$IMPORTED_URL" \
  --out shadow-report.json
# or against an imported library.sqlite:
axiom data shadow --component library \
  --source-dsn "$LEGACY_MIRROR_URL" --sqlite ~/.axiom/library.sqlite \
  --out shadow-report.json
```

Properties that matter to an operator:

- **Engine-portable, verifiable.** Canonical mappings (UUID strings, UTC
  RFC3339-µs timestamps, permutation-stable canonical JSON, validated
  enums, verbatim decimal tokens) make a bundle digest-comparable across
  PostgreSQL and SQLite; `manifest.json` is pinned by a `bundle.sha256`
  sidecar (SHA256SUMS pattern) and every batch file by a SHA-256 in the
  manifest. A tampered batch aborts the import ISOLATED (nothing of it
  applied); an unknown enum/status aborts loudly — never a silent default.
- **Idempotent by stable key + payload.** Re-running an import never
  duplicates; diverged rows abort as merge conflicts instead of being
  overwritten. The import is data-only (zero DDL) and runs under the
  `axiom_library` runtime role — the `setval` sequence resync after
  explicit-id inserts is why that role carries `UPDATE` on sequences
  (roles.sql section 5).
- **In-flight imports survive verbatim** — `awaiting_confirmation`
  imports land byte-identical, neither completed nor dropped.
- **No document texts, no secrets** in any log line or error: failures
  name tables, columns, counts, digests and key identifiers only.
- The legacy mirror tables (zotero_*, repair_cases) import into
  PostgreSQL targets; a `library.sqlite` target carries the `library_*`
  namespace only and skips the legacy set with counted warnings (F12).

The SQLite target additionally proves `PRAGMA integrity_check` and
`PRAGMA foreign_key_check` clean, and the imported file passes the
Library repository contract suite (F12 engine matrix). The dev
rehearsal against a fresh production mirror copy — export → import →
verify, all green — is documented in #313.

**Shadow-read properties (DM08 #317):**

- **Deterministic by pull point.** The legacy side reads a restored
  mirror copy pinned to one `REPEATABLE READ READ ONLY` snapshot (the
  cutoff is recorded in the report); the imported side derives from
  the same pull through the bundle. Both sides out of one pull = a
  re-runnable comparison, which is why the cutover window's last
  check is this same command.
- **Full corpus, not a sample.** Every Library table compares over
  every stable key: records, collections, renditions, selections,
  repair readback, raw envelopes — plus the acquisition ledgers
  (imports, events, steps, identifiers, provenance, revisions,
  anchors, audit, leases). Structural deviations (a stable key on one
  side only, a missing table, a drifted column set) are red.
- **Explicit allowlist, everything else red.** Approved normalizations
  are a documented, reviewed list — lexical number spellings
  (`numeric-value`, `float-value`, `json-number-value`) and the
  timestamp-instant belt — each absorption counted and sampled in the
  report. Read-time canonicalization (UTC µs timestamps,
  permutation-stable JSON, boolean folding, UUID case) applies to both
  sides by construction. **No id-remap entry exists on purpose**: the
  import preserves stable-key ids verbatim, so a differing id is an
  unexpected diff.
- **No-leaks report.** The artifact carries tables, columns, counts,
  stable-key identifiers and per-side value digests — never row
  values, never DSNs.
- **Sonde teeth, in CI and rehearsed on a real corpus:** an injected
  semantic deviation (a changed title, a changed envelope value) goes
  red; a pure numeric-spelling difference is absorbed and counted as
  normalized — the bounds of the allowlist are witnessed, not
  assumed. The dev rehearsal artifacts live in
  `docs/diagnostics/2026-10-04-shadow-read-dm08.md`.

## Clean-machine walkthrough

Nothing but this page: a fresh machine (or an empty directory), no
inherited environment, no host databases. Everything substrate-shaped runs
as disposable containers; the runtime itself is the binary you build.

**Prerequisites:** a Go toolchain (see `axiom/go.mod`), a container
runtime with a compose implementation (Docker + compose plugin, or podman +
podman-compose — the topology smoke accepts either via
`AXIOM_TOPOLOGY_COMPOSE`), `jq`, `curl`, `git`.

### 1. Clone and build

```bash
git clone https://github.com/Cyb3rDudu/axiom.git && cd axiom
make rag
AXIOM="$(ls dist/axiom-v* | grep -v '\.sha256$' | head -1)"   # if ls dist/ shows a different version string, use that artifact
"$AXIOM" version
```

### 2. Start the substrate (disposable containers)

```bash
docker run -d --name axiom-guide-pg \
  -e POSTGRES_USER=axiom -e POSTGRES_PASSWORD=axiom -e POSTGRES_DB=axiom \
  -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16

docker run -d --name axiom-guide-os \
  -e discovery.type=single-node -e DISABLE_SECURITY_PLUGIN=true \
  -e OPENSEARCH_JAVA_OPTS="-Xms512m -Xmx512m" \
  -p 127.0.0.1:9201:9200 opensearchproject/opensearch:2.19.1

# Zotero stand-in: answers the Server-ID probe and empty item pages.
# Skip it if a real Zotero with the local API enabled is running.
docker run -d --name axiom-guide-zotero -p 127.0.0.1:23119:23119 \
  python:3.11-slim python3 -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Zotero-Server-ID", "guide-standin")
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"[]")
    def log_message(self, *a):
        pass
http.server.HTTPServer(("0.0.0.0", 23119), H).serve_forever()'

# The compute worker: reference backend, from the topology image (the
# next command builds it once; the split excursion reuses it).
#
# Port note: 8112 is the conventional worker port — if something already
# listens there (or on any port below), pick a free one and use it
# consistently. This walkthrough uses 8116.
#
# The publish must cover ALL interfaces (-p 8116:8112, no 127.0.0.1
# prefix): the worker is remote-class (its own network namespace), and the
# dispatcher refuses non-loopback source bases for loopback-co-located
# candidates — address the worker by a machine-resolvable name instead
# (step 3), which requires more than loopback reachability.
docker build -f deploy/container/Dockerfile -t axiom-topology .
docker run -d --name axiom-guide-worker \
  --add-host=host.docker.internal:host-gateway \
  -e AXIOM_PROCESSOR_PORT=8112 -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
  -p 8116:8112 axiom-topology worker
```

`--add-host` makes the recipe work on Linux; Docker Desktop resolves the
name natively. (`docker` and `podman` are interchangeable throughout — on
podman hosts the worker could also use the native
`host.containers.internal` name.)

### 3. Write the environment file, then `serve all`

The compute worker's URL must be a **non-loopback** name that resolves on
this machine — the local network name (macOS: the LocalHostName with a
`.local` suffix; Linux: the hostname/FQDN or the machine's LAN address).
A loopback URL would trigger the dispatcher's solo-loopback guard (see
[troubleshooting](#remote-class-worker)) and rewrite
the source base to loopback, which the containerized worker cannot reach.

```bash
mkdir -p "$HOME/axiom-guide-state"
WORKER_HOST="$(hostname -f)"                       # Linux: hostname/FQDN
[ "$(uname -s)" = "Darwin" ] && WORKER_HOST="$(scutil --get LocalHostName).local"
cat > "$HOME/axiom-guide-state/axiom.env" <<EOF
export AXIOM_DATABASE_URL=postgresql://axiom:axiom@127.0.0.1:55432/axiom
export AXIOM_OPENSEARCH_URL=http://127.0.0.1:9201
export AXIOM_OS_INDEX=axiom-guide-chunks-v1
export AXIOM_ZOTERO_BASE=http://127.0.0.1:23119/api
export AXIOM_API_PORT=8111
export AXIOM_BIND_ADDR=0.0.0.0
export AXIOM_COMPUTE_WORKER_URLS=http://$WORKER_HOST:8116
export AXIOM_QUERY_RUNNER_URL=http://$WORKER_HOST:8116
export AXIOM_COMPUTE_WORKER_SOURCE_SECRET=guide-secret
export AXIOM_COMPUTE_WORKER_SOURCE_BASE_URL=http://host.docker.internal:8111
export AXIOM_DISPATCHER_ENABLED=1
export AXIOM_LIBRARY_IMPORT_PROVIDERS=fake
export AXIOM_ARTIFACT_ROOT="$HOME/axiom-guide-state/artifacts"
export AXIOM_QUARANTINE_ROOT="$HOME/axiom-guide-state/quarantine"
export AXIOM_ZOTERO_WRITE_KEY_FILE="$HOME/axiom-guide-state/no-write-key"
EOF
mkdir -p "$HOME/axiom-guide-state/artifacts" "$HOME/axiom-guide-state/quarantine"

set -a; . "$HOME/axiom-guide-state/axiom.env"; set +a
"$AXIOM" serve all
```

Why these values: the worker is a container, so it pulls document sources
over signed URLs from `host.docker.internal` — which requires the API to
listen beyond loopback (`AXIOM_BIND_ADDR=0.0.0.0`; on an untrusted network,
bind the specific interface instead). The write-key path deliberately does
not exist: the Zotero write surface stays off. The first boot migrates
the schema; watch for the startup banner, then `ok:true` with the four
dependency checks (postgres, zotero, query-runner, ingest-runner) at
`http://127.0.0.1:8111/api/health`.

### 4. Doctor, green

In a second terminal:

```bash
cd <repo> && AXIOM="$(ls dist/axiom-v* | grep -v '\.sha256$' | head -1)"
set -a; . "$HOME/axiom-guide-state/axiom.env"; set +a
"$AXIOM" doctor && echo "doctor exit 0"
curl -fsS http://127.0.0.1:8111/api/health | jq '{ok, checks, deprecations}'
```

Every check reads `ok`; the `deprecations` map is empty — the guide used
only canonical names.

### 5. One document through the pipeline, one search hit

Seed the Zotero mirror with the repository's test EPUB and push it through
the Store's revision intake (the same contract class the CI topology
smoke rides):

```bash
FIXTURE="axiom/internal/backfill/testdata/book.epub"
HASH="$(shasum -a 256 "$FIXTURE" | awk '{print $1}')"   # Linux: sha256sum

docker exec -i axiom-guide-pg psql -U axiom -d axiom -v ON_ERROR_STOP=1 <<SQL
BEGIN;
DELETE FROM zotero_attachments WHERE zotero_key='ATTF15G1';
DELETE FROM zotero_documents WHERE zotero_key='DOCF15G1';
DELETE FROM zotero_items WHERE zotero_key='DOCF15G1';
DELETE FROM zotero_sources WHERE base_url='https://guide.local';
INSERT INTO zotero_sources (base_url, library_id, server_id)
VALUES ('https://guide.local','users/0','guide-standin');
INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
SELECT id, 'DOCF15G1', 1, 'book', 'Guide walkthrough book'
FROM zotero_sources WHERE base_url='https://guide.local';
INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
SELECT id, 'DOCF15G1', 1, 'journalArticle', NULL, '{}', '{}'
FROM zotero_sources WHERE base_url='https://guide.local';
UPDATE zotero_documents d SET canonical_item_id = i.id
FROM zotero_items i, zotero_sources s
WHERE d.source_id = s.id AND i.source_id = s.id
  AND d.zotero_key = 'DOCF15G1' AND i.zotero_key = 'DOCF15G1'
  AND s.base_url='https://guide.local';
INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
SELECT s.id, d.id, 'ATTF15G1', 1, 'DOCF15G1', 'imported_file', 'application/epub+zip',
       'guide.epub', '$PWD/$FIXTURE', '$HASH', true, false
FROM zotero_sources s, zotero_documents d
WHERE s.base_url='https://guide.local' AND d.zotero_key='DOCF15G1' AND d.source_id=s.id;
COMMIT;
SQL

DOC="$(docker exec axiom-guide-pg psql -U axiom -d axiom -tAc \
  "SELECT d.id FROM zotero_documents d JOIN zotero_sources s ON s.id=d.source_id WHERE s.base_url='https://guide.local'" | tr -d '[:space:]')"
SRC="$(docker exec axiom-guide-pg psql -U axiom -d axiom -tAc \
  "SELECT id FROM zotero_sources WHERE base_url='https://guide.local'" | tr -d '[:space:]')"
REV="$(jq -nc --arg h "$HASH" --arg s "$SRC" \
  '{source_id:$s, revision_id:"1", rendition_id:"ATTF15G1", content_hash:$h,
    media_type:"application/epub+zip", content_ticket:"probe",
    bibliography:{record_id:"DOCF15G1", title:"Guide walkthrough book", citation_class:"citable"}}')"

curl -fsS -X POST http://127.0.0.1:8111/api/v1/store/ingest \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg r "$REV" '{idempotency_key:"f15-guide-1", revision:($r|fromjson)}')"
```

The intake answers `202`; the dispatcher claims the job, the worker
processes it (watch `"$HOME/axiom-guide-state"` artifacts appear and the
serve log's phase lines), the outbox lands the chunks in the index. Poll
search until the hit arrives:

```bash
for i in $(seq 1 60); do
  HITS="$(curl -fsS -X POST http://127.0.0.1:8111/api/v1/search \
    -H 'Content-Type: application/json' \
    -d "{\"query\":\"chapter inhalt\",\"top_n\":5,\"filters\":{\"document_ids\":[\"$DOC\"]}}")" \
    && [ "$(echo "$HITS" | jq '.hits | length')" -gt 0 ] && break
  sleep 5
done
echo "$HITS" | jq '.hits[0] | {chunk_id, doc: .source.doc_id, locator}'
```

A hit with `chunk_id`, `source.doc_id` and a typed `locator` — first light.

### 6. Split excursion (while the all-in-one keeps running)

The container split topology on the same machine — note it shares nothing
with your walkthrough state (own database, own index, own ports):

```bash
docker compose -f deploy/container/compose.topology.yml up -d --wait
deploy/container/topology-smoke.sh
docker compose -f deploy/container/compose.topology.yml down -v
```

The smoke proves the split over its own public edge (`127.0.0.1:18111`):
health, intake through the public edge, the remote-class worker ride to
searchability, typed search/passage shapes, the library kill probe (typed
`library/unavailable`, leak-free), and clean teardown. On podman hosts,
substitute `docker` → `podman` and run the smoke with
`AXIOM_TOPOLOGY_COMPOSE="python3 -m podman_compose"`.

### 7. Teardown

Stop the serve process (Ctrl-C — graceful shutdown), then:

```bash
docker rm -f axiom-guide-worker axiom-guide-zotero axiom-guide-os axiom-guide-pg
rm -rf "$HOME/axiom-guide-state"
```

Nothing is left: no host database, no host index, no state outside the
directories this walkthrough created.
