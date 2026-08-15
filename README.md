# axiom

**A Zotero-powered research knowledge system.** axiom turns your personal
research library into a searchable, citable knowledge base — connect Zotero,
process your documents, and search by *meaning* with exact page-level locators.

```
Zotero library → processing pipeline → searchable, citable knowledge
```

The system has three moving parts, cleanly separated:

- **`axiom_ng` (Go)** — the dispatcher and durable owner. It syncs your Zotero
  library, owns all ingest jobs, leases, retries, and state, validates every
  result, and writes the search index.
- **`axiom_ng_runner` (Python)** — pure compute. It converts PDF/EPUB to
  Markdown, chunks, embeds, and extracts entities/relationships, and hands a
  contract-shaped result back. It never owns durable state.
- **Stores** — PostgreSQL/pgvector for chunks and embeddings, OpenSearch for the
  searchable index, driven through an outbox so an outage never re-runs work.

## Documentation

| Go here | For |
| --- | --- |
| **[axiom docs](https://cyb3rdudu.github.io/axiom/)** | The full site: Welcome, Get Started, User/Developer/Operations guides, References. |
| **[PROCESSOR_CONTRACT.md](axiom_ng/docs/PROCESSOR_CONTRACT.md)** | The binding processor contract (v1). |
| **[Quickstart](https://cyb3rdudu.github.io/axiom/get-started/quickstart/)** | Set up and run your first job. |

## Repository structure

| Path | Contents |
| --- | --- |
| `axiom_ng/` | Go dispatcher, persistence, contract API, OpenSearch outbox. |
| `axiom_ng_runner/` | Python processor (contract v1) incl. vendored `compute_core`. |
| `axiom_ng/docs/` | Contract, deployment guide, benchmark reports (`benchmarks/`). |
| `docs/` | The documentation site source (MkDocs Material). |

## Quickstart (the 30-second version)

```bash
# 1. Runner (Python, reference backend — no GPU needed)
export AXIOM_PROCESSOR_COMPUTE=reference AXIOM_PROCESSOR_PORT=8537
.venv/bin/python -m axiom_ng_runner &

# 2. Dispatcher (Go)
export AXIOM_PROCESSOR_URL=http://127.0.0.1:8537 AXIOM_DISPATCHER_ENABLED=true
go run ./axiom_ng/cmd/axiom-ng &

# 3. Sync Zotero, then watch jobs complete
curl -X POST http://127.0.0.1:8011/api/zotero/sync
curl http://127.0.0.1:8011/api/ingest/jobs
```

See the full [Quickstart](https://cyb3rdudu.github.io/axiom/get-started/quickstart/)
for every env var and the troubleshooting table.

## Tests

`axiom_ng` (Go) and `axiom_ng_runner` (Python) each run their own suites — see
the READMEs in those packages. The processor contract has a black-box test
suite every implementation must pass.

## Archive

The old Python stack (axiom_backend, axiom_frontend, datalab, maestro fork)
lives on the branch
[`archive/old-axiom-python`](https://github.com/Cyb3rDudu/axiom/tree/archive/old-axiom-python),
including the history that `compute_core` grew out of. Its documentation is not
migrated into the current site.
