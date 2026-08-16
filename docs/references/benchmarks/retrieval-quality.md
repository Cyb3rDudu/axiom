# Retrieval Quality Benchmark

**Report type:** Measurement report (dated) · **Date:** 2026-08-16 · **Context:**
retrieval arm selection · **Data basis:** a 25-query gold suite (DE+EN,
concept/fact/norm/author), a real OpenSearch index (4,813 chunks), a real query
runner (warm), and the real database. Original:
`axiom_ng/docs/RETRIEVAL_BENCHMARK.md`. Reproducible with the retrieval-bench
command (see the canonical file).

> **Provisional gold caveat (important):** the gold annotations in this report
> are **provisional — derived by the implementor from the library's book titles,
> and not confirmed (0/25 are confirmed)**. Until the gold set is confirmed, the
> rankings below are provisional: queries whose gold is effectively a book title
> favor exact title signals. The suite is built so that confirming/correcting
> the gold annotations can flip the picture — that confirmation is the follow-up.
>
> This report documents the **system state as of 2026-08-16**; the figures are
> real measurements from the pinned run (a second run confirms the metrics to
> ±0.001; latencies vary with model-warm state).

## The question

Which retrieval arms should axiom run by default, given the measured quality and
latency?

## The measured configurations

| Configuration | P@5 | MRR | R@10 | p50 | p95 | Errors |
| --- | --- | --- | --- | --- | --- | --- |
| dense-only | **0.680** | **0.865** | 0.967 | 71 ms | 85 ms | 0 |
| hybrid (dense + bm25) | 0.608 | 0.815 | **0.987** | 69 ms | 90 ms | 0 |
| hybrid + rerank | 0.616 | 0.818 | 0.967 | 6.38 s | 7.18 s | 0 |
| + sparse | 0.608 | 0.791 | 0.947 | 7.38 s | 8.49 s | 0 |
| + graph | 0.600 | 0.790 | 0.927 | 9.16 s | 9.50 s | 0 |

Metrics: **P@5** = gold hits in the top 5; **MRR** = 1/rank of the first gold
hit; **R@10** = gold books found / |gold|. `top_n=10`, overfetch 3× = 30 rerank
candidates.

## Reading the numbers

1. **The rerank thesis is not confirmed on the provisional suite for
   precision.** Dense-only beats hybrid+rerank on P@5 (0.680 vs 0.616) and MRR
   (0.865 vs 0.818); rerank over hybrid adds only margin (+0.008 P@5, +0.003
   MRR). The suite is optimized for confirming/correcting this.
2. **The recall story holds.** Hybrid reaches R@10 0.987 vs dense-only 0.967 —
   the reason hybrid exists (BM25 catches what dense misses).
3. **Sparse is no gain at high cost.** MRR −0.027 and R@10 −0.020 vs
   hybrid+rerank, for ~+1.3 s p95. Expected niche: rare tokens (norm numbers,
   acronyms across languages) — the suite has too few such queries to isolate.
4. **Graph is slightly harmful and costly.** MRR −0.001, R@10 −0.020, +1.0 s
   p95; the graph-candidates SQL is not yet tuned. Default-off confirmed.
5. **Latency budget (top_n=10):** rerank costs ~4–6.4 s p50 locally. For a 2 s
   budget: a remote runner (CUDA, measured 0.95 s p95 at top_n=5), overfetch 2×
   (~20 candidates, −33 %), or top_n=5.

## Chosen production defaults

Per the benchmark, the compiled defaults are: **hybrid** (dense + bm25) as the
arms, **rerank on** (marginal but consistent; latency steered via a remote
runner), **sparse off** and **graph off** (both measured as no gain for their
cost), `rrfK=60` untuned. These are the defaults the running system uses; the
arms remain independently exercisable via config (see
[Configuration](../../developer-guide/configuration.md)).

## Open items

- **Confirming the gold annotations** (the 25 queries) — only then is the
  rerank thesis finally answered and the provisional status lifted.
- A rare-token sub-suite (norm numbers / acronyms) to give the sparse arm a
  fair profile.
- Graph-candidates SQL tuning, should the graph arm ever go productive.

Continue: [Mass Chunking](mass-chunking.md) · [Benchmarks & Analyses](../benchmarks.md)
