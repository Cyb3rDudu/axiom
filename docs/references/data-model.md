# Data Model

This is the schema-level reference for the durable store: what axiom persists,
how the tables relate, and the invariants that make the model trustworthy. The
source of truth for field-level detail is the migrations under
`axiom_ng/internal/db/schema/`; this page is the developer-facing map.

## The durable store at a glance

axiom's durable state lives in PostgreSQL (+pgvector for embeddings) and
OpenSearch (search index). The Zotero mirror tables are axiom's read-side copy
of Zotero; selection tables control job admission without reducing that mirror.
The `ingest_jobs` + `processing_*` + `opensearch_outbox` tables form the
processing pipeline. The KG read-model tables materialize entity families and
relation triples from active raw graph rows. Repair tables record the separate,
explicitly configured Zotero mutation path and its audit trail.

## Core tables

| Table | Role |
| --- | --- |
| `zotero_items` / `zotero_collections` / `zotero_item_collections` | Lossless mirror of the Zotero library (items, collections, memberships). |
| `zotero_documents` / `zotero_attachments` / `zotero_sources` | Normalized document projections + preference; one preferred attachment per document. |
| `zotero_selections` | Persisted document-level `included`/`excluded` job-admission choices; absence means default behavior. |
| `zotero_collection_selections` | Persisted collection-level choices keyed by stable Zotero collection key; intentionally has no collection foreign key. |
| `ingest_jobs` | A pending/claimed/processing/completed/failed/cancelled/skipped row per processable attachment; the claim/lease/fencing fields live here. |
| `processing_snapshots` | Versioned processing identity, provenance, activation state, and generation counter. |
| `processing_chunks` | The text chunks of a snapshot, with source locators + section hierarchy. |
| `processing_chunk_dense_embeddings` / `processing_chunk_sparse_embeddings` | The chunk embeddings (pgvector dense; sparse as key/value). |
| `processing_entities` / `processing_entity_mentions` | Extracted entities and their chunk/page-backed mentions. `processing_entities.alias_of` links a variant to its KG family survivor. |
| `processing_entity_relationships` / `processing_chunk_relationships` | Relationship graph edges + chunk-to-chunk edges, with evidence refs. |
| `kg_superseded_entities` | Archived semantic evidence for entity rows deleted by exact-form consolidation. |
| `kg_entity_roots` | Materialized KG family roots: survivor form, family forms, type votes, mention/member counts, normalized search keys. |
| `kg_relation_triples` | Materialized root-to-root relation triples, with evidence union, support counts, and confidence inputs. |
| `kg_relation_evidence_docs` | Per-triple document support counts used for relation filtering and source hydration. |
| `processing_artifacts` | Durable derived artifacts (e.g. normalized Markdown) with verified digests. |
| `opensearch_outbox` | The transactionally-created queue of index operations the drainer replays. |
| `repair_cases` | Repair state machine, analysis/plan, verification result, attempt accounting, and terminal verdict. |
| `zotero_write_audit` | Audit trail for quarantine, attachment deletion, and healed-attachment creation. |

## The snapshot / chunk / outbox triad

The processing pipeline's durable shape is three coordinated concerns:

1. **Snapshots** are versioned identities. A result with a new identity inserts
   a row and marks the previous active row inactive. A forced result with the
   same identity replaces that row's children and increments its `generation`.
   The DB-level unique constraint (`0011`) enforces **one active snapshot per
   attachment** across profiles.
2. **Chunks** hang off a snapshot and carry the retrieval payload: text,
   locators (physical page + logical label, or EPUB CFI), section hierarchy,
   paragraph indexes, and embeddings.
3. **Outbox** makes index consistency transactional. The same commit that
   activates a snapshot (and deactivates the previous one) writes the
   index/delete operations to `opensearch_outbox`; a separate drainer replays
   them into OpenSearch. An OpenSearch outage never re-runs Marker — the rows
   just wait.

## Key invariants

