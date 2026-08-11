# Lease, Dispatcher, and Python Processor Adapter Work Order

**Status:** Handoff document for new implementation and review sessions  
**Target branch:** `feat/axiom-ng-zotero-rag`  
**Baseline:** Part 2 Zotero mirror and metadata synchronization are considered complete  
**Binding contract:** [`PROCESSOR_CONTRACT.md`](./PROCESSOR_CONTRACT.md)  
**Architecture plan:** [`../../docs/plans/AXIOM_NG_ZOTERO_DESKTOP.md`](../../docs/plans/AXIOM_NG_ZOTERO_DESKTOP.md)

## 1. Purpose

The next development step is to turn pending `ingest_jobs` into durable RAG
processing snapshots through a replaceable Python document processor.

This work package implements the complete path:

```text
pending ingest_job
    -> atomic lease claim in Go
    -> fenced Go dispatcher
    -> asynchronous Python processor API
    -> Marker/EPUB conversion, chunking, embeddings, entities, relations
    -> validated result returned to Go
    -> atomic durable snapshot persistence owned by Go
    -> processor acknowledgement and temporary-file cleanup
    -> completed ingest_job
```

The objective is not merely to call Python. The objective is a restart-safe,
idempotent and observable processing system in which axiom-ng remains the sole
owner of durable state.

## 2. Product Goal

Every preferred PDF or EPUB mirrored from Zotero must be processable without
copying the source file and without allowing the Python service to write to
PostgreSQL, pgvector, OpenSearch, the knowledge graph, or Zotero.

After this work package, one real Zotero attachment must be able to move from
`pending` to `completed`, with:

- A verified immutable processing snapshot.
- Durable normalized Markdown.
- Chunks with physical and logical source locators.
- Section hierarchy and paragraph indexes.
- Dense and sparse embeddings when requested.
- Entities, entity occurrences, chunk relationships and evidence-backed entity
  relationships when requested.
- Processor identity, model identity and profile hash recorded.
- No durable copy of the original PDF or EPUB outside Zotero.
- No partial result replacing the last valid snapshot.

RAG search and chat APIs are the following work package. This package must
produce the durable data and consistency guarantees those APIs need.

## 3. Current Status and Trusted Baseline

### 3.1 Zotero synchronization

The system has one public synchronization path:

```text
POST /api/zotero/sync -> sync.Service.Run() -> canonical lossless sync
```

The preliminary parallel sync writer has been removed. There must not be a
second sync implementation writing to the same projection tables.

The canonical sync currently provides:

- Lossless `zotero_items.raw_envelope` and `raw_data` JSONB storage.
- Canonical collection hierarchy and item memberships.
- Normalized document metadata projections.
- Attachment projections with native path, original file URI, hash, file size,
  mtime, link mode and preferred flag.
- Notes and annotations stored canonically but never enqueued as documents.
- Exactly one preferred processable attachment per projected document.
- PDF preference over EPUB.
- Version guards, deletion/restore behavior and per-source serialization.
- Atomic item, projection, ingest-job and cursor writes.
- Metadata-only updates without duplicate processing for an unchanged hash.

At handoff, the real local library acceptance target is:

```text
canonical items:       39
collections:           20
document projections:  16
attachments:           23
preferred attachments: 16
item memberships:      40
```

New sessions must verify the current `HEAD` and tests rather than assuming a
specific commit hash. Do not reopen the single-sync architecture unless a
concrete regression is found.

### 3.2 Existing queue schema

`ingest_jobs` already contains:

- `status`: `pending`, `claimed`, `processing`, `completed`, `failed`,
  `cancelled`, `skipped`.
- Source, document and attachment foreign keys.
- `content_hash` and `force_rebuild`.
- `claimed_by`, `lease_until`, `attempt`, `max_attempts`.
- Processor name/version and JSON result fields.
- Error fields and timestamps.
- `resolved_at` for historical file-resolution failures.
- A partial uniqueness rule for normal attachment/hash jobs.

The existing schema is a starting point, not a complete lease protocol. Add an
additive migration for fencing, retry scheduling and immutable input data.

