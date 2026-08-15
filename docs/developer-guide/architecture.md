# Architecture Overview

> This is the entry chapter for developers. D5 deepens individual components and
> adds a mermaid diagram; here is the overview.

axiom is a **Zotero-powered research knowledge system**: an RAG data pipeline
from the Zotero library, through document processing, to a searchable knowledge
base of chunks, entities, and relationships.

## Core principle

```text
Zotero (sources + metadata)
        │ local path + immutable metadata snapshot
        ▼
axiom (Go) — dispatcher, leases, fencing, outbox, durable persistence
        │ HTTP/JSON: POST /v1/process
        ▼
axiom_ng_runner (Python) — compute only (conversion, chunking, ML)
        │ result + artifacts back
        ▼
axiom validates + persists (PostgreSQL/pgvector/graph, outbox→OpenSearch)
```

The central property: **axiom orchestrates, the Python runner computes, and only
axiom owns durable state.**

## The three layers

1. **Go orchestration (`axiom_ng`)** — Zotero sync, ingest jobs, atomic lease
   claims with fencing, retries, cancellation, persistent IDs, versioned
   processing snapshots, durable derived artifacts, chunks/embeddings/entities/
   relationships, PostgreSQL/pgvector and graph write paths, OpenSearch outbox.
   [Continue: axiom_ng (Go)](axiom-go.md)
2. **Python compute (`axiom_ng_runner`)** — a loopback HTTP processor per
   `PROCESSOR_CONTRACT` v1, with vendored `compute_core` (Marker conversion,
   chunker, BGE-M3 embedder, GLiNER/mREBEL extractors). It owns only computation
   and temporary job output, never durable application state.
   [Continue: axiom_ng_runner](axiom-runner.md)
3. **Transport rule (contract v1)** — dispatcher and runner exchange only the
   HTTP contract (`PROCESSOR_CONTRACT`). Bulk flows (result JSON, artifact
   bodies) need direct LAN reachability in both directions.
   [Details in the Deployment chapter](../operations/deployment.md)

## The transport boundary (decisive)

The runner is **pure compute** — it never touches Zotero, Postgres, OpenSearch,
or the graph. Only the contract crosses the wire. The dispatcher negotiates
capabilities at startup and fails if the runner is unreachable or
contract-incompatible. Bulk data runs directly over LAN; a tunnel serves only
the control plane (the third masking layer of a historical transport problem).

## Invariants (from the resolved work order)

- Only the current lease owner may mutate an active job.
- Every job mutation after the claim is fenced by the lease token.
- A stale worker can neither complete nor fail a reclaimed job.
- No DB transaction stays open during CPU/model execution (Go never holds a
  transaction while waiting on Python).
- A processor result is untrusted input until Go fully validates it.
- Result persistence is atomic; a failure preserves the previous active
  snapshot.
- A job becomes `completed` only after the result + artifacts are durably
  committed.
- Processor ACK happens only after the durable commit and is idempotent.
- Original PDF/EPUB are read in place and never durably copied.

## Where to continue?

- Contract in detail: [PROCESSOR_CONTRACT v1](processor-contract.md)
- Dispatcher/leases/persistence: [axiom_ng (Go)](axiom-go.md)
- Python runner + known limitations: [axiom_ng_runner](axiom-runner.md)
- Operations: [Operations → Deployment](../operations/deployment.md)
- Configuration + testing: follows in D5

Continue: [axiom_ng (Go)](axiom-go.md) · [axiom_ng_runner (Python)](axiom-runner.md)
