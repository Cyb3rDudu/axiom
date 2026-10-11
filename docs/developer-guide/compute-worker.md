# compute worker (axiom-compute-worker)

The **Python compute worker** (`axiom-compute-worker`, canonical name since F10 #304 / ADR 0001 §4; the legacy `axiom_ng_runner` module and `axiom-runner` entrypoint stay functional as warn-once aliases through 0.2.x) is a loopback HTTP service that implements
`PROCESSOR_CONTRACT` (transport contract v1) for document processing **and**
serves the query compute (`embed`, `rerank`) for search. In both roles it owns
**only computation and temporary job output**; all durable application state
lives in `axiom`.

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
POST /v1/embed             (R1)        query-embedding (query role)
POST /v1/rerank            (R2)        cross-encoder rerank (query role)
```

Processing is asynchronous: `POST /v1/process` validates the source, accepts
with `202`, and enqueues compute into a background worker. The client polls
`GET /v1/jobs/{id}` until a terminal state, then fetches the result.

## Roles

The runner plays two roles; the dispatcher wires which URL is which:

- **Query role** (`/v1/embed`, `/v1/rerank`) — low-latency compute for the
  search API. Defaults to the **local** always-on runner so retrieval survives a
  remote-runner outage. Override with `AXIOM_QUERY_RUNNER_URL`.
- **Ingest role** (`/v1/process`) — document processing, with a primary
  (`AXIOM_PROCESSOR_URL`) and a fallback (`AXIOM_INGEST_FALLBACK_URL`) forming a
  failover chain. The fallback defaults to a local runner (complete, ~11×
  slower).

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
| `POST /v1/embed` (R1) | Dense BGE-M3 vectors for query texts | `AXIOM_PROCESSOR_MAX_QUERY_TEXTS` caps the batch. Sparse vectors are returned only when explicitly requested. |
| `POST /v1/rerank` (R2) | Cross-encoder scores for (query, candidate) pairs, sorted desc | `AXIOM_PROCESSOR_RERANK_MAX_TEXTS` caps candidates. |

### Stage progression

Each ingest job moves through a fixed stage vocabulary (single source:
`axiom_compute_worker.PIPELINE_STAGES`):

```text
validate_source → convert → chunk → embed → entities → relationships → assemble
```

The live stage is exposed by `GET /v1/jobs/{job_id}`; after completion the same
stages are reconstructible from `manifest.stage_timings`. Query endpoints
(`/v1/embed`, `/v1/rerank`) are synchronous single-stage calls.

### Phase checkpoints and resume (#372)

Each phase writes a completion marker with its staged outputs to
`<work_root>/checkpoints/<attachment_id>/<key>/`, keyed by (content hash,
processing-profile hash, processor version). A retry resumes at the first
incomplete phase instead of restarting from zero; changed content, changed
profile or a new build re-keys cleanly. A force-rebuild recomputes from zero
by design — its key differs (contract §19's fresh-recompute semantics). Markers land last (atomic), a kill
mid-phase recomputes that phase wholly. `manifest.phase_reuse` names each
phase `computed` vs `reused` — an operator can tell a fresh processing from a
resumed one, and `stage_timings` witnesses ~0 elapsed for reused phases.
Retention is one key (one book's intermediates) per attachment per namespace;
saving into a new key prunes the superseded siblings within its own namespace
only. force-rebuild runs in its own namespace: it recomputes from zero by
design — it is the operator's escape hatch (contract §19; decision
2026-10-11, #372) — and can never prune the base namespace's resume basis.
Phase exceptions: `validate_source` and `assemble` carry no markers (pure
per-attempt work / deterministic rebuild), and `captions` has no live-stage
`enter()` — it rides the §9 `images` progress — so a reused captions phase
has no `stage_timings` entry.

Boundary (deliberate): phase granularity only — a phase that dies halfway
recomputes wholly; page-level resume inside a phase is out of scope.

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
is bundled **inside** the worker tree so that shipping one tree ships all the
compute it needs; the DB-driver import chain that previously entangled these
modules stayed behind with the old codebase, which is what makes a self-contained
runner (and a container) possible. The boundary is strict: `compute_core` performs
computation and returns structured results — it never imports or writes to a
database, OpenSearch, graph, or Zotero store.

## Start

```bash
.venv/bin/uvicorn axiom_compute_worker.app:app --host 127.0.0.1 --port 8537
# or
.venv/bin/python -m axiom_compute_worker
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

## Rename map — `axiom_ng_runner` → `axiom-compute-worker` (F10, #304)

ADR 0001 §4 row 3 wired with F10. Only the NAME PLATE moved — the HTTP
contract, the lease/claim protocol and the worker's own env contract are
frozen (the F01 baseline suite and the contract suite are the witnesses).

| Surface | Canonical (0.2.x) | Legacy alias (through 0.2.x) |
| --- | --- | --- |
| Folder | `axiom-compute-worker/` | — (repo-internal move) |
| Python module | `axiom_compute_worker` | `axiom_ng_runner` (warn-once package) |
| Entrypoint | `python -m axiom_compute_worker`, console script `axiom-compute-worker` | `python -m axiom_ng_runner`, script `axiom-runner` |
| Artifact | `axiom-compute-worker-<version>-macos-arm64.tar.zst` | pre-F10 tarballs install unchanged |
| Install | `/opt/axiom/compute-worker/<version>/`, shim `bin/axiom-compute-worker` | wrapper `bin/axiom-runner` (warns once, delegates) |
| make target | `make compute-worker` | `make runner` (deprecation echo) |
| launchd | `com.axiom.compute-worker` | old label retires with the operator switch |
| Dispatcher env | `AXIOM_COMPUTE_WORKER_{URL,URLS,NAME,TIMEOUT,SOURCE_SECRET,SOURCE_BASE_URL,HEALTH_INTERVAL}` | the `AXIOM_PROCESSOR_*` / `AXIOM_RUNNER_HEALTH_INTERVAL` spellings (witnessed via `/api/health/deprecations`) |
| Worker env | `AXIOM_PROCESSOR_*` (unchanged) | — (frozen per #304; NOT renamed) |

**Alias runtime:** aliases are functional through all of 0.2.x; removal
happens earliest in an announced major, never silently, and only after the
exported deprecation counters justify it (ADR 0001 §6). Importing the alias
warns exactly once per process and counts on every use
(`/v1/capabilities` → `deprecations.axiom_ng_runner`).

**Residual risks for remote-carrier operators:** old carrier trees keep
running untouched (their code never renamed). A carrier that re-syncs the
repo must rsync `axiom-compute-worker/` instead of `axiom_ng_runner/` and
update the Containerfile: `PYTHONPATH` must point at the PROJECT dir
(`/app/axiom-compute-worker`), not its parent — pre-F10 the tree root was
itself the package, now the packages sit one level inside (see the
Containerfile in `EXTERNAL_RUNNER_DEPLOYMENT.md`). With PYTHONPATH on the
project dir, `CMD ["python", "-m", "axiom_ng_runner"]` KEEPS WORKING
(warn-once alias); with PYTHONPATH on `/app` BOTH the canonical and the
legacy module fail to resolve. Dispatcher-side env may stay on the legacy
spellings; each process warns once and reports the use. Role vocabulary
(`AXIOM_QUERY_RUNNER_URL`, `AXIOM_INGEST_FALLBACK_URL`) deliberately keeps
its spelling — F09 froze the topology semantics.

**Alias scope:** the alias covers the package entrypoint
(`python -m axiom_ng_runner`, `import axiom_ng_runner`, the console
script) and re-exports the canonical top-level names. It deliberately does
NOT register submodules — `import axiom_ng_runner.config` does not resolve;
migrate such imports to `axiom_compute_worker.config` (ADR 0001 §4
promises the module ENTRYPOINT, not the full submodule tree).

Continue: [Processor Contract](processor-contract.md) ·
[Architecture Overview](architecture.md) · [Configuration](configuration.md)