### 3.3 Existing Python processing behavior to preserve

The existing Python application contains working logic that must be wrapped,
not rewritten from scratch:

- `axiom_backend/services/background_document_processor.py`
- `axiom_backend/ai_researcher/pdf_worker/`
- `axiom_backend/ai_researcher/epub_worker/`
- `axiom_backend/ai_researcher/core_rag/chunker.py`
- `axiom_backend/ai_researcher/core_rag/embedder.py`
- `axiom_backend/ai_researcher/core_rag/entity_extractor.py`
- `axiom_backend/ai_researcher/core_rag/relation_extractor.py`
- `axiom_backend/ai_researcher/core_rag/graph_store.py`

The active old ingestion path proves that Marker pagination, logical PDF page
labels, chunk locators, GLiNER and mREBEL work. The adapter must extract the
compute behavior from this path while removing all durable-store writes.

### 3.4 Ownership boundary

Zotero owns source PDF/EPUB files and bibliographic truth.

axiom-ng owns:

- Sync state and metadata mirrors.
- Ingest jobs, leases, retries and cancellation.
- Processor input snapshots.
- Result validation and durable processing snapshots.
- Markdown and referenced derived artifacts.
- Chunks, embeddings, entities and graph data.
- PostgreSQL/pgvector persistence.
- OpenSearch outbox and indexing state.

The Python processor owns computation and temporary job output only. It must
not own application data.

## 4. Non-Negotiable Invariants

1. Only the current lease owner may mutate an active job.
2. Every job mutation after claim is fenced by a unique lease token.
3. A stale worker cannot complete, fail, retry or cancel a reclaimed job.
4. No database transaction remains open during processor HTTP calls or model
   execution.
5. Repeating a processor request with the same idempotency key does not start
   duplicate compute work.
6. A processor result is untrusted input until fully validated by Go.
7. Result persistence is atomic. Failure preserves the previous active
   snapshot.
8. A job becomes `completed` only after required result data and artifacts are
   durably committed.
9. Processor acknowledgement happens only after durable commit and is
   idempotent.
10. Processor or axiom-ng restarts do not lose accepted work.
11. Expired leases are recoverable until `max_attempts` is exhausted.
12. Retryable failures use bounded backoff; deterministic failures become
    terminal.
13. A changed attachment hash cannot complete an older job as the current
    document snapshot.
14. Python never receives Zotero, PostgreSQL, OpenSearch or application API
    credentials.
15. Original PDF/EPUB files are read in place and never durably copied.

## 5. Required Deliverables

### 5.1 Go queue repository

Implement repository operations with explicit types and no ad hoc SQL in HTTP
handlers:

- `ClaimNextJob`
- `RenewLease`
- `MarkProcessing`
- `ScheduleRetry`
- `MarkFailed`
- `MarkCompleted`
- `MarkCancelled`
- `MarkSkipped`
- `RequestCancellation`
- `ReleaseOrExpireLease`
- `GetJob`
- `ListJobs`
- `RecoverExpiredJobs` or equivalent bounded recovery

Every operation after claim must require:

```text
job_id + worker_id + lease_token
```

and must return a typed lost-lease result when zero rows are affected.

### 5.2 Go processor client

Implement a typed client for every endpoint required by contract v1:

```http
GET  /v1/health
GET  /v1/capabilities
POST /v1/process
GET  /v1/jobs/{job_id}
GET  /v1/jobs/{job_id}/result
GET  /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST /v1/jobs/{job_id}/cancel
POST /v1/jobs/{job_id}/ack
```

Requirements:

- Separate connect, request and polling timeouts.
- Bounded response bodies.
- Strict JSON decoding for required fields while permitting additive unknown
  contract fields.
- Stable typed error mapping.
- No automatic retry of non-idempotent requests unless the idempotency contract
  makes the retry safe.
- Processor base URL configurable and loopback by default.

### 5.3 Go dispatcher

Implement a dispatcher service with:

