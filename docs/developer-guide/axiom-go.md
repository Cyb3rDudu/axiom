# axiom_ng (Go)

`axiom_ng` is the Go orchestrator of the system. It owns all durable
application state: Zotero synchronization, ingest jobs, leases, retries,
cancellation, persistent IDs, versioned processing snapshots, durable derived
artifacts, chunks/embeddings/entities/relationships, the PostgreSQL/pgvector
and knowledge-graph write paths, and OpenSearch outbox/index synchronization.

The Python runner computes; `axiom_ng` orchestrates, validates, and persists.

> This chapter summarizes `PROCESSOR_CONTRACT` and the resolved work order
> (`LEASE_DISPATCHER_PROCESSOR_ADAPTER_WORK_ORDER.md`, kept in git as a
> historical source). The contract structures are specified exactly in
> [PROCESSOR_CONTRACT v1](processor-contract.md).

## Core components

- **Dispatcher** — runs detached from the HTTP handlers; stable worker ID,
  configurable concurrency, poll interval with jitter, lease duration and
  renewal interval, graceful shutdown, cancel support. It negotiates
  capabilities against the processor at startup before dispatching claims.
- **Persistence** — snapshots committed atomically in a single transaction;
  the single source of truth for processing results.
- **Fencing (claim exclusivity)** — every post-claim job write is fenced by
  `job_id + worker_id + lease_token`: only the current lease owner may mutate
  an active job. A stale worker can neither complete nor fail a reclaimed job.
- **Outbox** — OpenSearch indexing is outbox-driven: an outbox entry is written
  in the same PostgreSQL transaction as the snapshot; a separate retryable
  outbox worker syncs it. An OpenSearch outage never forces Marker to rerun.

## Lease state machine

Allowed transitions:

```text
pending    -> claimed
claimed    -> processing
claimed    -> pending       retry before processor acceptance
claimed    -> failed / cancelled / skipped
processing -> pending       retryable failure or expired lease
processing -> completed / failed / cancelled

claimed/processing with expired lease -> claimed by a new owner
claimed/processing at max. attempts   -> failed (LEASE_EXHAUSTED)
```

The claim is **one** PostgreSQL statement or a short transaction using
`FOR UPDATE SKIP LOCKED`. On claim: a new random `lease_token`, set `claimed_by`,
increment `attempt` exactly once, set `lease_until`/`last_heartbeat_at` from
database time, and freeze the input snapshot + profile + idempotency key.

**Renewal:** well before expiry (e.g. every third of the lease duration),
database time instead of local wall-clock, continue during status
poll/artifact download/validation/persistence, stop after a terminal fenced
DB transition. Repeated renewal failure aborts local dispatch work.

**Fencing predicate for every post-claim update:**

```text
WHERE id = $job_id
  AND claimed_by = $worker_id
  AND lease_token = $lease_token
  AND status IN (...expected states...)
```

Zero affected rows => treat as lost lease, never as success.

## Persistence and replacement semantics

A successful result is persisted as **one** immutable processing snapshot,
identified by at least:

```text
attachment_id
content_hash
processor name and version
processing profile hash
```

Only **one** snapshot is active per document/attachment/profile. The activation
switch happens in the same transaction as the replacement insert. The
persistence transaction:

1. Insert the new snapshot + dependent rows.
2. Verify row counts and references.
3. Mark the new snapshot active, the previous one inactive.
4. Insert OpenSearch outbox work.
5. Mark the ingest job fenced as `completed`.
6. Commit once.

If any step fails, everything rolls back; the previous active snapshot remains
untouched. A partial/invalid result never replaces the last valid snapshot.

The processor result is durably persisted only after this commit; the ACK to
the processor happens only after the durable commit and is idempotent.

Continue: [PROCESSOR_CONTRACT v1](processor-contract.md) ·
[axiom_ng_runner (Python)](axiom-runner.md) ·
[References → Data Model](../references/data-model.md)
