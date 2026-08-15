# axiom

> 📚 **Dokumentation & Site:** [cyb3rdudu.github.io/axiom](https://cyb3rdudu.github.io/axiom/) — Willkommen, Guides, Referenzen.

Zotero-gestütztes Wissenssystem: **axiom-ng** (Go-Orchestrierung + Python-Compute-Runner)
und die RAG-Datenpipeline (Marker → Chunks → BGE-M3-Embeddings → GLiNER/mREBEL →
Postgres/pgvector + OpenSearch).

## Struktur

| Pfad | Inhalt |
| --- | --- |
| `axiom_ng/` | Go-Dispatcher, Persistenz, Contract-API, OpenSearch-Outbox |
| `axiom_ng_runner/` | Python-Processor (Contract v1) inkl. `compute_core` (vendored Compute modules) |
| `docs/` | Pläne, Architektur- und Betriebsdokumentation (auch der alten Ära) |
| `axiom_ng/docs/` | Vertrag, Deployment, Benchmarks, L8-Analysen |

Einstieg: `axiom_ng/docs/PROCESSOR_CONTRACT.md` (Contract v1) und
`axiom_ng/docs/EXTERNAL_RUNNER_DEPLOYMENT.md` (GPU-Runner-Betrieb, Mac/MPS inklusive).

Test: `axiom_ng` (Go) und `axiom_ng_runner` (Python) führen ihre eigenen Suiten —
siehe die READMEs der Pakete.

> Der alte Python-Stack (axiom_backend/frontend/datalab, maestro-Fork) lebt auf
> [`archive/old-axiom-python`](https://github.com/Cyb3rDudu/axiom/tree/archive/old-axiom-python)
> weiter — inklusive der Historie, aus der `compute_core` hervorgegangen ist.