- Stable worker ID generated/configured per process.
- Configurable concurrency, bounded by processor capabilities.
- Poll interval with jitter.
- Lease duration and renewal interval.
- Graceful shutdown and cancellation.
- Capability negotiation before claims are dispatched.
- Fenced state transitions.
- Retry classification and bounded exponential backoff.
- Recovery after axiom-ng or processor restart.
- Structured logs containing job ID, attempt, lease token prefix, document ID,
  attachment ID and stage, but no document content.

The dispatcher must run as part of axiom-ng only when explicitly enabled by
configuration. Tests must not start an uncontrolled background loop.

### 5.4 Python processor adapter

Implement a loopback HTTP service conforming exactly to
`PROCESSOR_CONTRACT.md`.

Recommended location:

```text
axiom_backend/processor_service/
  __init__.py
  __main__.py
  app.py
  models.py
  job_store.py
  runner.py
  adapter.py
  validation.py
  tests/
```

Use the Python framework and dependency conventions already present in
`axiom_backend`. Do not introduce a second web framework without a concrete
need.

The adapter must:

- Bind to `127.0.0.1` by default.
- Validate allowed source roots and regular-file readability.
- Verify the supplied source hash.
- Accept asynchronously and return `202` quickly.
- Deduplicate by `idempotency_key`.
- Persist enough temporary operational state to survive a service restart.
- Limit heavy processing concurrency, initially to one job.
- Support status, result, artifact download, cancellation and acknowledgement.
- Preserve temporary results until acknowledgement or configured expiry.
- Remove temporary output after acknowledgement.
- Never import or invoke DB, OpenSearch or Zotero write paths.

### 5.5 Pure Python compute boundary

Create a pure computation interface that can be tested without the HTTP layer:

```python
def process_document(request: ProcessRequest, work_dir: Path) -> ProcessorResult:
    ...
```

Equivalent class-based APIs are acceptable. The important boundary is:

- Input: immutable request plus source path.
- Output: contract result plus temporary artifact files.
- No database session.
- No GraphStore persistence.
- No OpenSearch client.
- No Zotero API access.

Reuse existing conversion and ML components behind this boundary. Do not copy
the entire `background_document_processor.py` and then disable writes with
flags. Extract reusable computation or introduce small adapters around the
existing conversion/chunking/extraction APIs.

### 5.6 Go result validation and persistence

Implement result validation before persistence. At minimum validate all rules
from Processor Contract section 14:

- Contract and job identity.
- Attachment identity and exact content hash.
- Processor and processing-profile identity.
- Unique job-local references.
- Contiguous chunk indexes.
- Locator validity.
- Dense dimensions and finite values.
- Sparse key/value validity.
- Entity mention bounds.
- Relationship references and evidence.
- Artifact media type, size and digest.
- Declared statistics against actual arrays.

Persist one immutable snapshot. Add additive migrations for the minimum durable
model required by the contract. The schema must represent:

- Processing snapshot identity and activation state.
- Processor/profile/model provenance.
- Manifest and warnings.
- Durable artifacts and their verified digests.
- Chunks and locators.
- Dense and sparse embeddings.
- Entities and chunk occurrences.
- Chunk and entity relationships with evidence.
- An OpenSearch outbox entry generated in the same PostgreSQL transaction.

Do not have Python write old RAG tables as a shortcut. If existing PostgreSQL
tables are reused, document the mapping and prove that snapshot replacement,
source scoping and provenance remain correct.

The persistence transaction must:

1. Insert the new immutable snapshot and all dependent rows.
2. Verify row counts and references.
3. Mark the new snapshot active and the previous one inactive.
4. Insert OpenSearch outbox work.
5. Mark the ingest job completed with fenced ownership.
6. Commit once.

If any step fails, rollback everything and retain the previous active snapshot.

## 6. Lease State Machine

### 6.1 Required schema additions

Create a new ordered migration. Do not edit already-applied migrations.

At minimum add:

```text
lease_token UUID
next_attempt_at TIMESTAMPTZ
last_heartbeat_at TIMESTAMPTZ
cancel_requested_at TIMESTAMPTZ
input_snapshot JSONB
processing_profile JSONB
profile_hash TEXT
idempotency_key TEXT
processor_job_id TEXT or an explicitly documented same-as-job-id rule
```