| Invariant | Where it is enforced |
| --- | --- |
| One mutable, fenced ingest job per processable attachment | `ingest_jobs` + lease/fencing predicates (repo). |
| Only the current lease owner can mutate an active job | Fencing predicate (`job_id + worker_id + lease_token`); zero-rows ⇒ lost-lease. |
| `completed` is set only after the result + artifacts are durably committed | The completion update runs in the same transaction as the persist. |
| One active snapshot per attachment | DB unique constraint (migration `0011`), not just app logic. |
| A normal identity change preserves the superseded row | The persist transaction inserts the new identity and only deactivates the old; same-identity `force_rebuild` is the explicit replace-in-place exception. |
| Index converges to active snapshot chunks after the outbox drains | Transactional index/delete outbox operations plus obsolete-operation guards. |
| Processor result refs resolve within the snapshot | Validation before persist (contract §14). |
| Normal ingest makes no durable source copy | Source files are read in place; processing artifacts are derived. The separately configured repair path quarantines an original before any Zotero mutation. |
| Selection changes do not erase the mirror or existing chunks | `zotero_selections` and `zotero_collection_selections` gate new job creation only. |
| Repair mutation has a custody record | The original is quarantined and the quarantine audit must commit before the old Zotero attachment is deleted. |
| KG identity maintenance is serialized | Mutating KG maintenance paths run inside one transaction under a transaction-scoped advisory lock. |
| Entity consolidation preserves deleted-row evidence | Loser entity evidence is archived in `kg_superseded_entities` before loser rows are deleted. |
| KG API reads rebuildable projections | `kg_entity_roots`, `kg_relation_triples`, and `kg_relation_evidence_docs` are refreshed from active raw KG rows. |

## Migration map

Migrations are embedded and applied in lexical order at startup. Existing files
are immutable; schema changes use a new numbered migration.

| Migration | Durable change |
| --- | --- |
| `0001_ingest_jobs.sql` | Ingest status enum, initial job queue, status index. |
| `0002_zotero.sql` | Sources, document/attachment projections, job foreign keys and idempotency. |
| `0003_zotero_canonical.sql` | Lossless canonical items, collections, memberships, and canonical projection links. |
| `0004_zotero_canonical_integrity.sql` | Canonical foreign-key integrity. |
| `0005_ingest_jobs_resolved.sql` | Failure resolution timestamp and unresolved-failure index. |
| `0006_ingest_jobs_lease.sql` | Lease token, retry schedule, heartbeat, cancellation, frozen input/profile, and idempotency key. |
| `0007_ingest_jobs_ack_pending.sql` | Durable processor-ack retry marker. |
| `0008_processing_snapshots.sql` | pgvector extension, snapshot identities and provenance, activation scope, and verified artifacts. |
| `0009_processing_chunks_outbox.sql` | Chunks, dense/sparse embeddings, entities, mentions, relations, and OpenSearch outbox. |
| `0010_ingest_jobs_runner_name.sql` | Human-readable runner identity stamped at claim time. |
| `0011_snapshots_one_active_per_attachment.sql` | DB-level unique partial index enforcing one active snapshot per attachment. |
| `0012_zotero_selections.sql` | Document-level ingest selection. |
| `0013_zotero_collection_selections.sql` | Collection-level ingest selection keyed independently of mirror row lifecycle. |
| `0014_repair_cases.sql` | Repair status/cases, per-attachment attempt counter, and Zotero write audit. |
| `0015_entity_aliases.sql` | Nullable `processing_entities.alias_of` link and partial index for KG family variants. |
| `0016_superseded_entities.sql` | Superseded entity archive with survivor/loser uniqueness and survivor, loser, and form indexes. |
| `0017_kg_read_model.sql` | Materialized KG roots, relation triples, evidence-document support, rank/filter indexes, unique `(source_root_id, target_root_id, type)`, and no self-loop check. |

## Reading the model

- **From a document to its data:** `zotero_documents` → effective selection →
  `ingest_jobs` → `processing_snapshots` → `processing_chunks` (+ embeddings,
  entities, relationships, artifacts).
- **From an ingest job to its state:** `ingest_jobs.status` + the leased fields
  (`claimed_by`, `lease_until`, `lease_token`) tell you who owns it and whether
  it is stuck (see [Monitoring](../operations/monitoring.md) and
  [Troubleshooting](../operations/troubleshooting.md)).
- **From the index back to the source:** an outbox/OpenSearch entry points at
  snapshot/chunk identifiers that resolve back to the durable store.
- **From a KG family to its members:** `kg_entity_roots.root_entity_id` is the
  survivor/root; `processing_entities.alias_of` points variants at that root,
  and `kg_entity_roots.forms` lists the visible family forms.
- **From a materialized relation to its evidence:** `kg_relation_triples` stores
  the root-level triple and evidence chunk IDs; `kg_relation_evidence_docs`
  stores per-document support.
- **From a consolidated entity to its deleted evidence:**
  `kg_superseded_entities.survivor_entity_id` lists loser form, type,
  description, source snapshot, document, and mention count.
- **From a repair to its custody chain:** `repair_cases.attachment_id` resolves
  the affected attachment; `zotero_write_audit` records quarantine and each
  Zotero mutation associated with the case.

Next: [Benchmarks & Analyses](benchmarks.md) · [FAQ](faq.md) ·
[Developer Guide → Architecture Overview](../developer-guide/architecture.md)
