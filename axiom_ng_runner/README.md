# Axiom processor runner (contract v1)

A loopback HTTP document processor implementing `PROCESSOR_CONTRACT.md`
(transport contract v1). It owns **only computation and temporary job output**;
all durable application state lives in axiom-ng.

## What it is

```
POST /v1/process  (202, async)
GET  /v1/health
GET  /v1/capabilities
GET  /v1/jobs/{job_id}
GET  /v1/jobs/{job_id}/result
GET  /v1/jobs/{job_id}/artifacts/{artifact_ref}
POST /v1/jobs/{job_id}/cancel
POST /v1/jobs/{job_id}/ack
```

Processing is asynchronous: `POST /v1/process` validates the source, accepts
202, and enqueues compute in a background worker. The client polls
`GET /v1/jobs/{id}` until a terminal state, then fetches the result.

## Compute backends

| Backend | Use | Dependencies |
| --- | --- | --- |
| `reference` (default) | Hermetic contract tests; lightweight real conversion | fastapi, uvicorn, pydantic, pymupdf |
| `real` | Wire the vendored `compute_core` Marker/pdf_worker, epub_worker, embedder & extractors | torch, FlagEmbedding, gliner, mrebel, marker-pdf |

The reference backend converts PDF via PyMuPDF / EPUB via zipfile, reuses a
hermetic deterministic chunker, and emits contract-shaped results with honest
page/section provenance. It never touches a database, OpenSearch, graph or
Zotero store.

## Run

```bash
.venv/bin/uvicorn axiom_ng_runner.app:app --host 127.0.0.1 --port 8537
# or
.venv/bin/python -m axiom_ng_runner
```

Configuration (see `config.py`):

```
AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
AXIOM_PROCESSOR_PORT=8537
AXIOM_PROCESSOR_WORK_ROOT=/tmp/axiom_processor_work
AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/path/to/zotero/storage
AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS=1
AXIOM_PROCESSOR_COMPUTE=reference   # or "real"
```

## Tests

```bash
pytest tests/ -v
```

The 12 black-box contract tests (PROCESSOR_CONTRACT §19) run against the
reference backend and need only the runtime + pymupdf + fastapi:
health/capabilities, idempotency, PDF→markdown+chunks, page/section
provenance round-trip, hash mismatch, reference integrity, embeddings vs
capabilities, no-durable-store access, cancellation, ack cleanup+idempotency,
restart recovery without fake success, and no durable source copy.

## Known limitations (Gate 5 blockers, NOT Gate 3)

The `reference` backend (default, used by the contract suite) is complete and
contract-conformant. The `real` backend (Marker/GPU compute) has known gaps
that must be closed before it is used in production (Gate 5), tracked here so
they are not lost:

- **Subprocess cancellation is non-functional for the real backend.**
  `_real_pipeline` runs the Marker/pdf_worker via a blocking `subprocess.run`
  and does NOT register the process handle in `_running`, so the cancel
  endpoint's terminate branch is dead code for real jobs (contract §17 / §9.2).
  Fix: retain the `Popen` handle in `_running[job_id]["process"]` so `job_cancel`
  can `terminate()` it, and make the subprocess cooperative (poll cancel).
- ~~**Real backend transitively imports DB-store modules.**~~ Resolved by
  the compute_core vendor move (#118): the DB-store import chain stayed
  behind with the old tree; compute_core has no driver imports at all.
- **Real backend does not wire GLiNER/mREBEL extractors.** `_real_pipeline`
  calls converter + Chunker + TextEmbedder only; entities/relations still use
  the reference regex extractor for both backends. Wire the real extractors
  before relying on real-mode entity output.
- **`prune_expired` is never scheduled**, so `result_retention_seconds` is
  currently inert and acked-job tombstones accumulate. Add a periodic sweep.
- **No request-queue cap** (§9.2): every accepted POST spawns an unbounded
  daemon thread; the semaphore gates concurrency only, not queue length.
