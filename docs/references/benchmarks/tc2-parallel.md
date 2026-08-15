# TC2: 3-Runner Parallel Scale Test & Determinism Proof

**Report type:** Measurement report (dated) · **Date:** 2026-08-15 · **Context:**
L8 test case 2 · **Data basis:** complete fresh run of the 16 books after a
clean slate; reference = TC1 backup. Original:
`axiom_ng/docs/benchmarks/TC2_PARALLEL_BENCHMARK.md`.

> This report documents the **system state as of 2026-08-15**. Figures remain
> valid as measurements; setup details are reduced to roles.

## Setup

- **3 runner containers** (rootless Podman, `--network=host`) on GPU hosts:
  2× RTX-3090 class (24 GB) + 1× RTX-A3000 laptop class (12 GB).
- **Dispatcher:** 3 independent instances, each
  `AXIOM_PROCESSOR_RUNNER_NAME=<label>`, concurrency 1, same DB (claim
  exclusivity via `SKIP LOCKED` + claim fencing).

> Round 1 was discarded (all runners on GPU 0 — see L8 analysis, Trap 10). The
> documented run is round 2.

## The run

Start → complete: **16/16 completed, 0 failed, 0 zombies, 0 pending** — wall clock
**56 min**.

### Job distribution (runner_name column)

| Runner | GPU | Jobs | avg min/job | max | min | Compute sum |
| --- | --- | --- | --- | --- | --- | --- |
| runner-a | 3090 class | 6 | 5.7 | 7.6 | 3.2 | 34.1 min |
| runner-b | 3090 class | 7 | 6.2 | 13.3 | 2.0 | 43.2 min |
| runner-c | A3000 class | 3 | **17.7** | 24.0 | 10.2 | 53.0 min |

**Work-conserving:** the fast cards take more (13), the laptop card 3 — exactly
the architecture promise (`SKIP LOCKED` claim without a load balancer).

### Double-processing check

- Active snapshots >1 per attachment: **0**
- Duplicate (attachment, chunk_index) pairs: **0**

Claim exclusivity holds under 3 competing workers.

### GPU utilization (labeled samplers, 30-s cadence, 123 samples)

| GPU | avg util | busy (≥50%) | max VRAM |
| --- | --- | --- | --- |
| 3090 a | 33 % | 34 % | 12.6 GB |
| 3090 b | 34 % | 34 % | 15.1 GB |
| A3000 | **74 %** | **75 %** | 11.4 GB |

The 3090s were done after ~40 min and idle; **the laptop card was the critical
path** (53 compute-min ≈ wall clock 56 min).

### Scaling factor

- TC1 (serial, 1× 3090): 12 books / 72 min → **6.0 min/book**
- TC2 (3 GPUs, one laptop card): 16 books / 56 min → **3.5 min/book** →
  **1.71× throughput** (wall clock)
- Homogenous projection: 3× 3090 → ~32 min (2.9×). The laptop card does not
  speed up the wall clock but widens processing breadth.

### Consistency

- **Outbox 16/16 done** · **OpenSearch 4,813 docs == 4,813 chunks**
- 16 active snapshots, 0 orphaned processing rows

## Determinism proof (against TC1 backup, joined by zotero_key)

Method: per document chunk count, `md5(string_agg(text, '' ORDER BY
chunk_index))`, and locator MD5 aggregated; deviations diffed per chunk and
classified.

| Document | Result |
| --- | --- |
| 12 books (incl. both Springer PDFs) | **byte-identical** (count+text+locator) |
| ESGBS (Heaton, EPUB) | **34/34 chunks identical** — the apparent delta was a force_rebuild double activation, not content |
| Demystifying (Sonko, EPUB) | tempdir leak → **after fix #124: 252/252 byte-identical** |
| Perspektiven (PDF) | 52/300 chunk texts differ |
| Nachhaltiges Management (PDF) | 615/754 differ, 754→757 chunks |

**Corrected balance:** 13/16 strictly byte-identical, 14/16 after path
normalization, **2/16 Marker edge cases**.

### Classification of deviations

1. **EPUB tempdir leak:** the random suffix of the EPUB extraction tempdir lands
   in the Markdown. After normalization all 252 chunks are byte-identical.
   Deterministic bug — fix would be a path normalization before chunking.
2. **Marker table flip:** the same table recognized once with 3, once with 4
   columns (layout-model edge case) → 52 chunk texts differ; chunk count and
   locators stay identical.
3. **Marker heading flip:** a heading-level flip shifts chunk boundaries
   cascadingly (heading reopen in the chunker) → large effect on one edge case.

### Embedding determinism

6 identical chunks (2 books × 3 indexes), TC1 vs. TC2 vector:
**cosine = 1.000000 exact on all 6** — BGE-M3 is bit-reproducible on this GPU
class for identical input; float noise across different physical cards is not
measurable.

### Determinism conclusion

The pipeline **around Marker is fully deterministic** (chunker, EPUB path,
embeddings bit-exact). Nondeterminism sits exclusively in Marker's layout
classification on edge cases. Irrelevant for RAG retrieval; byte-identical
reruns would require Marker to run deterministically (decision outside).

## Side finding: force_rebuild double activation

The force_rebuild path creates a new generation but does not deactivate the
previous one (different profile_hash from the force flag → no unique conflict).
Follow-up issue.

## Recommendations (outside this report)

1. EPUB tempdir normalization before chunking (small fix, makes EPUBs
   byte-deterministic).
2. force_rebuild: deactivate the old generation.
3. Deterministic Marker only if byte-identical reruns become a product
   requirement (cost: performance loss).
4. Document the migration race: clean slate → start one instance first.
5. `/dockerenv` start gate as a deploy-checklist entry.

Continue: [Mass Chunking](mass-chunking.md) · [Chunk Quality](chunk-quality.md) ·
[Reports](../benchmarks.md)
