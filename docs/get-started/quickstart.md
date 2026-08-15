# Quickstart

Get a minimal **axiom** setup running and process your first document — all with
local, generic defaults and placeholders where your environment differs. No
private infrastructure knowledge is required to follow this guide.

## What you need before you start

- **A local Zotero desktop instance** with the Local API enabled (Settings →
  Advanced → "Allow other applications on this computer to communicate with
  Zotero"). axiom talks to Zotero over `http://localhost:23119/api`.
- **PostgreSQL with pgvector** — axiom persists its durable state, chunks, and
  embeddings here. Any PostgreSQL 15+ with the `vector` extension works.
- **OpenSearch** — the searchable index. The pipeline writes its index through
  an outbox, so a temporary outage never forces a re-run.
- **A Go toolchain** (to build the dispatcher) and a **Python 3.11+ runtime** (to run the processor).
- **A GPU is optional.** Everything runs on CPU or Apple MPS for a first test;
  a GPU host comes in later for mass processing (see Operations → Deployment).

> If any requirement is daunting, start minimal: only the Go dispatcher and the
> Python `reference` processor on one host, all on loopback. A single small PDF
> end-to-end is enough to prove the pipeline.

## Layout

Two processes work together:

```text
axiom (Go dispatcher)  ──HTTP/contract──▶  axiom_ng_runner (Python processor)
owns all state, leases,                does conversion, chunking, ML
durability, search index
```

Both are configured with `AXIOM_*` environment variables. The values below are
the **local defaults** — safe to start from.

## 1. Configure the runner (Python processor)

Create a venv and run the processor in `reference` mode (no GPU, self-contained):

```bash
python3 -m venv .venv
.venv/bin/pip install -r axiom_ng_runner/requirements.txt

export AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
export AXIOM_PROCESSOR_PORT=8537
export AXIOM_PROCESSOR_COMPUTE=reference          # or "real" for GPU
export AXIOM_PROCESSOR_WORK_ROOT=/tmp/axiom_processor_work
export AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=<path-to-zotero-storage>
export AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS=1

.venv/bin/python -m axiom_ng_runner
```

Wait for a message like `Uvicorn running on http://127.0.0.1:8537`.

> `<path-to-zotero-storage>` is the `storage` folder of your local Zotero data
> directory. The processor only reads the files a job points it at; it never
> owns anything.
>
> Need notes instead of a blanket allow-any-root? The contract §18 allows
> configuring a strict list. For a first run, pointing at the storage folder is
> fine.

## 2. Configure the dispatcher (Go)

Build and run the Go sidecar that owns the sync, jobs, and index:

```bash
export AXIOM_ZOTERO_BASE=http://localhost:23119/api
export AXIOM_ZOTERO_LIBRARY=users/0              # your Zotero library id
export AXIOM_DATABASE_URL=postgres://<user>:<pass>@localhost:5432/<db>
export AXIOM_OPENSEARCH_URL=http://localhost:9200
export AXIOM_OPENSEARCH_USERNAME=<your-user>      # or leave unset for none
export AXIOM_OPENSEARCH_PASSWORD=<your-pass>      # or leave unset for none
export AXIOM_API_PORT=8011
export AXIOM_BIND_ADDR=127.0.0.1
export AXIOM_PROCESSOR_URL=http://127.0.0.1:8537
export AXIOM_DISPATCHER_ENABLED=true
export AXIOM_DISPATCHER_CONCURRENCY=1

go run ./axiom_ng/cmd/axiom-ng
```

The dispatcher checks that Zotero is reachable and the runner is
contract-compatible; it fails fast if not.

## 3. Sync your library

Ask axiom to mirror Zotero:

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync
```

This creates one *ingest job* per preferred processable attachment. Confirm with:

```bash
curl http://127.0.0.1:8011/api/ingest/jobs
```

You should see jobs in status `pending`.

## 4. Let the pipeline run

The dispatcher (enabled above) claims jobs, sends them to the runner, validates
and persists each result, and writes to the search index. Give it a few seconds
per small document, then check status:

```bash
curl http://127.0.0.1:8011/api/ingest/jobs
```

A document that finished will show `completed`. If any are `failed`, see the
Troubleshooting section — the top causes are a missing Zotero storage path, a
mismatched runner computation backend, or the index not being reachable.

## 5. Confirm the index

The chunks that end up in OpenSearch are your searchable data. The pipeline
drives it through an outbox, so the count updates shortly after jobs complete.

> That's it for retrieval-as-feature — the full search/retrieval surface is
> still rolling out (Epic #130). This quickstart proves the pipeline: Zotero in,
> processed, citable chunks out, indexed.

## Troubleshooting the common three

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `Zotero local API not reachable` at startup | Local API disabled or Zotero not running | Enable Settings → Advanced → Local API; ensure Zotero is open on `localhost:23119`. |
| Jobs stuck or `failed` with a source error | `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` doesn't include the attachment path | Point `ALLOWED_SOURCE_ROOTS` at the real Zotero storage folder and restart the runner. |
| Runner never picks up work / contract error | `AXIOM_PROCESSOR_URL` or `COMPUTE` mismatch between dispatcher and runner | Both sides must use the same URL and a runner the dispatcher can reach (loopback same host is simplest). |

Continue: [Concept Tour](concept-tour.md) · [Welcome](../index.md) ·
[Architecture Overview](../developer-guide/architecture.md)
