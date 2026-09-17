# Retention / GC Maintenance Run (#281)

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
```

The run is **idempotent**: a second run reports zero removals.

## What it deletes

1. **Superseded snapshots** (not the active one) — their chunks, dense +
   sparse embeddings, entities, mentions, relationships and artifacts
   cascade with them, plus their drained (`done`) outbox rows.
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
