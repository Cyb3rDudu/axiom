# axiom dispatcher

`axiom_ng` is the Go orchestrator of the system. It owns all durable
application state: Zotero synchronization, ingest jobs, leases, retries,
cancellation, persistent IDs, versioned processing snapshots, durable derived
artifacts, chunks/embeddings/entities/relationships, the PostgreSQL/pgvector
and knowledge-graph write paths, and OpenSearch outbox/index synchronization.

The Python runner computes; `axiom_ng` orchestrates, validates, and persists.

> This chapter summarizes `PROCESSOR_CONTRACT` and the resolved work order
> (`LEASE_DISPATCHER_PROCESSOR_ADAPTER_WORK_ORDER.md`, kept in git as a
> historical source). The contract structures are specified exactly in
> [Processor Contract](processor-contract.md).

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

## Claim → persist lifecycle (the fence in five ideas)

The dispatcher drives each job through five fenced steps:

```text
claim  → submit  → validate+fetch → persist (atomic) → ack
 |         |             |               |             |
 fence     fence         fence           fence         idempotent
```

1. **Claim** — one atomic `FOR UPDATE SKIP LOCKED` acquires the lease; only this
   lease owner may touch the job until it is released or expires.
2. **Submit** — the dispatcher builds the contract request from the frozen input
   snapshot (`job_id + idempotency_key`) and `POST /v1/process`. The runner
   accepts asynchronously (202).
3. **Validate + fetch** — while renewing the lease, the dispatcher polls the
   runner, then fetches and validates the result and each artifact. Everything
   the runner returns is untrusted until validated.
4. **Persist (atomic)** — one transaction inserts the immutable snapshot,
   marks the new generation active and the previous one inactive, inserts
   OpenSearch outbox work, and marks the job `completed`. Any failure rolls
   back and keeps the previous active snapshot.
5. **Ack** — only after the durable commit, the dispatcher POSTs the
   acknowledgement; the runner may then delete its temporary output. ACK is
   idempotent and never re-runs compute.

**Why fencing matters:** every post-claim step is a separate operation, and
workers can stall or restart mid-way. If a reclaimed job let a stale worker
write, two workers could race on one job. The fence (`job_id + worker_id +
lease_token` on every post-claim predicate, zero-rows ⇒ lost-lease) guarantees
that only the current lease holder can ever complete, fail, or cancel a job —
so a reclaimed job can never be double-completed or completed by an observer
who no longer owns it.

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

### Snapshot generations, superseded generations, and the tombstone outbox

Snapshots are **generations**, not rows to update in place:

- **Activation rule:** only one snapshot is `active` per attachment. On a
  replace (e.g. a `force_rebuild`), the new snapshot becomes active in the same
  transaction that deactivates the old one (enforced by migration `0011` →
  `one active per attachment` at the DB level, not just in app logic).
- **Superseded generations:** an old snapshot stays immutable in the store for
  audit/recovery; it is only ever marked inactive — never deleted or overwritten.
  The search index must therefore **forget** the superseded snapshot's chunks.
- **Tombstone outbox:** forgetting is handled by the outbox. The same
  transaction that deactivates a generation inserts an outbox **delete-ops**
  record, so the OpenSearch drainer removes the superseded chunks atomically
  with the DB switch. Without this, a `force_rebuild` would leave orphaned docs
  in the index.

**Invariant:** at any moment, the OpenSearch doc-count equals the active
snapshots' chunk-count. A divergence between index and active snapshot is the
symptom of a missed tombstone/obsolete step (see Operations → Troubleshooting).

### Failure semantics

The processor result is durably persisted only after the commit; the ACK to the
processor happens only after the durable commit and is idempotent. Outbox work
(a DRY operation) is created in the same transaction as the snapshot, so an
OpenSearch outage never re-runs Marker — the drainer retries until the index
catches up.

## Configuration wiring

`axiom_ng` reads its configuration entirely from `AXIOM_*` environment
variables at startup (see the [complete table](configuration.md)). The
`DispatcherProfile` is frozen into each job's input snapshot at claim time;
changing it affects only newly claimed jobs, not in-flight ones.

Continue: [Processor Contract](processor-contract.md) ·
[axiom runner](axiom-runner.md) ·
[References → Data Model](../references/data-model.md)
