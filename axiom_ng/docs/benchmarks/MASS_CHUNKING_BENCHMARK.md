# Mass-Chunking Benchmark — 2026-08-14

> **Env rename note (#119, 2026-08-15):** At the time this benchmark ran, the
> dispatcher variables were still named `AXIOMNG_*` — since the rename they are
> `AXIOM_*` (e.g. `AXIOMNG_PROCESSOR_URL` → `AXIOM_PROCESSOR_URL`). The
> historical commands in this document are deliberately left un-rewritten.

Production DB setup: the complete Zotero library (16 documents) run through
the Carrier runner (RTX 3090, CUDA container), dispatcher on the Mac,
Concurrency=1, lease 5m (defaults unchanged).

## Setup & Infrastructure Findings

| Item | State |
| --- | --- |
| Runner | `runner-poc` container, Podman rootless, CDI (`--device nvidia.com/gpu=all`), RTX 3090 #0 |
| Route Mac→Carrier | **Tailscale** `100.99.105.103:8012`. `192.168.1.2:8012` is dropped by the NixOS firewall (`nixos-fw`, only 22/3389/5353/53 open) |
| Source delivery | **rsync bridge (deviation!)**: 16 Zotero KEY dirs (127 MB) → Carrier `~/Code/runner-poc/zotero-storage/`, container mount **path-preserving** `-v …:/Users/dudu/Zotero/storage:ro` + `ALLOWED_SOURCE_ROOTS=/Users/dudu/Zotero/storage` — the dispatcher `local_path` remained valid unchanged, hash gate active. The sshfs read-mount failed because the Carrier rejected the macOS sshd pubkey (debugging never happened, to be clarified later). **Delete** the staging copy **after review** |
| Reset | axiom_db: `DROP SCHEMA public CASCADE` (20 objects), migration on startup; OS index deleted; ArtifactRoot emptied; fresh runner workroot (container restart) |

## Results

**16/16 completed, 0 failed, 0 retries** (all `attempt=1`).

### Job table (ordered by completion; times from the DB)

| # | Document | Type | Size | Duration (s) |
| --- | --- | --- | --- | --- |
| 1 | nachhaltiges-management-nachhaltigkeit-… | PDF | 19 MB | **323** (cold start: model load + Triton JIT) |
| 2 | demystifying-environmental-social-governance-esg | EPUB | 11 MB | 108 |
| 3 | 978-3-642-39889-6 | PDF | 9.1 MB | 97 |
| 4 | 978-3-642-40015-5 | PDF | 6.7 MB | 185 |
| 5 | 978-3-642-53893-3 | PDF | 4.9 MB | 84 |
| 6 | 978-3-642-54882-6 | PDF | 7.6 MB | 203 |
| 7 | 978-3-642-54917-5 | PDF | 3.2 MB | 95 |
| 8 | 978-3-658-02842-8 | PDF | 3.0 MB | 114 |
| 9 | 978-3-658-04426-8 | PDF | 8.0 MB | 222 |
| 10 | 978-3-658-03600-3 | PDF | 7.2 MB | 208 |
| 11 | nachhaltige-nicht-nachhaltigkeit | PDF | 2.8 MB | 99 |
| 12 | esgbs-the-false-narrative | EPUB | 163 kB | 97 |
| 13 | environmental-social-governance-investing | PDF | 2.5 MB | 187 |
| 14 | environmental-social-and-governance-and-sustainable-development-in-healthcare | PDF | 7.7 MB | 235 |
| 15 | the-adventure-of-sustainable-performance | PDF | 9.3 MB | 95 |
| 16 | ganzheitliches-life-cycle-management | PDF | 17 MB | **403** (last job; stored `started_at` is stale → see Hygiene) |

### Totals

| Metric | Value |
| --- | --- |
| Batch total time (wall) | **2,759 s ≈ 46 min** (11:56:30–12:42:29 UTC) |
| Throughput | **~20.9 documents/hour** (Concurrency=1) |
| Cold/warm | First job 323 s incl. model load (HF volume warm, Triton JIT included); warm 84–403 s, median ~114 s |
| Dispatcher overhead | ~4 s total across 16 jobs (started_at(n+1) == completed_at(n), gap-free sequential) |
| Size↔time | Roughly correlated, but dominated by page count/image-heaviness (17 MB book: 403 s; 9.3 MB: 95 s) |

### Horizontal drill-through (post-run)

| Layer | Count | Consistency |
| --- | --- | --- |
| ingest_jobs completed | 16 | 0 errors, 0 retries |
| Active snapshots | 16 | 1 per attachment |
| Chunks | 4,810 | |
| Dense embeddings | **0** | see profile finding |
| Entities/Mentions/Relationships | **0** | see profile finding |
| Outbox | 16 done, 0 otherwise | Follow-up delta ≤ 1 poll tick (2 s) after job completion |
| OpenSearch `axiom-ng-chunks-v1` | **4,810 docs == chunks** | knn_vector mapping (1024) from the first batch ensure |

### GPU

RTX 3090 #0 (24 GB). The run was unattended → no nvidia-smi series recorded
during it (documentation gap, deliberate). POC reference: ~2.8 GB VRAM with
Marker+GLiNER+mREBEL; runner logs show artifact fetches for all jobs
(image-0000…image-0262) + ACKs from the dispatcher over Tailscale
(100.79.104.120).

## Profile Finding (important)

The Zotero sync enqueued with the dispatcher default profile
`{"profile":"full-rag-v1"}` — the claim materializes **all feature booleans
as `false`** (extract_entities/relationships, compute_dense/sparse). The
contract name "full-rag-v1" does NOT switch the features on: the runner reads
the explicit booleans (`ProcessingOptions` defaults false), not the profile
name. Consequences for this run: a pure Marker→Markdown→Chunk→Locator
pipeline (including image artifacts and OS indexing of the texts), **without**
L4 embeddings and **without** L6 entities/relationships.

For the full-RAG benchmark (Hivemind's acceptance criteria: 1024-dim
embeddings, entity type diversity) a second run with explicitly set booleans
is needed — either `AXIOMNG_DISPATCHER_PROFILE` with true booleans at sync
time, or an SQL update of `processing_profile`+`input_snapshot.processing`
before the claim (recipe from the L6 smoke). Decision point in the report.

## Hygiene & Measurement Findings

1. **Job reset SQL forgot `started_at`**: the first attempt, aborted before
   the batch, left behind a stale `started_at` (job 16: stored 3229 s instead
   of the real 403 s). Corrected figure above; extend the reset recipe with
   `started_at=NULL`.
2. `claimed_at` does not exist — the measured values are
   `started_at`/`completed_at`.
3. nvidia-smi recording missing (unattended run) — to be captured on the
   full-RAG run.

## Comparison Values

| Environment | 3-page PDF (ESG) | Note |
| --- | --- | --- |
| Mac MPS | 110–160 s | Gate-5/6 smokes |
| Carrier cold | 150 s | incl. downloads+JIT |
| Carrier warm | 30 s | POC |
| Carrier library slice | **0.58 s/page** | 4,787 pages total (sum of `manifest.source_page_count`), 2,759 s batch |

*The 3-page POC (30 s warm) does not scale linearly to whole books:
Marker layout recognition dominates on image-heavy pages.*
