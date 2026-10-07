# Deprecation schedule

Policy and removal criteria for legacy names — the data basis is usage
counters, not gut feeling. Companion page: [Operator Guide: Migrating to
0.2.0](migration-guide.md) · binding decision: [ADR 0001](../adr/0001-canonical-naming.md).

## The policy

- Legacy names are **functional through all of 0.2.x**. Nothing breaks, no
  behavior change.
- Every use of a legacy name **warns exactly once per process** (not per
  call) and **increments a counter**.
- The counters are exported in the running system: `deprecations` in
  `/api/health` (the runtime binary) and in `/v1/capabilities` (the compute
  worker).
- Removal happens **earliest in an announced major release**, never
  silently, and only on counter evidence.

## Removal criteria (counter readings, not dates)

A legacy name becomes a removal candidate when **all** of the following
hold:

1. **Zero usage across a full release cycle**: the counter reads `0` on
   production deployments over two consecutive minor trains (self-reported
   health exports plus operator confirmation on the tracked hosts).
2. **The canonical spelling is the only documented one**: the migration
   guide and configuration reference carry no legacy spellings outside the
   mapping tables.
3. **The removal is pre-announced**: named in a release note and on this
   page at least one minor release before the major that removes it,
   including the migration step (usually: change the env file, relabel the
   service).

Counters that keep moving — even rarely — keep the alias alive: a single
nonzero reading on a production host defers removal to the next
evaluation. There is no date by which an alias dies; there is a threshold
it must fall below.

## Reading your own counters

```bash
curl -fsS http://<api-host>:<port>/api/health | jq .deprecations
curl -fsS http://<worker-host>:<port>/v1/capabilities | jq .deprecations
```

An empty map (or the field absent): no legacy names in use — nothing to
migrate. Any key present tells you exactly which spelling still loads and
how often it has been read since process start.

## Current legacy names

| Legacy name | Kind | Witness | Horizon |
| --- | --- | --- | --- |
| `axiom-ng` | binary | counter (alias warns + counts) | removal candidate per criteria above |
| `axiom_ng_runner` module entrypoint, "axiom-runner" service | compute worker module | counter | same criteria |
| `axiom_ng` Go module/directory (#352) | code path | none — internal import paths moved mechanically; rebuild from source | n/a (not a runtime name) |
| state directory `~/.axiom-ng/` (#352) | filesystem | startup migration symlink (no counter) | symlink stays through 0.2.x; removal with the alias generation |
| default index `axiom-ng-chunks-v1` (#352) | OpenSearch index | none — byte-preserving `_reindex` (`scripts/reindex_index_rename.sh`); rollback = `AXIOM_OS_INDEX` | old index deleted after soak; the in-code rename guard (`LegacyIndexName`) is removed together with it |
| `axiom-fixer` shim | repair worker binary | counter | same criteria |
| `AXIOM_PROCESSOR_URL`, `AXIOM_PROCESSOR_URLS`, `AXIOM_PROCESSOR_RUNNER_NAME`, `AXIOM_PROCESSOR_TIMEOUT`, `AXIOM_PROCESSOR_SOURCE_SECRET`, `AXIOM_PROCESSOR_SOURCE_BASE_URL` | dispatcher-side env | counter | same criteria |
| `AXIOM_RUNNER_HEALTH_INTERVAL` | dispatcher-side env | counter | same criteria |
| `AXIOM_FIXER_CMD` | repair env | counter | same criteria |
| `make runner` | Make target | warns, still builds | same criteria |

**Not legacy** (do not migrate these): the compute worker's own env
contract (`AXIOM_PROCESSOR_PORT`, `AXIOM_PROCESSOR_BIND_ADDR`,
`AXIOM_PROCESSOR_COMPUTE`, `AXIOM_PROCESSOR_WORK_ROOT`,
`AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS`,
`AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS`, `AXIOM_PROCESSOR_RESULT_RETENTION`)
is the worker's frozen public surface through 0.2.x; the index and state-dir
legacy names survive only in the mapping tables above.

## Retired by deletion

Surfaces removed outright (no alias, no counter — the replacement is
the successor architecture itself):

- **`axiom_fixsvc/`** — the #184-era standalone repair service. Dormant
  since the F08 repair track landed: the track's invoker spawns the
  autarkic fixer artifact as its supervised worker, and the fixer
  carries the #184 design principles (isolation, network-API boundary,
  red-sondierbare Import-Audit). Deleted in the "fixer home" strand
  (2026-10-07); no CI/Make/script reference existed at deletion time
  (grep witness in the strand's PR). History:
  `git log --follow -- axiom_fixsvc/`; successor: the F08 repair track
  (`axiom/internal/library/repair/`) plus the fixer artifact
  (`axiom-fixer/`, `scripts/build_fixer_artifact.sh`).

## Operator-side switches (no counter — your action)

These are not witnessed by counters because only you can flip them:

- **launchd label** `com.axiom.runner` → `com.axiom.compute-worker`:
  `launchctl bootout gui/$(id -u)/com.axiom.runner`, then bootstrap the
  new template from `deploy/launchd`. Also switch env files and service
  wrappers that reference the old worker paths to
  `/opt/axiom/bin/axiom-compute-worker`.
- **Env files**: replace legacy spellings with the canonical
  `AXIOM_COMPUTE_WORKER_*` / `AXIOM_REPAIR_WORKER_CMD` keys (the
  [mapping table](migration-guide.md#name-mapping) is the
  reference).
- **Cron/scheduler entries and scripts** that invoke the alias binary:
  point them at `axiom`.

## Follows with F13 (#307)

Persistent runtime configuration (`axiom config set`, the `config.sqlite`
store) is not part of 0.2.0's shipped surface; its documentation follows
when F13 lands. Until then the environment is the configuration source.