Use `NOT NULL` only when old rows can be safely backfilled. Add indexes matching
the real claim predicate. Do not add indexes by intuition without checking the
query plan.

### 6.2 Allowed transitions

```text
pending    -> claimed
claimed    -> processing
claimed    -> pending       retry before processor acceptance
claimed    -> failed
claimed    -> cancelled
claimed    -> skipped
processing -> pending       retryable failure or expired recoverable lease
processing -> completed
processing -> failed
processing -> cancelled

claimed/processing with expired lease -> claimed by a new owner
claimed/processing at max attempts    -> failed (LEASE_EXHAUSTED)
```

Terminal states must not return to `pending` except through an explicit user
retry operation that creates a new attempt or deliberately resets the job under
a documented rule.

### 6.3 Claim semantics

The claim operation must be one PostgreSQL statement or one short transaction
using `FOR UPDATE SKIP LOCKED`.

Eligibility:

- `pending` and `next_attempt_at <= now()`.
- Or `claimed`/`processing` with an expired lease.
- `attempt < max_attempts`.
- Not cancellation-requested.
- Source, document and attachment still exist and are active.
- Attachment is still preferred.
- Job content hash still matches the current attachment content hash, unless
  this is an explicit force rebuild with frozen input.

On claim:

- Generate a new random `lease_token`.
- Set `claimed_by`.
- Increment `attempt` exactly once.
- Set `status='claimed'`.
- Set `lease_until` and `last_heartbeat_at` from database time.
- Freeze `input_snapshot`, processing profile and idempotency key if not already
  frozen.
- Return all data required to build the processor request.

Jobs that are obsolete because the document/attachment was deleted, replaced,
unpreferred or hash-mismatched must become `skipped` with a stable reason. They
must not remain at the head of the queue forever.

### 6.4 Fencing

Every update by a dispatcher worker must include:

```sql
WHERE id = $job_id
  AND claimed_by = $worker_id
  AND lease_token = $lease_token
  AND status IN (...expected states...)
```

Lease renewal must fail once the lease was reclaimed. A worker that receives a
lost-lease result must stop local state mutation and must not persist or
acknowledge a processor result.

### 6.5 Lease renewal

- Renew substantially before expiry, for example every one-third of the lease
  duration.
- Use database time, not local wall-clock comparisons.
- Continue renewal while waiting for processor status, downloading artifacts,
  validating and persisting.
- Stop renewal after a terminal fenced DB transition.
- Repeated renewal failure must cancel local dispatch work. It must not be
  interpreted as successful completion.

### 6.6 Attempts and retries

Classify failures into:

- Transport/transient processor failures: retryable.
- Processor error with `retryable=true`: retryable.
- Validation failures: terminal unless explicitly proven transient.
- Source missing/hash mismatch/unsupported format: terminal.
- Lost lease: neither success nor failure by the stale worker.

Use bounded exponential backoff with jitter. Store `next_attempt_at` in the DB.
Do not sleep inside a transaction and do not hold a lease merely to wait for a
retry window.

When attempts are exhausted, set:

```text
status=failed
error_code=LEASE_EXHAUSTED or RETRY_EXHAUSTED
lease fields cleared
completed_at=now()
```

## 7. Dispatcher Algorithm

For each worker slot:

1. Check shutdown state.
2. Claim one eligible job atomically.
3. If none exists, wait for the poll interval with jitter.
4. Build the contract request only from the frozen input snapshot.
5. Confirm the processor supports contract version, content type and requested
   features.
6. `POST /v1/process` with job ID and idempotency key.
7. Fenced transition `claimed -> processing` after acceptance.
8. Poll processor status while renewing the lease.
9. If cancellation was requested, call processor cancel and converge to
   `cancelled` after confirmation or safe timeout handling.
10. On processor failure, apply retry or terminal failure policy.
11. On completion, fetch and validate the result.
12. Stream each required artifact to a temporary axiom-ng path while hashing;
    verify type, length and digest.
