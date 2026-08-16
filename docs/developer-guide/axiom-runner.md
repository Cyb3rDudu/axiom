# axiom_ng_runner (Python)

The **Python processor runner** is a loopback HTTP service that implements
`PROCESSOR_CONTRACT` (transport contract v1) for document processing **and**
serves the query compute (`embed`, `rerank`) for search. In both roles it owns
**only computation and temporary job output**; all durable application state
lives in `axiom_ng`.

> **Canonical sources** for this chapter are the files in the package:
> `README.md`, `config.py`, `app.py`, and `PROCESSOR_CONTRACT` (contract v1).
> This page is their universal summary for the site.

## What it is

```text
POST /v1/process           (202, async)  document processing (ingest role)
GET  /v1/health
GET  /v1/capabilities
GET  /v1/jobs/{job_id}
GET  /v1/jobs/{job_id}/result
GET  /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST /v1/jobs/{job_id}/cancel
POST /v1/jobs/{job_id}/ack
POST /v1/embed             (R1 #131)     query-embedding (query role)
POST /v1/rerank            (R2 #132)     cross-encoder rerank (query role)
```

Processing is asynchronous: `POST /v1/process` validates the source, accepts
with `202`, and enqueues compute into a background worker. The client polls
`GET /v1/jobs/{id}` until a terminal state, then fetches the result.

## Roles (R4, #134)

The runner plays two roles; the dispatcher wires which URL is which:

- **Query role** (`/v1/embed`, `/v1/rerank`) — low-latency compute for the
  search API. Defaults to the **local** always-on runner so retrieval survives a
  remote-runner outage. Override with `AXIOM_QUERY_RUNNER_URL`.
- **Ingest role** (`/v1/process`) — document processing, with a primary
  (`AXIOM_PROCESSOR_URL`) and a fallback (`AXIOM_INGEST_FALLBACK_URL`) forming a
  failover chain. The fallback defaults to a local runner (complete, ~11×
  slower — the #128 proof figure).

The dispatcher probes capabilities at startup and logs the resolved role wiring.
A missing **required ingest** capability fails the negotiation fast; a missing
**query** capability only degrades search with a warning (by design — retrieval
survives a partial runner outage). Both query endpoints
use a process-wide warm model singleton (lazy-load on first request, keep warm
afterward) so the low-latency budget is met.

## Endpoint reference

| Endpoint | Purpose | Notes |
| --- | --- | --- |
| `GET /v1/health` | Liveness | |
| `GET /v1/capabilities` | Contract version, formats, models, limits | Single source the dispatcher negotiates against. |
| `POST /v1/process` | Accepts (or dedups) a processing job | Asynchronous, 202. |
| `GET /v1/jobs/{job_id}` | Live status + stage | |
| `GET /v1/jobs/{job_id}/result` | Completed result | |
| `GET /v1/jobs/{job_id}/artifacts/{artifact_ref}` | Artifact bytes | |
| `POST /v1/jobs/{job_id}/cancel` | Cooperative cancel | |
| `POST /v1/jobs/{job_id}/ack` | Durability ack (idempotent) | Authorizes temp-file deletion. |
| `POST /v1/embed` (R1) | Dense BGE-M3 vectors for query texts | `AXIOM_PROCESSOR_MAX_QUERY_TEXTS` caps the batch. |
| `POST /v1/rerank` (R2) | Cross-encoder scores for (query, candidate) pairs, sorted desc | `AXIOM_PROCESSOR_RERANK_MAX_TEXTS` caps candidates. |

### Stage progression

Each ingest job moves through a fixed stage vocabulary (single source:
`axiom_ng_runner.PIPELINE_STAGES`):

```text
validate_source → convert → chunk → embed → entities → relationships → assemble
```

The live stage is exposed by `GET /v1/jobs/{job_id}`; after completion the same
stages are reconstructible from `manifest.stage_timings`. Query endpoints
(`/v1/embed`, `/v1/rerank`) are synchronous single-stage calls.

## Compute backends

| Backend | Use | Dependencies |
| --- | --- | --- |
| `reference` (default) | Hermetic contract tests; lightweight real conversion | fastapi, uvicorn, pydantic, pymupdf |
| `real` | Vendored `compute_core`: Marker/pdf_worker, epub_worker, embedder & extractors | torch, FlagEmbedding, gliner, mrebel, marker-pdf |

The `reference` backend converts PDF via PyMuPDF and EPUB via zipfile, reuses a
hermetic deterministic chunker, and emits contract-shaped results with honest
page/section provenance. It never touches a database, OpenSearch, graph, or
Zotero store.

## compute_core (vendored, and why)

`compute_core/` is the **vendored compute layer** — the converter workers,
chunker, embedder, entity/relation extractors, and (from R2) the reranker. It
is bundled **inside** `axiom_ng_runner/` so that shipping one tree ships all the
compute it needs; the DB-driver import chain that previously entangled these
modules stayed behind with the old codebase, which is what makes a self-contained
runner (and a container) possible. The boundary is strict: `compute_core` performs
computation and returns structured results — it never imports or writes to a
database, OpenSearch, graph, or Zotero store.

## Start

```bash
.venv/bin/uvicorn axiom_ng_runner.app:app --host 127.0.0.1 --port 8537
# or
.venv/bin/python -m axiom_ng_runner
```

Configuration (see `config.py`):

```text
AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
AXIOM_PROCESSOR_PORT=8537
AXIOM_PROCESSOR_WORK_ROOT=/tmp/axiom_processor_work
AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/path/to/zotero/storage
AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS=1
AXIOM_PROCESSOR_COMPUTE=reference          # or "real"
AXIOM_PROCESSOR_MAX_QUERY_TEXTS=16         # /v1/embed batch cap
AXIOM_PROCESSOR_RERANK_MAX_TEXTS=64        # /v1/rerank candidate cap
```

Details for all variables: [Configuration](configuration.md) (single table).

## Tests

```bash
pytest tests/ -v
```

The black-box contract tests (`PROCESSOR_CONTRACT` §19) run against the
`reference` backend and need only the runtime + pymupdf + fastapi:
health/capabilities, idempotency, PDF→markdown+chunks, page/section provenance
round-trip, hash mismatch, reference integrity, embeddings vs capabilities, no
durable-store access, cancellation, ack cleanup+idempotency, restart recovery
without fake success, and no durable source copy.

## Known limitations

The `reference` backend (default, used by the contract suite) is complete and
contract-conformant. The `real` backend (Marker/GPU compute) has open gaps that
must be closed before it is used productively — tracked here so they are not
lost:

- **Subprocess cancellation is non-functional in the real backend.**
  `_real_pipeline` runs Marker/pdf_worker via a blocking `subprocess.run` and
  does not register the process handle in `_running`; the terminate branch of
  the cancel endpoint is dead code for real jobs (contract §17). Fix: keep the
  `Popen` handle in `_running[job_id]["process"]` so `job_cancel` can call
  `terminate()`, and make the subprocess cooperative (cancel poll).
- **The real backend does not wire the GLiNER/mREBEL extractors.**
  `_real_pipeline` calls only converter + chunker + text embedder;
  entities/relations still use the reference regex extractor in both backends.
- **`prune_expired` is never scheduled**, so `result_retention_seconds` is
  currently inert and acked-job tombstones accumulate. Add a periodic sweep.
- **No request-queue cap:** every accepted POST starts an unbounded daemon
  thread; the semaphore gates only concurrency, not queue length.

Continue: [PROCESSOR_CONTRACT v1](processor-contract.md) ·
[Architecture Overview](architecture.md) · [Configuration](configuration.md)
