# Retention / GC Maintenance Run (#281, execution hardening #290)

The database should tell the truth at first glance: superseded snapshots,
stale job attempts, and stale derived counters accumulate otherwise — a
measurement summed over ALL snapshots instead of the latest (252 chunks =
127 stale + 125 current), the outcome view alarmed over historical residue.

The retention run is **operator tooling, not an auto-pilot**: nothing here
ever deletes without an explicit invocation, and the default is the
**dry run** (exact counts only).

## Running it

```bash
# dry run (default) — exact removal counts, nothing deleted
/opt/axiom/bin/axiom-ng -maintenance-retention

# real run
/opt/axiom/bin/axiom-ng -maintenance-retention --apply

# job retention age override (default 14 days)
AXIOM_RETENTION_JOB_DAYS=30 /opt/axiom/bin/axiom-ng -maintenance-retention --apply

# execution form overrides (#290)
/opt/axiom/bin/axiom-ng -maintenance-retention --apply --timeout=4h --batch=10
# or via env: AXIOM_RETENTION_TIMEOUT=4h, AXIOM_RETENTION_BATCH=10
# (an explicit flag WINS over the env var; --timeout 4h with a space
# works too)
```

The run is **idempotent**: a second run reports zero removals.

## Execution form (#290)

The first production `--apply` (2026-09-18/19) proved the plan correct
but the execution form fragile: the single monolithic DELETE ran ~40 min
with zero bytes flowing back to the client, the host<->VM port-forward
dropped the silently idle mapping, the transaction rolled back unseen,
and the Go client hung 9 h because nothing bounded the run. The apply
therefore now:

- **deletes in tranches** — up to `--batch` snapshots (default 25) per
  transaction, commit between tranches, then the stale job attempts under
  the same discipline. No single transaction runs longer than one
  tranche's worth of cascade (default ≈ a few minutes at the measured
  ~13 s per snapshot). Every tranche locks its candidate rows
  (`SELECT … FOR UPDATE`) and its DELETE re-evaluates the keep-guards, so
  a snapshot reactivated or a job re-queued while the tranche is in
  flight survives — the never-delete rules hold under concurrency, not
  just on a quiescent DB;
- **unlinks artifact files per committed tranche** — a run aborted after
  the first tranche (deadline, dead connection, kill) has already freed
  the bytes of everything it committed. Residual window: a hard kill
  between a tranche's commit and its unlink can orphan that one
  tranche's files (no DB row references them; they are inert and can be
  swept manually);
- **is deadline-bounded** — the whole run gets `--timeout=` (default 2 h,
  or `AXIOM_RETENTION_TIMEOUT`; the flag beats the env). A dead
  connection or expired deadline fails the run loudly with exit 1
  instead of hanging forever — including mid-query: an in-flight
  tranche blocked on a lock is cancelled at the deadline;
- **logs one progress line per tranche** — `apply progress: snapshots
  removed X/Y` — so a long run is observable without WAL forensics.

An interrupted run is safe at every tranche boundary: committed tranches
stay removed, the rest survives untouched, and a **re-run simply
resumes** (a subsequent run reports zero once the backlog is drained).
Concurrent runs are not corrupted (the candidate locks serialize them on
the same rows) but there is no reason to race two operators: one run at
a time.

Ops note for very long maintenance runs: executing inside the container
network (past the port-forward) is the most robust environment; with
batched deletes this is mostly unnecessary.

## What it deletes

1. **Superseded snapshots** (not the active one), in tranches of `--batch`
   — their chunks, dense +
   sparse embeddings, entities, mentions, relationships and artifacts
   cascade with them, plus their drained (`done`) outbox rows. The
   durable artifact FILES under `AXIOM_ARTIFACT_ROOT` are unlinked after
   the commit (a failed unlink leaves an orphaned file, reported — never a
   dangling row).
2. **Stale terminal job attempts** (`completed`/`failed`/`cancelled`/
   `skipped`) older than the retention age (default 14 d) that are not the
   document's latest job.

## What it never deletes

- the **latest job per document** — it IS the outcome truth;
- every job whose attachment has **any repair case** (heal forensics);
- every job that **produced an active snapshot**;
- every **non-terminal** job (`pending`/`claimed`/`processing`);
- superseded snapshots with **still-relevant outbox rows** (pending, or
   terminal-but-recoverable failed) — their OpenSearch delete is the only
   path that removes the stale index docs; they become removable once the
   row is gone or marked done;
- the **active snapshot** and everything under it.

Because the latest job per document is untouchable, the outcome-truth
derivation cannot flip (no completed document turns failed/pending) —
pinned by the fixture IT with before/after assertions.

## When to run

After larger rebuild waves or heal campaigns, and whenever snapshot/job
counts look inflated relative to the corpus. The dry run is always safe;
against a production copy it is the acceptance instrument (organize with
the owner before a real production run). Deleted means gone — there is no
archive.
