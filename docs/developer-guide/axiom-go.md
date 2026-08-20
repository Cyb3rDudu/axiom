# axiom dispatcher

`axiom_ng` is the Go orchestrator of the system. It owns all durable
application state: Zotero synchronization, ingest jobs, leases, retries,
cancellation, persistent IDs, versioned processing snapshots, durable derived
artifacts, chunks/embeddings/entities/relationships, the PostgreSQL/pgvector
and knowledge-graph write paths, and OpenSearch outbox/index synchronization.

The Python runner computes; `axiom_ng` orchestrates, validates, and persists.

The runner contract structures are specified in
[Processor Contract](processor-contract.md). The client-facing Go routes are
specified in the [HTTP API reference](../references/api.md).

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
- **HTTP API** — owns Zotero sync and selection, ingest visibility, search and
  passage retrieval, knowledge-graph reads and consolidation, signed processor
  source delivery, and the conditionally wired repair surface.
- **Graph hygiene** — filters frontmatter-backed entities and relationships
  before persistence, then consolidates exact canonical-form duplicates after
  successful sync bursts or on an explicit administrative call.

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
4. **Persist (atomic)** — one transaction inserts or force-refreshes the
   versioned snapshot, marks it active and other identities inactive, inserts
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

A successful result is persisted under one versioned processing-snapshot
identity, defined by:

```text
attachment_id
content_hash
processor name and version
processing profile hash
```

Only **one** snapshot is active per attachment, across profiles. The activation
switch happens in the same transaction as the insert or force replacement. The
persistence transaction:

1. Insert a new snapshot identity, or refresh the existing identity for a
   forced rebuild, plus dependent rows.
2. Verify row counts and references.
3. Mark the new snapshot active, the previous one inactive.
4. Insert OpenSearch outbox work.
5. Mark the ingest job fenced as `completed`.
6. Commit once.

If any step fails, everything rolls back; the previous active snapshot remains
untouched. A partial/invalid result never replaces the last valid snapshot.

### Snapshot generations, superseded generations, and the tombstone outbox

Snapshot identities carry a generation counter:

- **Activation rule:** only one snapshot is `active` per attachment. A new
  identity inserts a new row and deactivates the previous active row in the
  same transaction (enforced by migration `0011` at the DB level).
- **Force replacement:** `force_rebuild` with the same identity reuses the
  snapshot ID, removes its child rows, writes the fresh result, and increments
  `generation` atomically. This is the deliberate exception to append-only
  snapshot rows.
- **Superseded identities:** a row superseded by a different identity remains
  stored and inactive. The search index must therefore **forget** its chunks.
- **Tombstone outbox:** forgetting is handled by the outbox. The same
  transaction that deactivates a generation inserts an outbox **delete-ops**
  record. The record commits atomically with the DB switch; the asynchronous
  drainer then removes the superseded chunks. Without this, a `force_rebuild`
  would leave orphaned docs in the index.

**Convergence invariant:** after pending outbox work drains, the OpenSearch
doc-count equals the active snapshots' chunk-count. Temporary divergence is
expected while index/delete operations are pending; persistent divergence after
the drain indicates a missed or obsolete operation (see Operations →
Troubleshooting).

### Failure semantics

The processor result is durably persisted only after the commit; the ACK to the
processor happens only after the durable commit and is idempotent. Outbox work
(a DRY operation) is created in the same transaction as the snapshot, so an
OpenSearch outage never re-runs Marker — the drainer retries until the index
catches up.

## Zotero sync and standing consolidation

The sync service reads the canonical Zotero delta, computes attachment file
facts before opening the apply transaction, and commits the canonical mirror,
normalized document/attachment projections, collection memberships, selected
ingest jobs, and library cursor together. The attachment content hash is the
work gate: metadata-only changes do not enqueue duplicate processing.

A successful sync schedules exact-form entity consolidation on a 10-second
debounce. Consecutive successful syncs reset the timer and collapse into one
run. Consolidation uses a detached 30-minute context because the HTTP request
has already completed; failures are logged and do not retroactively fail the
sync. Shutdown cancels a pending timer. The operation is also exposed as
`POST /api/kg/consolidate` and the one-shot `-consolidate-entities` command.

## Persistence gates

Processor output crosses three boundaries before it becomes active product
state:

1. Contract validation checks source identity, contiguous chunk indexes,
   unique and resolvable refs, embedding dimensions and values, locator trust,
   relation evidence, statistics, and artifact declarations.
2. Verified artifact records must match the fetched bytes' digest, length,
   media type, and ref.
3. The graph frontmatter gate removes gated mentions and unsupported entities
   and relations before insert. It does not remove the underlying chunks from
   retrieval.

Only then does one fenced transaction persist the snapshot, switch activation,
write OpenSearch index/delete outbox operations, and mark the job completed.

## Configuration wiring

`axiom_ng` reads its configuration entirely from `AXIOM_*` environment
variables at startup (see the [complete table](configuration.md)). The
`DispatcherProfile` is frozen into each job's input snapshot at claim time;
changing it affects only newly claimed jobs, not in-flight ones.

Continue: [Processor Contract](processor-contract.md) ·
[axiom runner](axiom-runner.md) ·
[References → Data Model](../references/data-model.md)
