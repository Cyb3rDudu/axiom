# Data Model

This is the schema-level reference for the durable store: what axiom persists,
how the tables relate, and the invariants that make the model trustworthy. The
source of truth for field-level detail is the migrations under
`axiom_ng/internal/db/schema/`; this page is the developer-facing map.

## The durable store at a glance

axiom's durable state lives in PostgreSQL (+pgvector for embeddings) and
OpenSearch (search index). The Zotero mirror tables are our read-side copy of
Zotero; the `ingest_jobs` + `processing_*` + `opensearch_outbox` tables are the
processing pipeline.

## Core tables

| Table | Role |
| --- | --- |
| `zotero_items` / `zotero_collections` / `zotero_item_collections` | Lossless mirror of the Zotero library (items, collections, memberships). |
| `zotero_documents` / `zotero_attachments` / `zotero_sources` | Normalized document projections + preference; one preferred attachment per document. |
| `ingest_jobs` | A pending/claimed/processing/completed/failed/cancelled/skipped row per processable attachment; the claim/lease/fencing fields live here. |
| `processing_snapshots` | One immutable, versioned processing result per document processing. |
| `processing_chunks` | The text chunks of a snapshot, with source locators + section hierarchy. |
| `processing_chunk_dense_embeddings` / `processing_chunk_sparse_embeddings` | The chunk embeddings (pgvector dense; sparse as key/value). |
| `processing_entities` / `processing_entity_mentions` | Extracted entities and their chunk/page-backed mentions. |
| `processing_entity_relationships` / `processing_chunk_relationships` | Relationship graph edges + chunk-to-chunk edges, with evidence refs. |
| `processing_artifacts` | Durable derived artifacts (e.g. normalized Markdown) with verified digests. |
| `opensearch_outbox` | The transactionally-created queue of index operations the drainer replays. |

## The snapshot / chunk / outbox triad

The processing pipeline's durable shape is three coordinated concerns:

1. **Snapshots** are generations. `processing_snapshots` holds one immutable
   row per processing result; on a replace the previous snapshot is marked
   **inactive** (never overwritten), and the DB-level unique constraint
   (`0011`) enforces **one active snapshot per attachment**.
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
| Old snapshots stay immutable (only ever deactivated) | Snapshot replacement inserts a new generation; never deletes/overwrites the old. |
| Index doc-count == active snapshots' chunk-count | Transactional outbox delete-ops on superseded generations. |
| Processor result refs resolve within the snapshot | Validation before persist (contract §14). |
| No durable source copies | Source files are read in place; artifacts are derived, not originals. |

## Reading the model

- **From a document to its data:** `zotero_documents` → `ingest_jobs` →
  `processing_snapshots` → `processing_chunks` (+ embeddings, entities,
  relationships, artifacts).
- **From an ingest job to its state:** `ingest_jobs.status` + the leased fields
  (`claimed_by`, `lease_until`, `lease_token`) tell you who owns it and whether
  it is stuck (see [Monitoring](../operations/monitoring.md) and
  [Troubleshooting](../operations/troubleshooting.md)).
- **From the index back to the source:** an outbox/OpenSearch entry points at
  snapshot/chunk identifiers that resolve back to the durable store.

Next: [Benchmarks & Analyses](benchmarks.md) · [FAQ](faq.md) ·
[Developer Guide → data-model summary](../developer-guide/architecture.md)