13. Persist snapshot, rows, artifacts metadata, outbox and completed job in one
    fenced PostgreSQL transaction.
14. Move/commit verified durable artifacts according to an atomic artifact
    strategy. The DB must never reference an unavailable final artifact.
15. Call processor acknowledgement after durable commit.
16. If acknowledgement fails, retain completed state and retry acknowledgement
    separately. Never rerun processing solely because ACK failed.

The implementation must explicitly document the artifact commit strategy. A
staging path plus atomic rename on the same filesystem is preferred for local
v1. Crash recovery must clean unreferenced staging files without deleting
referenced artifacts.

## 8. Processor Request Construction

Use the request schema in `PROCESSOR_CONTRACT.md` without inventing an alternate
payload.

The immutable metadata snapshot comes from the canonical Zotero/document rows
and must include all bibliographic values required later for provenance. Missing
values stay null; no LLM enrichment occurs.

The idempotency key must include:

```text
attachment_id + content_hash + profile_hash
```

Serialize deterministically before hashing the processing profile. Record the
exact profile JSON and hash in the job and snapshot.

For local v1, `local_path` must be an absolute native path under an allowed
Zotero root. Go should preflight it; Python independently verifies it.

## 9. Python Adapter Execution Model

### 9.1 Temporary state

Use a configured processor work root outside the Zotero library. Each job gets
an isolated directory keyed by job ID. Store a small state manifest or local
SQLite record sufficient to recover:

- Request and idempotency key.
- Accepted/running/terminal state.
- Current stage and progress.
- Error details.
- Result manifest path.
- Artifact paths and acknowledgement state.

This state is operational and temporary, not application truth.

### 9.2 Concurrency

- Default `max_concurrent_jobs=1` because Marker and GPU models are heavy.
- Use subprocess boundaries already present for Marker and EPUB conversion.
- Cancellation must terminate owned subprocesses.
- Do not run multiple Marker jobs merely because the HTTP server has multiple
  request workers.
- Cap request queue size and reject overload predictably.

### 9.3 Compute stages

Expose stable stages such as:

```text
validate_source
convert
normalize_markdown
chunk
embed
extract_entities
extract_relationships
assemble_result
completed
```

Optional requested stages may produce warnings only where the processing
profile permits degradation. Required stages fail the job.

### 9.4 PDF and EPUB provenance

PDF chunks must preserve:

- Zero-based physical page indexes.
- Logical page labels as strings.
- Marker pagination source.
- Ordered section hierarchy.
- Start/end paragraph indexes.

EPUB chunks may use EPUB CFI or another real EPUB locator. Never invent page
numbers for an EPUB without stable pagebreak labels.

## 10. Go Persistence and Replacement Semantics

### 10.1 Snapshot identity

At minimum, uniqueness must include:

```text
attachment_id
content_hash
processor_name
processor_version
profile_hash
```

Replaying the same completed result must return the existing snapshot and remain
safe. A force rebuild may create a new snapshot only under an explicit identity
or generation rule.

### 10.2 Active snapshot

Only one snapshot is active for a document/attachment/profile scope. Switching
the active snapshot occurs in the same transaction that inserts the replacement
data. Old snapshots remain immutable for audit/recovery until a separate
retention policy removes them.

### 10.3 OpenSearch

Do not call OpenSearch inside the snapshot transaction. Insert an outbox record
containing the snapshot identity and intended operation. A separate retryable
outbox worker may be minimal in this work package, but the transactionally
created outbox record is mandatory.

### 10.4 Knowledge graph

Graph rows must be scoped to the new snapshot or its durable chunk/entity IDs.
Evidence chunk references are mandatory for non-sequential relationships. A
failed graph insert rolls back the new PostgreSQL snapshot; Python must not
write graph rows directly.

## 11. Configuration

Add explicit configuration with conservative defaults:

