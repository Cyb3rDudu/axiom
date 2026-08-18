# axiom-ng Document Processor Contract

Status: Draft v0.1  
Owner: axiom-ng  
Transport v1: HTTP on loopback  

## 1. Purpose

This contract separates durable data ownership from hardware- and
library-specific document processing.

- `axiom-ng` owns Zotero synchronization, ingest jobs, persistent IDs,
  PostgreSQL/pgvector data, derived artifacts, the knowledge graph and search
  index synchronization.
- A document processor performs compute-heavy extraction. The first processor
  is implemented in Python because Marker and the required ML libraries have
  the best hardware support there.
- A processor does not own durable application state and must be replaceable by
  another implementation of this contract.

The contract preserves the page and section provenance already implemented in
the Python pipeline. It does not move Marker, chunking or ML inference into Go.

## 2. Ownership Boundary

### Zotero owns

- Original PDF and EPUB files.
- Bibliographic metadata and Zotero item identity.
- Collections, tags, notes, annotations and attachment relationships.

### axiom-ng owns

- The lossless Zotero mirror and normalized metadata projections.
- Ingest jobs, leases, retries and cancellation.
- Persistent IDs and versioned processing snapshots.
- Durable Markdown and other explicitly retained derived artifacts.
- Chunks, locators, embeddings, entities and relationships.
- PostgreSQL/pgvector writes and knowledge-graph writes.
- OpenSearch synchronization and consistency recovery.

### The processor owns only computation

- Reading the supplied source file for the duration of a job.
- PDF/EPUB conversion to Markdown.
- Page-label extraction and source-location mapping.
- Structure-aware chunking.
- Dense and sparse embedding calculation.
- Entity and relationship extraction.
- Optional image and table extraction.
- Temporary files until axiom-ng acknowledges the result.

The processor MUST NOT:

- Read from Zotero directly.
- Write to PostgreSQL, pgvector, OpenSearch or the knowledge graph.
- Update ingest-job state in axiom-ng's database.
- Modify bibliographic metadata supplied by Zotero.
- Keep durable copies of source PDF or EPUB files.
- Invent missing metadata with an LLM.

## 3. Processing Flow

```text
Zotero attachment
       |
       | local_path + immutable metadata snapshot
       v
axiom-ng ingest job
       |
       | POST /v1/process
       v
Python processor
       |
       | result manifest + computed payload + artifacts
       v
axiom-ng validation
       |
       +--> PostgreSQL / pgvector / graph transaction
       +--> durable derived-artifact storage
       +--> OpenSearch outbox/indexing
       |
       | POST /v1/jobs/{job_id}/ack
       v
Processor removes temporary files
```

Source files are read in place in local v1. As an ADDITIVE v1 extension,
`attachment.source_url` may carry an HMAC-signed download URL (dispatcher
signs `job_id|lease_unix` with the shared `AXIOM_PROCESSOR_SOURCE_SECRET`;
the axiom-ng endpoint `/api/processor/source/{job_id}` verifies signature,
expiry, job status and lease before streaming the bytes). The pulled bytes
are temporary and are deleted after the job is acknowledged or expires;
the content hash gate applies to downloaded bytes exactly as to local files.

## 4. Versioning

All endpoints are under `/v1`.

Every request contains `contract_version`. Additive optional fields are allowed
within v1. Removing a field, changing its meaning or changing a required type
requires a new major contract version.

Unknown fields MUST be ignored by readers and preserved when a component acts
as a proxy.

## 5. Endpoints

```http
GET    /v1/health
GET    /v1/capabilities
POST   /v1/process
GET    /v1/jobs/{job_id}
GET    /v1/jobs/{job_id}/result
GET    /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST   /v1/jobs/{job_id}/cancel
POST   /v1/jobs/{job_id}/ack
POST   /v1/embed        (additive v1, §7a, #131)
POST   /v1/rerank       (additive v1, §7a, #132)
```

