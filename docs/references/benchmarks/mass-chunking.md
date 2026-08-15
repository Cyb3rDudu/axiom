# Mass Chunking Benchmark

**Report type:** Measurement report (dated) · **Date:** 2026-08-14 · **Context:**
production DB build (complete Zotero library, 16 documents) via an external GPU
runner · Original: `axiom_ng/docs/benchmarks/MASS_CHUNKING_BENCHMARK.md`.

> **Env-naming note:** at run time the dispatcher variables were still
> `AXIOMNG_*`; since the rename they are `AXIOM_*` (e.g. `AXIOMNG_PROCESSOR_URL`
> → `AXIOM_PROCESSOR_URL`). Historical commands are deliberately left
> unchanged.

> This report documents the **system state as of 2026-08-14**. Figures remain
> valid as measurements; setup details are reduced to roles.

## Setup

- Dispatcher on the central host, runner container on an external GPU host
  (RTX-3090 class, CUDA container), concurrency=1, 5m lease (defaults unchanged).
- Source delivery at the time via an rsync bridge (staging the Zotero-KEY
  folders onto the runner host). **Superseded today** by the `source_url`
  mechanism (HMAC-signed download URL, see
  [Operations → Deployment](../../operations/deployment.md), §5) — no Zotero
  copies on the GPU host anymore.
- Reset before the run: DB `DROP SCHEMA public CASCADE`, OS index cleared,
  artifact root emptied, runner work root fresh.

## Results

**16/16 completed, 0 failed, 0 retries** (all `attempt=1`).

### Job table (order by completion; times from the DB)

| # | Document | Type | Size | Duration (s) |
| --- | --- | --- | --- | --- |
| 1 | nachhaltiges-management-nachhaltigkeit-… | PDF | 19 MB | **323** (cold start: model load + Triton JIT) |
| 2 | demystifying-environmental-social-governance-esg | EPUB | 11 MB | 108 |
| 3–15 | misc (Springer PDFs, ESG investing, sustainability, life cycle …) | PDF/EPUB | 2.5–9.3 MB | 84–235 |
| 16 | ganzheitliches-life-cycle-management | PDF | 17 MB | **403** (last job; stored `started_at` stale → see hygiene) |

### Totals

| Metric | Value |
| --- | --- |
| Batch wall-clock total | **2,759 s ≈ 46 min** |
| Throughput | **~20.9 documents/hour** (concurrency=1) |
| Cold/warm | first job 323 s incl. model load; warm 84–403 s, median ~114 s |
| Dispatcher overhead | ~4 s over 16 jobs (started_at(n+1) == completed_at(n)) |
| Size↔time | roughly correlated, but page-count/image-heavy dominated |

### Horizontal throughput (after the run)

| Level | Count | Consistency |
| --- | --- | --- |
| ingest_jobs completed | 16 | 0 errors, 0 retries |
| active snapshots | 16 | 1 per attachment |
| chunks | 4,810 | |
| outbox | 16 done, 0 other | follow delta ≤ 1 poll tick after job completion |
| OpenSearch chunks index | 4,810 docs == chunks | knn_vector mapping (1024) |

GPU: VRAM footprint ~2.8 GB with Marker+GLiNER+mREBEL. Unattended run → no
nvidia-smi series (documented gap, deliberate).

## Profile finding (important)

The Zotero sync enqueued with the dispatcher default profile
`{"profile":"full-rag-v1"}` — the claim materializes **all feature booleans as
`false`** (extract_entities/relationships, compute_dense/sparse). The contract
name "full-rag-v1" does NOT enable features: the runner reads the explicit
booleans, not the profile name. Consequence for this run: a pure
Marker→Markdown→chunk→locator pipeline (incl. image artifacts and OS indexing
of the texts), **without** L4 embeddings and **without** L6
entities/relationships.

For a full-RAG run the booleans must be set explicitly (either a dispatcher
profile with true booleans at sync time, or an SQL update of
`processing_profile` + `input_snapshot.processing` before the claim).

## Hygiene / measurement findings

1. **Job-reset SQL forgot `started_at`:** the aborted first attempt before the
   batch left a stale `started_at` (job 16). Extend the reset recipe with
   `started_at=NULL`.
2. `claimed_at` does not exist — the measured quantity is
   `started_at`/`completed_at`.
3. No nvidia-smi recording (unattended run) — catch up on the full-RAG run.

## Comparison values

| Environment | 3-page PDF | Note |
| --- | --- | --- |
| Apple MPS (dispatcher host) | 110–160 s | Gate-5/6 smokes |
| External GPU host cold | 150 s | incl. downloads+JIT |
| External GPU host warm | 30 s | POC |
| Library average | **0.58 s/page** | 4,787 pages total, 2,759 s batch |

> The 3-page POC (30 s warm) does not scale linearly to whole books:
> Marker layout recognition dominates on image-heavy pages.

Continue: [TC2 Parallel Test](tc2-parallel.md) · [L8 Analysis](l8-durchstich.md) ·
[Reports](../benchmarks.md)
