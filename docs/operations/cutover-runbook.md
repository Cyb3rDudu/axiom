# Cutover Runbook: `axiom data cutover` / `axiom data rollback`

> DM09 (#318). The one irreversible-ish moment of the 0.1.x → 0.2.x
> migration, as a gated, manifest-led, resumable procedure with a real
> way back. This runbook is the operator contract; the tool is its
> enforcement. **Rehearse before every window** — the dev rehearsal
> procedure is the last section, and its evidence lives in
> `docs/diagnostics/`.

## Model

- **Plan** (`--plan`): one JSON file, `0600`, authored per window. It
  carries DSNs (source pull point, store/adoption DB, target), the
  baseline bundle path, the maintenance lease policy, the config
  switch rows, restart/stage commands and health checks. It never
  leaves the host. The plan snapshot is copied into the run directory
  at start; a resume requires the same plan (digest-pinned).
- **Run directory** (`<runs_dir>/<run-id>/`): `run.json` (the gate
  state machine — this is the resume mechanism), `plan.json`
  (snapshot), `freeze/` (the freeze delta bundle), `delta-report.json`,
  `anchor.json` (the reverse-delta base), `config-backup.json`,
  `shadow-report.json`, `audit.ndjson` (append-only, one line per gate
  event), command logs, and — after a rollback — `reverse/`,
  `reverse-delta-report.json`, `rollback.json`.
- **Confirmation**: without `--require-confirmation` both commands
  validate only (plan, baseline, connectivity) and touch **nothing**.
  Every dangerous step lives behind the flag.

## Gate sequence (cutover)

| # | Gate | What it does | Resume point |
|---|------|--------------|--------------|
| 1 | `maintenance` | runs the plan's stop commands (sync, dispatcher, repair, backfills), drains active `ingest_jobs` leases per the documented choice, takes the job-table mutation witness | re-runs stop commands (make them idempotent) |
| 2 | `freeze` | pulls the source under one `REPEATABLE READ READ ONLY` snapshot, computes the delta vs the baseline bundle (added/changed/tombstoned), lands the freeze bundle + delta report, re-checks the witness (a moving corpus aborts the run — tainted freeze) | redoes the freeze pull |
| 3 | `target-schema` | migrates the target (core + library, PostgreSQL) or opens/migrates the SQLite file; ALWAYS runs the adoption check — unverified is not prepared | idempotent |
| 4 | `delta-import` | **asserted order: refuses unless gate 3 completed in this run** (the import is data-only). Baseline-count precheck on fresh entry, delta import (window mode: the freeze is source truth, diverged rows are rewritten in place and counted), tombstone deletes (children before parents), then the **freeze anchor** (per-row digests of the target — the rollback's reverse-delta base) | row-idempotent; skips the precheck when resuming |
| 5 | `store-adoption` | applies the store migrations on the adopted DB — the DM06 cross-component FK drops run **in-window on the real database** | idempotent (ledgered) |
| 6 | `config-switch` | backs up the config.sqlite state into the run dir (once — a resume never re-snapshots mid-state), checks environment compatibility (a key the env pins would mask the file row → abort listing keys), then flips rows atomically | idempotent (converges) |
| 7 | `restart` | runs the restart command (boot **without mutating workers** — the switch rows carry e.g. `AXIOM_DISPATCHER_ENABLED=0`), then health checks | re-runs the command (idempotent restart script) |
| 8 | `shadow` | the DM08 shadow-read re-run, source vs target, full corpus — the window's **last data check**; red aborts before any worker re-enable | re-runs |
| 9 | `reenable:<stage>` | per plan stage: config rows (e.g. dispatcher on), restart command, checks | idempotent per stage |

The failed-lease choices (gate 1), documented **in the plan** and
recorded per encounter in `run.json`:

- `wait` — poll until the workers finish (bounded by
  `maintenance.wait_timeout_s`; expiry aborts the run).
- `cancel` — the repo's own cancellation semantics: pending rows cancel
  immediately, claimed/processing rows get `cancel_requested_at` and
  converge when their lease expires (the gate itself applies the
  expired-cancel terminalization — no dispatcher is alive in the
  window to do it). Timeout still bounds the total drain.
- `abort` — stop the run, listing every lease (job id, worker, lease
  expiry). The lease is left untouched.

## Rollback (`axiom data rollback --run <run-dir>`)

A rollback invocation is a **fresh pass** over its gates — every one
is safe to re-run, and the pre/post-write decision is re-evidenced
each time (the target may have drifted further since the last pass):

1. `rb:maintenance` — stop the new stack's mutators (the rollback
   plan's stop commands).
2. `rb:reverse-delta` — reads the target, diffs against the freeze
   anchor:
   - **zero drift → pre-write path**: config revert + restart-old
     only. The comparison itself is the evidence.
   - **drift → post-write path**: the reverse delta (new/changed rows
     vs the anchor) lands in `cutover_shadow_*` tables on the legacy
     database — `LIKE` the live table (types, nullability), **no FKs,
     no constraints** (reversed rows may reference entities that only
     existed on the new store), plus `applied_at`/`applied_run`
     bookkeeping. Idempotent per stable key: identical → skip,
     divergent → replace. Rows deleted on the new store are
     **reported** as tombstone keys, never auto-deleted.
3. `rb:config-revert` — restore the backed-up settings exactly (rows
   the backup carried are rewritten; rows the switch added are
   removed).
4. `rb:restart` — restart the old stack per the rollback plan + checks.

**Not owned by the tool** (runbook territory, done around the
rollback): binary/name rollback (0.1.x binaries back, renames undone)
— deployment is administrator/nix territory; the DM06 FK drops stay
applied (they are benign for 0.1.x — the constraints protected the
child tables, their absence removes protection but breaks nothing;
re-adding is a manual `ALTER TABLE` if ever required).

## Failure mid-cutover

Nothing improvises. A failed gate leaves `run.json` with the completed
gates; re-invoke with the same plan and `--run-dir <dir>`:

```bash
axiom data cutover --plan window.json --run-dir <runs_dir>/<run-id> --require-confirmation
```

Completed gates are skipped (verified, not assumed: the import's
precondition is the schema gate's completed record **of this run**);
the failed gate re-runs idempotently. Fix the cause (e.g. open the
health port, resolve the env conflict), then resume — no restart from
zero. The audit log keeps every event either way.

## Pre-window checklist

1. Fresh prod pull restored as the mirror copy (the plan's
   `source_dsn`); export → import → verify → shadow green against the
   prepared target (the rehearsed DM04/DM08 loop).
2. Baseline bundle = the pull the target was imported from. **Each
   window's delta base is the previous window's freeze state** —
   re-baseline in full after every completed window.
3. The plan's stop/restart/stage commands exist, are idempotent, and
   run in the services' env class (the config-switch gate enforces
   what that env must look like from the cutover process).
4. Dry-run the plan: `axiom data cutover --plan window.json`
   (no flag!) — plan, baseline, and every DSN verified reachable,
   nothing touched.

## Dev rehearsal (the proof procedure)

Against a restored prod copy in the dev environment — the same
procedure the acceptance evidence documents:

1. Restore the prod pull into the dev legacy copy; prepare the target
   (migrate + import the baseline); shadow green.
2. Evolve the legacy copy (new row, changed row, deleted row), plant a
   stuck lease (`claimed` + future `lease_until`).
3. Dry-run (no flag) → assert nothing moved.
4. Cutover with the flag, plan choice `cancel` → lease converges,
   delta lands, FK drops apply, config switches, shadow green,
   dispatcher staged on.
5. Rollback immediately → **pre-write** verdict.
6. Write on the new store (new + changed rows), rollback →
   **post-write**: shadow tables carry the reversed rows; rollback
   again → idempotent (no duplicates, no count drift).
7. Abort/resume sonde: fail the restart check (closed port), resume
   after opening it — the stop marker fires exactly once, the freeze
   cutoff is not recomputed.

The automated version of this procedure is
`internal/cutover/cutover_pg_it_test.go` (PG-gated);
`docs/diagnostics/` carries the human-run evidence.
