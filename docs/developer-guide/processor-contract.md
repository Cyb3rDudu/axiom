# Processor Contract

!!! note "Version"
    This page summarizes the **v1** contract. The canonical, implementation-
    binding detail rules live in `axiom_ng/docs/PROCESSOR_CONTRACT.md`; the
    version below is implied by that canonical file.

The Processor Contract separates durable data ownership from hardware- and
library-specific document processing:

- **axiom** owns Zotero synchronization, ingest jobs, persistent IDs,
  PostgreSQL/pgvector data, derived artifacts, the knowledge graph, and search
  index synchronization.
- **A document processor** performs compute-heavy extraction (first
  implementation in Python, because Marker and the ML libraries have the best
  hardware support there).
- **A processor owns no durable application state** and must be replaceable by
  another implementation of the same contract.

## Contract versions & endpoints

All endpoints live under `/v1`; every request carries `contract_version`.
Additive optional fields are allowed within v1.

```text
GET    /v1/health
GET    /v1/capabilities
POST   /v1/process
GET    /v1/jobs/{job_id}
GET    /v1/jobs/{job_id}/result
GET    /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST   /v1/jobs/{job_id}/cancel
POST   /v1/jobs/{job_id}/ack
```

Processing is asynchronous. `POST /v1/process` accepts or deduplicates a job
and returns quickly; long-running Marker/model operations do not hold the
request connection open.

## Ownership boundary (short)

**The processor MUST NOT:**

- read Zotero directly,
- write to PostgreSQL, pgvector, OpenSearch, or the knowledge graph,
- change ingest-job state in the axiom database,
- change bibliographic metadata supplied by Zotero,
- keep durable copies of the source PDF/EPUB,
- invent missing metadata with an LLM.

**The processor owns only computation:** reading the source file (for the
duration of the job), PDF/EPUB → Markdown, page/source-locator mapping,
structure-aware chunking, dense/sparse embeddings, entity/relationship
extraction, optional image/table extraction, and temporary files up to
acknowledgement.

## Core mechanics

- **Processing flow:** ingest job → `POST /v1/process` → processor → result +
  compute payload + artifacts → axiom validation → PostgreSQL/pgvector/graph
  transaction + durable artifact storage + OpenSearch outbox → `ack`. An `ack`
  lets the processor remove temporary files; `ack` is idempotent and the default
  must not prevent axiom restart recovery.
- **Idempotency:** the `idempotency_key` identifies equivalent processor work;
  the same accepted request returns the existing processor job instead of
  duplicate work. Replay after an acked job answers `409/ARTIFACTS_EXPIRED`
  (terminal, non-retryable); recompute needs a fresh idempotency key
  (`force_rebuild`).
- **Provenance:** chunk provenance (ref, index, text, locator, section
  hierarchy, paragraph indexes, token_count, embeddings) is required, not
  optional — and survives processor replacement and re-indexing. For PDFs,
  physical zero-based page indexes + logical page labels as strings; for EPUB,
  a CFI locator, never invented page numbers.
- **Validation before persistence (§14):** source identity + hash, unique
  contiguous chunk indexes, unique local refs, all references, dense-vector
  dimensions/values, sparse key/value types, required locators, evidence
  references on extracted relationships, result counts against the actual
  arrays.
- **Errors:** terminal failures use stable machine-readable codes (e.g.
  `SOURCE_NOT_FOUND`, `SOURCE_HASH_MISMATCH`, `MODEL_UNAVAILABLE`,
  `OUT_OF_MEMORY`, `CHUNKING_FAILED`, `CANCELLED`, `INTERNAL_ERROR`), each with
  a default retryability.
- **Security (§18):** loopback bind `127.0.0.1` by default; allowed source
  roots; path-traversal/regular-file rejection; never pass Zotero/DB/OS
  credentials to the processor; no full document text/embeddings/secrets in logs
  by default.

## Contract tests (§19)

Every processor implementation must pass the same black-box suite:
health/capabilities, idempotency, known PDF → Markdown + chunk, provenance
round-trip, hash-mismatch failure, reference integrity, embeddings vs
capabilities, no PostgreSQL/OpenSearch writes, cancellation, ack
cleanup+idempotency, restart without fake success, no durable source copy,
source_url delivery, and replay-after-ACK semantics.

## Full reference

You can read the binding, complete contract text in the canonical file:

- Repo path: `axiom_ng/docs/PROCESSOR_CONTRACT.md`
- GitHub: [PROCESSOR_CONTRACT.md](https://github.com/Cyb3rDudu/axiom/blob/main/axiom_ng/docs/PROCESSOR_CONTRACT.md)

Continue: [axiom runner](axiom-runner.md) ·
[axiom dispatcher](axiom-go.md) · [Architecture Overview](architecture.md)