```text
AXIOMNG_DISPATCHER_ENABLED=false
AXIOMNG_DISPATCHER_WORKER_ID=<generated stable process id>
AXIOMNG_DISPATCHER_CONCURRENCY=1
AXIOMNG_DISPATCHER_POLL_INTERVAL=2s
AXIOMNG_DISPATCHER_LEASE_DURATION=120s
AXIOMNG_DISPATCHER_HEARTBEAT_INTERVAL=30s
AXIOMNG_PROCESSOR_BASE_URL=http://127.0.0.1:<port>
AXIOMNG_PROCESSOR_REQUEST_TIMEOUT=30s
AXIOMNG_PROCESSOR_POLL_INTERVAL=2s
AXIOMNG_ARTIFACT_ROOT=<configured durable derived-artifact root>

AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
AXIOM_PROCESSOR_PORT=<port>
AXIOM_PROCESSOR_WORK_ROOT=<temporary processor root>
AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=<Zotero storage roots>
AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS=1
AXIOM_PROCESSOR_RESULT_RETENTION=<safe recovery duration>
```

Validate relationships such as heartbeat interval being shorter than lease
duration. Invalid production configuration must fail startup clearly.

## 12. API Scope

Keep the existing endpoints and add the queue operations needed for operations:

```http
GET  /api/ingest/jobs
GET  /api/ingest/jobs/{id}
POST /api/ingest/jobs/{id}/retry
POST /api/ingest/jobs/{id}/cancel
```

The dispatcher itself does not need a public claim endpoint. Claiming is an
internal repository operation. Do not expose lease tokens through the REST API.

The API remains loopback-bound by default. Do not add remote auth design in this
work package.

## 13. Implementation Sequence and Review Gates

Implement in small commits in this order. Stop for review at each gate, but do
not ask for architectural choices already settled in this document.

### Gate 0: Baseline verification

- Confirm the single canonical `/api/zotero/sync` path.
- Run build, vet, unit tests and DB integration tests.
- Record current source-scoped live counts.
- Do not modify unrelated dirty worktree files.

### Gate 1: Queue migration and lease repository

- Add migration and typed queue operations.
- Add concurrent claim, fencing, heartbeat, expiry, exhaustion, stale-job and
  cancellation tests against an isolated PostgreSQL database.
- No dispatcher loop yet.

### Gate 2: Processor client and dispatcher with fake processor

- Implement typed Go client.
- Implement dispatcher lifecycle using an `httptest` fake processor.
- Prove retries, restart recovery, lost lease, cancellation and ACK recovery.
- No real Marker execution yet.

### Gate 3: Python processor black-box service

- Implement all contract endpoints and temporary job state.
- Wrap PDF and EPUB conversion first, then chunking and requested ML stages.
- Pass the processor contract black-box suite.
- Prove no database/OpenSearch/Zotero writes.

### Gate 4: Result validation and Go persistence

- Add snapshot/RAG/outbox migrations.
- Implement validation and atomic persistence.
- Test invalid and partial results cannot replace an active snapshot.
- Test artifact digest mismatch and crash-safe staging.

### Gate 5: Real local end-to-end smoke test

- Start Python processor on loopback.
- Start axiom-ng dispatcher with concurrency one.
- Select one small real Zotero PDF first.
- Observe `pending -> claimed -> processing -> completed`.
- Verify Markdown, chunks, locators, embeddings, entities and graph rows.
- Verify ACK cleanup and no source-file copy.
- Restart both services during controlled test jobs and prove recovery.
- Then process the remaining preferred attachments.

Do not deploy to another host or process all 16 documents until the one-document
smoke test and review pass.

## 14. Required Tests

### 14.1 Lease repository integration tests

1. Two concurrent claimers never receive the same job.
2. `SKIP LOCKED` allows different jobs to be claimed concurrently.
3. Claim increments attempt once and creates a new lease token.
4. Heartbeat extends only the matching lease.
5. A stale token cannot update a reclaimed job.
6. Expired jobs are reclaimed below max attempts.
7. Expired jobs become terminal when attempts are exhausted.
8. Future `next_attempt_at` jobs are not claimed.
9. Cancel-requested jobs are not newly dispatched.
10. Deleted/unpreferred/hash-stale jobs become skipped.
11. Forced rebuild semantics remain explicit and tested.
12. Claim input snapshot is immutable across retries.