Processing is asynchronous. `POST /v1/process` accepts or deduplicates a job
and returns quickly. Long-running Marker and model operations do not keep the
request connection open.

## 6. Capabilities

`GET /v1/capabilities` returns the processor implementation and the exact
features available on the current host.

```json
{
  "contract_versions": ["1.0"],
  "processor": {
    "name": "axiom-python-marker",
    "version": "0.1.0"
  },
  "formats": [
    "application/pdf",
    "application/epub+zip"
  ],
  "features": {
    "markdown": true,
    "page_locators": true,
    "section_hierarchy": true,
    "images": true,
    "dense_embeddings": true,
    "sparse_embeddings": true,
    "entities": true,
    "entity_relationships": true,
    "query_embedding": true,
    "reranking": true
  },
  "models": {
    "dense_embedding": {
      "name": "BAAI/bge-m3",
      "dimensions": 1024
    },
    "entity_extraction": {
      "name": "gliner"
    },
    "relationship_extraction": {
      "name": "mrebel"
    },
    "query_embedding": {
      "name": "BAAI/bge-m3",
      "dimensions": 1024
    },
    "reranking": {
      "name": "BAAI/bge-reranker-v2-m3"
    }
  },
  "limits": {
    "max_concurrent_jobs": 1,
    "max_source_bytes": 2147483648,
    "max_query_texts": 16,
    "rerank_max_texts": 64
  }
}
```

Capability model names are examples. axiom-ng records the values returned by
the running processor and must not assume them from this document.

## 7. Process Request

`POST /v1/process`

```json
{
  "contract_version": "1.0",
  "job_id": "8eac26ea-48e4-42fd-a6a9-251f0594426f",
  "idempotency_key": "attachment-id:content-hash:profile-hash",
  "source": {
    "type": "zotero",
    "source_id": "2a8c66bc-437e-44da-8584-4379985633bc",
    "server_id": "ZQpTgJQ5H9O0"
  },
  "document": {
    "document_id": "44cc1f15-41c7-4751-9e70-fae14005072b",
    "zotero_key": "5J6XFMNP",
    "zotero_version": 181,
    "metadata_snapshot": {
      "itemType": "book",
      "title": "Example Book",
      "creators": [
        {
          "creatorType": "author",
          "firstName": "Ada",
          "lastName": "Lovelace"
        }
      ],
      "date": "2024",
      "language": "de"
    }
  },
  "attachment": {
    "attachment_id": "8e1ce84f-29d8-48fc-8c87-6eb21e747afa",
    "zotero_key": "NU8SS6HG",
    "zotero_version": 181,
    "content_type": "application/pdf",
    "filename": "example-book.pdf",
    "local_path": "/Users/dudu/Zotero/storage/NU8SS6HG/example-book.pdf",
    "source_url": "http://100.79.104.120:8011/api/processor/source/<job_id>?exp=1786715133&sig=<hmac-sha256-hex>",
    "content_hash": "sha256:3ab8c7d6",
    "size_bytes": 12345678,
    "mtime_ms": 1786336894000
  },
  "processing": {
    "profile": "full-rag-v1",
    "force_rebuild": false,
    "language_hint": "de",
    "extract_images": true,
    "compute_dense_embeddings": true,
    "compute_sparse_embeddings": true,
    "extract_entities": true,
    "extract_relationships": true
  }
}
```

### Request rules

- `job_id` is generated and owned by axiom-ng.
- `idempotency_key` identifies equivalent processor work. Repeating the same
  accepted request MUST return the existing processor job instead of starting
  duplicate work.
- `metadata_snapshot` is immutable input context. It may be copied into chunk
  context but MUST NOT be enriched or corrected by the processor.
- `local_path` is a native absolute path in local v1. It is never a durable
  processor-owned path.
- The processor MUST verify that the path is a regular readable file.
- The processor MUST calculate the source hash and compare it with
  `content_hash` before producing a successful result.
