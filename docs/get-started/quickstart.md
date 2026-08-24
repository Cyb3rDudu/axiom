# Quickstart

Get a minimal **axiom** setup running and process your first document — local,
with generic defaults, no private infrastructure knowledge required. For
production/multi-machine depth, follow the links to [Developer
Guide](../developer-guide/architecture.md) and [Operations](../operations/deployment.md).

## What you need

- **Zotero desktop** with the Local API enabled (Settings → Advanced →
  "Allow other applications on this computer to communicate with Zotero").
- **PostgreSQL with pgvector** and **OpenSearch** — persisted state + the
  search index.
- **A Go toolchain and a Python 3.11+ runtime** to run the two processes below.

A GPU is optional for the first test — everything runs on CPU/Apple MPS. Full
setup details (env vars, firewall, GPU) are in
[Operations → Deployment](../operations/deployment.md).

## Layout

```text
axiom dispatcher  ──HTTP contract──▶  axiom runner
owns state, queue, search index        does conversion, chunking, ML
```

Both run on loopback for a first test. To spread compute across machines (e.g.
retrieval on a local runner, heavy processing on a remote GPU), set
`AXIOM_PROCESSOR_URLS` to an ordered candidate list — e.g.
`http://gpu-host:19542,http://127.0.0.1:8012` (remote GPU first, local floor
last). The health probe skips unreachable candidates automatically, so the
same env file works at home and on the road. Details:
[Services → ingest runner selection](../operations/services.md) and the
[container deployment guide — Operations → Deployment](../operations/deployment.md).

## 1. Run the axiom runner

```bash
python3 -m venv .venv
.venv/bin/pip install -r axiom_ng_runner/requirements.txt
export AXIOM_PROCESSOR_COMPUTE=reference
export AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=<path-to-zotero-storage>  # the `storage` folder of your Zotero data dir
.venv/bin/python -m axiom_ng_runner
```

Wait for `Uvicorn running on http://127.0.0.1:8537`.

## 2. Run the axiom dispatcher

```bash
export AXIOM_ZOTERO_BASE=http://localhost:23119/api
export AXIOM_DATABASE_URL=postgres://<user>:<pass>@localhost:5432/<db>
export AXIOM_OPENSEARCH_URL=http://localhost:9200
export AXIOM_PROCESSOR_URL=http://127.0.0.1:8537
export AXIOM_DISPATCHER_ENABLED=true

cd axiom_ng && go run ./cmd/axiom-ng
```

The dispatcher checks Zotero is reachable and the runner is contract-compatible.

## 3. Sync, then watch the pipeline

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync   # mirror Zotero → ingest jobs
curl     http://127.0.0.1:8011/api/ingest/jobs       # watch pending → completed
```

A document that finishes shows `completed`; its chunks land in the searchable
index.

## If something fails

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `Zotero local API not reachable` | Local API disabled or Zotero closed | Enable Settings → Advanced → Local API; keep Zotero running. |
| Jobs stuck or fail on a source error | Runner cannot read the Zotero storage path | Point `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` at the real Zotero storage folder and restart the runner. |
| Runner never picks up work | URL/compute mismatch between dispatcher and runner | Use the same `AXIOM_PROCESSOR_URL` on both sides (loopback same host is simplest). |

More patterns: [Troubleshooting](../operations/troubleshooting.md).

Continue: [Concept Tour](concept-tour.md) · [Welcome](../index.md) ·
[Architecture Overview](../developer-guide/architecture.md)