### 14.2 Dispatcher tests with fake processor

1. Accepted job transitions to processing.
2. Duplicate POST after transport ambiguity is deduplicated.
3. Lease renews during long processing.
4. Lost lease prevents result persistence and ACK.
5. Retryable processor failure schedules backoff.
6. Non-retryable failure becomes terminal.
7. Processor timeout and restart recover through the same job ID.
8. Cancellation calls processor cancel and converges safely.
9. Completed result is validated before persistence.
10. ACK failure does not rerun completed processing.
11. Graceful shutdown stops claiming and releases/recoverably expires work.

### 14.3 Python processor black-box tests

Run all tests listed in Processor Contract section 19, including:

- Idempotency.
- PDF and EPUB processing.
- Hash mismatch.
- Page and section provenance.
- Reference integrity.
- Cancellation.
- Restart behavior.
- ACK cleanup.
- No durable source copy.
- No durable-store access.

Use tiny deterministic fixtures committed where licensing permits. Do not use
the user's full library as the normal test suite.

### 14.4 Persistence tests

1. Valid result creates one complete active snapshot.
2. Duplicate result is idempotent.
3. Invalid refs roll back all new rows.
4. Invalid vector dimensions/non-finite values roll back.
5. Missing relationship evidence rolls back.
6. Artifact digest or size mismatch rolls back.
7. Previous active snapshot survives every failure mode.
8. Successful replacement switches active snapshot atomically.
9. OpenSearch outage leaves a retryable outbox item, not a failed snapshot.
10. Source metadata remains Zotero-owned and is never overwritten by processor
    output.

## 15. Acceptance Criteria

This work package is complete only when all of the following are true:

- One public Zotero sync path remains functional.
- Pending jobs are claimed atomically and fenced.
- Concurrent dispatchers cannot double-own a job.
- Expired work recovers after restart.
- Retry and cancellation semantics are deterministic.
- The Python service passes contract-v1 black-box tests.
- Python performs no durable-store writes.
- One real PDF and one EPUB complete end to end where fixtures are available.
- PDF page labels and physical page positions survive.
- EPUB locators are real and no page numbers are invented.
- A valid result becomes one immutable durable snapshot.
- Invalid/partial results leave the previous snapshot untouched.
- Durable artifacts are verified and source files are not copied.
- OpenSearch work is represented by a durable outbox entry.
- Processor ACK occurs only after commit and is recoverable.
- Build, vet, formatting, unit tests and isolated DB tests pass.
- A concise live-smoke report records job IDs, transitions, counts, processor
  capabilities and snapshot identity without logging document content.

## 16. Explicit Non-Goals

Do not add these during this work package:

- RAG search or chat API redesign.
- Research Missions agent.
- Remote processor transport.
- Source-file upload/object storage.
- LLM metadata correction or bibliographic validation.
- A second queue implementation.
- Python database/OpenSearch/graph credentials.
- Multiple concurrent Marker jobs before capability and resource tests justify
  them.
- Durable copies of Zotero PDF/EPUB files.
- Unrelated refactors of the old application.

## 17. Implementor Instructions

1. Read this document and `PROCESSOR_CONTRACT.md` completely before editing.
2. Inspect current `HEAD`, worktree changes and existing migrations first.
3. Treat Part 2 sync and metadata ownership as established architecture.
4. Use additive migrations; never edit already-applied migration files.
5. Keep commits aligned with review gates and small enough to audit.
6. Preserve unrelated user changes and untracked academic files.
7. Run DB tests only against an isolated test database.
8. Report exact commands, skipped tests and live evidence honestly.
9. Do not claim atomicity unless every relevant state write is in the stated
   transaction and external calls are outside it.
10. Do not claim retry safety without a fencing-token test.
11. Do not deploy or process the full library without explicit approval after
    the one-document smoke test.