- Processing feature flags request computation only. They do not authorize the
  processor to write to a durable store.

## 7a. Query Endpoints (additive v1 extension, #131/#132)

Synchronous query-side compute. Unlike `POST /v1/process` these answer in the
request (no job lifecycle): the models are process-wide singletons — lazy-
loaded on first use, kept warm for the process lifetime (low latency is the
point). The processor still owns no durable state; retrieval against the OS
index is axiom-ng's job.

### POST /v1/embed (#131)

Request:

```json
{
  "contract_version": "1.0",
  "texts": ["Suchanfrage ..."],
  "max_texts": 3
}
```

- `texts`: 1..N non-blank query texts (N = `limits.max_query_texts`, 16).
- `max_texts`: optional per-request cap; may only lower the server cap.
- `include_sparse` (additive, R5 #135, default false): additionally returns
  the learned lexical weights per text — the query side of the OpenSearch
  `rank_features` arm. Same encode pass; weights are JSON numbers.

Response:

```json
{
  "contract_version": "1.0",
  "model": "BAAI/bge-m3",
  "dimensions": 1024,
  "embeddings": [[0.012, -0.034, ...]],
  "sparse": [{"130629": 0.28, ...}]
}
```

`sparse` is present only when `include_sparse` was set (one map per input
text, aligned with `embeddings`). `model`/`dimensions` always agree with
`models.query_embedding` in the
capability report. BGE-M3 is a symmetric encoder: queries and passages use
the same model and pooling, so these vectors are cosine-comparable with the
chunk embeddings from `POST /v1/process` (verified by the OS roundtrip
test). 4xx error codes (detail `{code, message}`): `QUERY_TEXTS_EMPTY`,
`QUERY_TEXT_BLANK`, `QUERY_TEXTS_TOO_MANY`, `MAX_TEXTS_INVALID`,
`CONTRACT_VERSION_UNSUPPORTED`. A model/capability shape disagreement is a
500 `EMBEDDING_SHAPE_MISMATCH` (never silent zeros).

### POST /v1/rerank (#132)

Request:

```json
{
  "contract_version": "1.0",
  "query": "Suchanfrage",
  "texts": ["Kandidat 1", "Kandidat 2"],
  "top_n": 10
}
```

- `texts`: 1..64 non-blank candidates (`limits.rerank_max_texts`).
- `top_n`: >= 1; values above `len(texts)` return all texts (archive slicing
  semantics), values below 1 are rejected.

Response:

```json
{
  "contract_version": "1.0",
  "model": "BAAI/bge-reranker-v2-m3",
  "scores": [
    {"index": 0, "score": 0.987},
    {"index": 2, "score": 0.512}
  ]
}
```

`scores` is sorted descending, sigmoid-normalized (0..1); `index` refers to
the request's `texts` position; ties keep input order. 4xx error codes:
`RERANK_QUERY_EMPTY`, `RERANK_TEXTS_EMPTY`, `RERANK_TEXT_BLANK`,
`RERANK_TEXTS_TOO_MANY`, `RERANK_TOP_N_INVALID`,
`CONTRACT_VERSION_UNSUPPORTED`; shape disagreements are 500
`RERANK_SHAPE_MISMATCH`.

## 8. Process Acceptance

An accepted request returns HTTP `202 Accepted`.

```json
{
  "contract_version": "1.0",
  "job_id": "8eac26ea-48e4-42fd-a6a9-251f0594426f",
  "status": "accepted",
  "deduplicated": false
}
```

Valid states are:

```text
accepted -> running -> completed
                    -> failed
                    -> cancelled
```

## 9. Job Status

`GET /v1/jobs/{job_id}`

```json
{
  "contract_version": "1.0",
  "job_id": "8eac26ea-48e4-42fd-a6a9-251f0594426f",
  "status": "running",
  "stage": "chunk",
  "progress": {
    "completed_units": 230,
    "total_units": 684,
    "unit": "chunks"
  },
  "started_at": "2026-08-11T01:30:00Z",
  "updated_at": "2026-08-11T01:42:12Z"
}
```

Progress is advisory. axiom-ng uses the terminal state and result validation as
the source of truth.

The `stage` vocabulary (in progression order, single source:
`axiom_ng_runner.PIPELINE_STAGES`): `validate_source` → `convert` → `chunk` →
`embed` → `entities` → `relationships` → `assemble`.

## 10. Processor Result

`GET /v1/jobs/{job_id}/result` is available only for a completed processor job.
It returns `application/vnd.axiom.processor-result+json`.

The result uses job-local references. axiom-ng validates all references and
maps them to durable IDs while persisting the processing snapshot.

```json
{
  "contract_version": "1.0",
  "job_id": "8eac26ea-48e4-42fd-a6a9-251f0594426f",
  "status": "completed",
  "source": {
    "attachment_id": "8e1ce84f-29d8-48fc-8c87-6eb21e747afa",
    "content_hash": "sha256:3ab8c7d6",
    "verified": true
  },
  "processor": {
    "name": "axiom-python-marker",
    "version": "0.1.0",
    "profile": "full-rag-v1",
    "profile_hash": "sha256:84f47a5b",
    "models": {
      "marker": "marker-1.0",
      "dense_embedding": "BAAI/bge-m3",
      "entity_extraction": "gliner",
      "relationship_extraction": "mrebel"
    }
  },
  "artifacts": [
    {
      "ref": "markdown",
      "kind": "markdown",
      "media_type": "text/markdown; charset=utf-8",
      "sha256": "d23412f5",
      "size_bytes": 456789,
      "retention": "durable"
    },
    {
      "ref": "image-0001",
      "kind": "extracted_image",
      "media_type": "image/png",
      "sha256": "7741bb7a",
      "size_bytes": 98765,
      "retention": "durable_if_referenced"
    }
  ],
  "manifest": {
    "source_page_count": 312,
    "page_label_map": {
      "0": "i",
      "1": "ii",
      "12": "1",
      "34": "23"
    }
  },
  "chunks": [
    {
      "ref": "chunk-0001",
      "index": 0,
      "text": "The first extracted passage.",
      "locator": {
        "type": "page_span",
        "physical_page_start": 34,
        "physical_page_end": 35,
        "page_label_start": "23",
        "page_label_end": "24",
        "page_source": "pdf_label_sane",
        "source": "marker_paginate"
      },
      "structure": {
        "section_titles": [
          "Environmental analysis",
          "Stakeholders"
        ],
        "start_paragraph_index": 118,
        "end_paragraph_index": 123
      },
      "token_count": 481,
      "image_refs": ["image-0001"],
      "embeddings": {
        "dense": {
          "model": "BAAI/bge-m3",
          "dimensions": 3,
          "values": [0.12, -0.34, 0.56]
        },
        "sparse": {
          "model": "BAAI/bge-m3",
          "values": {
            "101": 0.82,
            "843": 0.31
          }
        }
      },
      "metadata": {}
    }
  ],
  "entities": [
    {
      "ref": "entity-0001",
      "text": "St. Gallen Management Model",
      "canonical_form": "st. gallen management model",
      "type": "METHOD",
      "description": null,
      "mentions": [
        {
          "chunk_ref": "chunk-0001",
          "start_char": 0,
          "end_char": 28,
          "confidence": 0.93
        }
      ]
    }
  ],
  "chunk_relationships": [],
  "entity_relationships": [],
  "stats": {
    "pages": 312,
    "chunks": 1,
    "artifacts": 2,
    "entities": 1,
    "entity_relationships": 0,
    "chunk_relationships": 0
  },
  "warnings": []
}
```

The dense vector above is shortened for readability. In a real result,
`dimensions` MUST equal the number of values and the processor capability.

## 11. Chunk Provenance

Chunk provenance is required, not optional metadata. It must survive processor
replacement and re-indexing.

Required chunk fields:

- `ref`: unique within the result.
- `index`: zero-based order within this processing snapshot.
- `text`: exact text embedded and indexed.
- `locator`: source position information.
- `structure.section_titles`: ordered heading hierarchy. The deepest entry is
  the heading under which the chunk's first content sits — the section-trail
  state at chunk start, never the state after the closing boundary (#186;
  enforced by `axiom_ng_runner/tests/test_chunker_section_trail.py`). For a
  chunk that opens with recycled overlap text from the previous chunk, the
  first NON-overlap content decides.
- `structure.start_paragraph_index` and `end_paragraph_index`.
- `token_count`.
- Embeddings requested by the processing profile.

For PDFs, a page locator should contain both:

- Zero-based physical PDF page indexes for opening the correct page.
- Logical page labels as strings for citations, including values such as `iv`,
  `23` or `A-3`.

The current Python implementation derives logical labels from the PDF page
label map and Marker pagination markers. Its existing fields map as follows:

| Existing Python field | Contract field |
| --- | --- |
| `chunk_id` | job-local `ref`; Go assigns the durable ID |
| `chunk_index` | `index` |
| `page_start` | `locator.page_label_start` |
| `page_end` | `locator.page_label_end` |
| `section_titles` | `structure.section_titles` |
| `start_paragraph_index` | `structure.start_paragraph_index` |
| `end_paragraph_index` | `structure.end_paragraph_index` |
| `token_count` | `token_count` |
| `image_refs` | `image_refs` |
| `page_label_map` | `manifest.page_label_map` |

For EPUB, the processor may use `locator.type = "epub_cfi"` and provide CFI
start/end fields. Page labels MUST NOT be fabricated for sources without stable
pages. An `epub_cfi` locator MUST carry `page_source: "none"`.

### Page source trust level (#173)

Every page locator carries its trust level in `page_source` — the contract is
"never guess": a page reference is always attributed, and only
`folio_verified` may be cited as a printed page. Values:

| Value | Meaning |
| --- | --- |
| `folio_verified` | printed folio read from the text layer AND verified as a consistent ascending sequence — the only citable print-page form |
| `pdf_label_sane` | PDF label, sanity-checked (unique, monotone, plausible) — presentable only with a marker |
| `physical_only` | bare PDF page index — never renderable as a printed page |
| `none` | EPUB CFI / pageless source — chapter and CFI, no page number |

Rules:

- `page_span` locators MUST carry `page_source` with one of
  `folio_verified | pdf_label_sane | physical_only` (stamped by the runner's
  page-trust pipeline from the start page's trust level; `page_label_end` is
  dropped when the end page carries a different level — numbering spaces are
  never mixed).
- `epub_cfi` locators carry `page_source: "none"`.
- axiom-ng rejects (terminal, §14): a blank `page_source` with
  `LOCATOR_PAGE_SOURCE_MISSING`, an unknown value with
  `LOCATOR_PAGE_SOURCE_UNKNOWN`, and `epub_cfi` carrying a page level with
  `LOCATOR_PAGE_SOURCE_INCONSISTENT`.

## 12. Relationships

Chunk and entity relationships refer only to job-local refs in the processor
result. axiom-ng resolves these references after assigning durable IDs.

```json
{
  "chunk_relationships": [
    {
      "source_chunk_ref": "chunk-0001",
      "target_chunk_ref": "chunk-0002",
      "type": "sequential_next",
      "strength": 0.85,
      "metadata": {}
    }
  ],
  "entity_relationships": [
    {
      "source_entity_ref": "entity-0001",
      "target_entity_ref": "entity-0002",
      "type": "part_of",
      "strength": 0.8,
      "evidence_chunk_refs": ["chunk-0001"],
      "extractor": "mrebel",
      "metadata": {}
    }
  ]
}
```

Every non-sequential relationship MUST contain evidence chunk references.

## 13. Artifacts

Original PDF and EPUB files are never processor output and are never copied
into durable axiom-ng storage.

Allowed durable derived artifacts are:

- Normalized Markdown.
- The extraction manifest.
- Images or tables referenced by retained Markdown or required as evidence.
- Optional processor diagnostics explicitly enabled by policy.

Artifact bytes are fetched with:

```http
GET /v1/jobs/{job_id}/artifacts/{artifact_ref}
```

The processor MUST return the declared media type, byte length and digest.
axiom-ng verifies these values before retaining an artifact.

Unreferenced temporary files are never durable artifacts.

## 14. Persistence Semantics

axiom-ng persists a successful result as one immutable processing snapshot
identified by at least:

```text
attachment_id
content_hash
processor name and version
processing profile hash
```

Before committing a snapshot, axiom-ng MUST validate:

- The echoed source identity and content hash.
- Unique, contiguous chunk indexes.
- Unique job-local refs.
- All artifact, chunk and entity references.
- Dense-vector dimensions and finite numeric values.
- Sparse-vector key and value types.
- Required locators for page-based formats.
- Evidence references on extracted relationships.
- Result counts against the actual arrays.

The processor result is not durable application state until axiom-ng commits
it. Partial processor output MUST NOT replace the previous valid snapshot.

PostgreSQL/pgvector and graph writes should commit transactionally. OpenSearch
is synchronized from committed data through an outbox or equivalent retryable
operation; an OpenSearch outage must not require rerunning Marker.

## 15. Completion Acknowledgement

After all required result data and artifacts are durably committed, axiom-ng
calls:

```http
POST /v1/jobs/{job_id}/ack
```

```json
{
  "persisted": true,
  "snapshot_id": "a646463a-56db-4af2-8b7f-407b53cd835e"
}
```

Only this acknowledgement authorizes the processor to remove result artifacts
and temporary source copies. The processor may also expire unacknowledged jobs
after a configurable retention period, but the default must allow axiom-ng to
recover from a restart.

Acknowledgement is idempotent.

### Replay after acknowledgement (additive v1 extension, #126)

A resubmit that dedups onto an **acknowledged** job hits the seam between
§19.2-style dedup (return the existing result) and §15/§19.10 (artifacts died
with the ACK). The processor answers with a terminal, parseable refusal
instead of a result whose artifacts can no longer be fetched:

```http
POST /v1/process  (same idempotency_key as an acked job)
→ 409 Conflict
{
  "detail": {
    "code": "ARTIFACTS_EXPIRED",
    "message": "job was acknowledged; result artifacts are gone (contract §15/§19.10). Re-enqueue with a fresh idempotency key (force_rebuild) to recompute.",
    "retryable": false
  }
}
```

The dispatcher maps this to a terminal `ARTIFACTS_EXPIRED` job error (no
retry — re-submitting hits the same wall); recompute requires a new
idempotency key (force_rebuild). Un-acked dedup keeps returning
202 + `deduplicated: true`.

## 16. Errors

Terminal failures use a stable machine-readable code.

```json
{
  "contract_version": "1.0",
  "job_id": "8eac26ea-48e4-42fd-a6a9-251f0594426f",
  "status": "failed",
  "error": {
    "code": "PDF_CONVERSION_FAILED",
    "message": "Marker exited with status 1",
    "retryable": false,
    "stage": "convert",
    "details": {}
  }
}
```

Initial error codes:

| Code | Default retryable | Meaning |
| --- | --- | --- |
| `SOURCE_NOT_FOUND` | false | Supplied local source path does not exist |
| `SOURCE_NOT_READABLE` | true | Source exists but cannot currently be read |
| `SOURCE_HASH_MISMATCH` | false | Source bytes differ from the requested hash |
| `UNSUPPORTED_FORMAT` | false | Processor cannot process the media type |
| `PDF_CONVERSION_FAILED` | false | Marker/conversion failed deterministically |
| `MODEL_UNAVAILABLE` | true | Required model or accelerator is unavailable |
| `OUT_OF_MEMORY` | true | Processing exceeded available memory/VRAM |
| `CHUNKING_FAILED` | false | Markdown could not be converted into chunks |
| `EMBEDDING_FAILED` | true | Embedding computation failed |
| `ENTITY_EXTRACTION_FAILED` | true | Requested entity extraction failed |
| `RELATION_EXTRACTION_FAILED` | true | Requested relation extraction failed |
| `CANCELLED` | false | axiom-ng cancelled the processor job |
| `INTERNAL_ERROR` | true | Unclassified processor failure |

Warnings describe optional-stage degradation. A successful result may contain
warnings only if the processing profile permits that stage to be optional.

## 17. Cancellation

`POST /v1/jobs/{job_id}/cancel` requests cooperative cancellation. It is
idempotent. The processor must terminate subprocesses it owns and clean up
temporary work that is no longer required for recovery.

Cancellation does not modify axiom-ng's durable ingest-job state directly.
axiom-ng updates its own state after observing the processor result.

## 18. Security

Local v1 requirements:

- Bind the processor API to `127.0.0.1` by default.
- Accept source paths only from configured allowed roots.
- Reject path traversal and non-regular files.
- Never expose Zotero API credentials to the processor.
- Never include database or OpenSearch credentials in a processor request.
- Do not log full document text, embeddings or secrets by default.

Remote transport (additive v1 extension): the processor pulls source bytes
from a signed, expiring URL when `local_path` is not locally accessible
(LOCAL path takes precedence). Requirements: HMAC-signed and expiring URLs,
plain-HTTP only on a trusted transport (e.g. a tailnet), http(s) schemes
only, a total download budget and byte cap, the hash gate over the
downloaded bytes, no hash-value echo in mismatch errors, and temp files that
die with the job's work dir.

## 19. Required Contract Tests

Every processor implementation must pass the same black-box test suite:

1. Health and capabilities report contract v1.
2. Repeated idempotency keys do not start duplicate processing.
3. A known PDF produces Markdown and at least one chunk.
4. Chunk text, page labels, physical page indexes and section hierarchy survive
   the API round trip.
5. Source hash mismatch fails before successful output.
6. All chunk/entity/relationship refs resolve within the result.
7. Dense and sparse embeddings match declared capabilities.
8. The processor performs no PostgreSQL or OpenSearch writes.
9. Cancellation terminates running subprocesses.
10. Acknowledgement removes temporary output and is idempotent.
11. Processor restart does not silently convert an accepted job into success.
12. No durable copy of the original PDF/EPUB remains after acknowledgement.
13. source_url delivery (additive v1 extension): a valid local_path wins over
    source_url; non-http(s) schemes are rejected; the hash gate rejects
    downloaded bytes that do not match content_hash (without echoing the
    actual hash); the downloaded temp file dies with acknowledgement.

14. Replay-after-ACK: a resubmit of an acknowledged job answers 409 with
    `code: ARTIFACTS_EXPIRED` (terminal, non-retryable) — never a result whose
    artifacts were already removed.

axiom-ng integration tests must additionally prove that an invalid or partial
processor result cannot replace the last valid processing snapshot.

## 20. Migration from the Existing Python Pipeline

The existing Python implementation already contains valuable processing logic
that should be wrapped, not rewritten:

- Marker pagination is enabled during PDF conversion.
- PDF page labels are extracted and mapped to Marker physical page markers.
- The chunker stores logical `page_start` and `page_end` values.
- The chunker stores section titles, paragraph indexes, token counts and image
  references.
- Dense/sparse embeddings, GLiNER entities and mREBEL relationships are already
  computed in Python.

Migration changes the persistence direction:

```text
Old: Python computes and writes directly to all stores.
New: Python computes and returns a contract result; Go validates and writes.
```

The first adapter should preserve current processing behavior while removing
database, OpenSearch and Zotero access from the processor boundary.