12. Continue through the current gate without repeatedly asking whether to
    proceed; stop only for a real architectural blocker, required destructive
    migration, missing dependency, or review gate.

Each gate report must include:

- Commits.
- Files and schema changed.
- State-machine behavior added.
- Unit/integration/black-box test results.
- Tests skipped and why.
- Live actions performed.
- Known limitations.
- Explicit statement that no source PDF/EPUB was copied durably.

## 18. Reviewer Instructions

The reviewer must inspect code and tests directly. Implementor summaries are
evidence pointers, not proof.

For every gate:

1. Review findings first, ordered Critical/High/Medium/Low with file and line.
2. Check migrations on both a fresh DB and an upgraded DB.
3. Verify every claimed transaction boundary in code.
4. Look for external HTTP/file/model work inside DB transactions.
5. Verify fencing predicates on every post-claim mutation.
6. Verify zero-row updates are treated as lost lease, not success.
7. Inspect retry loops for duplicate processing, unbounded retries and hot
   polling.
8. Test concurrent claims with independent DB connections/processes.
9. Test stale workers after reclaim.
10. Confirm processor idempotency survives HTTP ambiguity and restart.
11. Confirm Python has no imports or runtime access to DB, OpenSearch, graph or
    Zotero persistence.
12. Validate source path restrictions and hash verification.
13. Treat processor result JSON as hostile input and review all validation.
14. Verify previous active snapshot survives every tested failure.
15. Verify ACK happens after commit and ACK failure does not rerun Marker.
16. Confirm OpenSearch is outbox-driven.
17. Confirm no source copy is retained.
18. Run build, vet, formatting and tests independently when possible.
19. Do not run integration tests against the user's application database when
    tests can mutate shared state.
20. Do not approve a gate based only on happy-path live counts.

The reviewer should reject these shortcuts:

- Claim followed by an unfenced update.
- In-memory-only leases or retries.
- Holding a DB transaction while waiting on Python.
- Marking a job completed before durable snapshot commit.
- Python writing directly to application stores.
- Returning old database IDs from Python instead of job-local refs.
- Replacing snapshots with delete-then-insert outside one transaction.
- Treating OpenSearch failure as a reason to rerun document extraction.
- Relying on process-local locks for cross-process correctness.
- Tests that inspect SQL text but never execute concurrent behavior.

At the final gate, the reviewer must provide one of:

- `APPROVED`: all acceptance criteria met, with residual risks listed.
- `APPROVED WITH FOLLOW-UP`: no correctness blocker; bounded follow-ups listed.
- `NOT APPROVED`: blocking findings with concrete remediation and tests.

## 19. Session Start Checklist

Every new implementation or review session starts with:

```bash
cd /Users/dudu/Code/axiom
git status --short
git branch --show-current
git log --oneline -10
git diff --check
```

Then read:

```text
axiom_ng/docs/LEASE_DISPATCHER_PROCESSOR_ADAPTER_WORK_ORDER.md
axiom_ng/docs/PROCESSOR_CONTRACT.md
docs/plans/AXIOM_NG_ZOTERO_DESKTOP.md
```

Before changing queue code, inspect:

```text
axiom_ng/internal/db/schema/
axiom_ng/internal/repo/ingest.go
axiom_ng/internal/repo/apply.go
axiom_ng/internal/sync/syncer.go
axiom_ng/internal/server/
```

Before changing Python processing, inspect the active runtime path and its tests.
Do not assume `core_rag/processor.py` is the active upload path; the old deployed
system used `services/background_document_processor.py` with the Marker/EPUB
workers and ML extraction modules listed above.

## 20. Handoff Note

Part 2 is treated as finished for purposes of this handoff: Zotero is the source
of truth, axiom-ng has one canonical sync path, metadata is mirrored losslessly,
and preferred file jobs are created idempotently.

The next session begins at Gate 0 and then implements Gate 1. It must not spend
time rebuilding Zotero synchronization or inventing a second document model.
The development direction is fixed:

```text
Zotero owns sources and metadata.
Python computes.
Go orchestrates, validates and persists.
Research agents consume axiom-ng APIs later.
```
