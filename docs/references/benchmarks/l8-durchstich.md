# L8 Throughput Analysis

**Report type:** Measurement report (dated) · **Date:** 2026-08-15 · **Context:**
closure-gate analysis · **Data basis:** production DBs (`axiom_db`, TC2 state;
TC1 backup), reproducible. Original: `axiom_ng/docs/benchmarks/L8_DURCHSTICHS_ANALYSE.md`.

> This report documents the **system state as of 2026-08-15**. The figures are
> real measurements; the lessons (transport, fencing, GPU pinning) are preserved
> as operating rules in the operations chapters.

## The question posed

> Does the system deliver the horizontal throughput — reliably, observably, and
> at what cost?

**Answer in one sentence:** Yes — 16/16 books processed cleanly on 3
heterogeneous GPUs at 1.71× throughput, distribution provable via SQL, live
stage visibility per book, at ~6 GPU-minutes per book on an RTX-3090-class card;
throughput scales as long as the GPUs are equally fast.

## The measurement chain (all runs against the same 16-book library)

| Run | Setup | Wall clock | Result |
| --- | --- | --- | --- |
| Benchmark (previous day) | 1× 3090 serial | 2,759 s (~46 min, warm) | 16/16, 4,810 chunks — first full extraction |
| TC1 (L8) | 1× 3090 serial | 72 min (12 books in the final run) | 16/16, 0 failed after offender-chain fixes |
| TC2 (L8) | 2× 3090 + A3000, 3 dispatchers | **56 min / 16 books** | 16/16, 0 failed, **0 double-processing** |

- **TC2 distribution** (per `ingest_jobs.runner_name`, pure SQL): gpu0 6
  books/34.1 compute-min · gpu1 7/43.2 · **A3000 3/53.0 (avg 17.7 min/book)** —
  work-conserving without a load balancer: fast cards take more, `SKIP LOCKED`
  + claim fencing exclusive.
- **Scaling factor:** 1.71× throughput (6.0 → 3.5 min/book) with a
  heterogeneous third card; homogenous projection 2.9× (3× 3090 ≈ 32 min). The
  laptop card did not lengthen the wall clock because of compute shortage, but
  as a straggler tail (74% busy vs 34% on the 3090s).
- **GPU time per book:** 3090 class ≈ 6 min full profile (Marker + BGE-M3 +
  GLiNER + mREBEL), laptop class ≈ 17.7 min. Stage breakdown via
  `manifest.stage_timings`: mREBEL dominates (~104 s/book), GLiNER ~34 s,
  embedding ~57 s.
- **Consistency under concurrent writers:** outbox 16/16 done,
  OpenSearch doc-count == active chunk count.

## Data quality (quality-gate precursor) — GO

- **Chunk layout:** median 382 tokens, 0 monster-chunks; 9.2% heading anchors
  (structurally correct, retrieval-low-value — merge candidate, not a bug).
- **Locators:** 100% coverage (4,524 page_span + 286 CFI); counter-check
  page-exact against original PDFs.
- **Retrieval:** 5/5 realistic DE+EN queries on-topic in the right books and
  sections, cross-lingual; zero semantic garbage in 25 hits.
- **Determinism:** 13/16 books byte-identical across independent runs; one more
  path-normalized identical after the tempdir-leak fix; **2/16 Marker edge cases**
  (table columns, heading level — GPU float nondeterminism in the layout model).
  **Embeddings bit-exact** (cosine 1.000000 across GPU boundaries).
- **Graph:** 26,353 entities, 55,537 mentions, 10,382 relations — 100% with
  evidence chunk; 31% stable edges.

## The offender chain — twelve traps and their remediation

Historically a staged debugging chain: three offenders masked each other; each
fix uncovered the next. **Today they are code, test, or checklist.**

**Pipeline (silent failures):**

1. **Silent exits in the dispatcher poll loop** — jobs rotted unmarked. Fix:
   every exit surface marked retry/terminal + decoupled renewal goroutine.
2. **Replay without fence-complete** — the identity-replay branch never
   completed the job row; the lease expired. Fix: clean terminal behavior after
   ACK.
3. **force_rebuild double activation** — Fix: latest-persist-wins per attachment
   + DB-level unique index.
4. **OS index served superseded generations** — no tombstone → orphaned docs.
   Fix: outbox delete-ops in the same persist transaction + obsolete guards +
   self-heal.

**Performance (14–38 min job gaps):**
5. **Serial artifact staging + shared timeout** — Fix: bounded parallelism +
   per-call budgets; side effect: the submit floor `max(30 s, resultBudget)`
   preserves remote source delivery.
6. **Transport ceiling 1: tunnel bulk collapse** — Fix: direct LAN.
7. **Transport ceiling 2: port forwarding in the container runner** — Fix:
   `--network=host` is mandatory; bulk never over the tunnel, tunnel only for
   the control plane.
8. **GLiNER CPU default** — `DEVICE_GLINER=cuda` must be explicit: ~1 h/book
   vs. 5 min.
9. **defaultProfile trap** — the profile name alone switches nothing; sync
   materializes all feature booleans explicitly.

**Operations (TC2 round 1 discarded):**
10. **dockerenv trap** — container detection failed in the rootless runner
    → the override trampled the GPU pinning (all runners on card 0, Marker OOM).
    Fix: `RUN touch /.dockerenv` + start gate (torch device-count/name per
    container + test allocation on every card — the 30-s check that would have
    prevented the whole failed round).
11. **Migration race** — 3 dispatchers migrate a fresh DB simultaneously → 2
    crash on the `pg_type` conflict (fail-fast, restart suffices). Lesson: clean
    slate → ONE instance first.
12. **EPUB tempdir leak** — pandoc `<img src="/tmp/epub_media_<random>/…">` in
    chunk texts made reruns byte-different. Fix: basename normalization before
    chunking.

**Process lessons (not code):** kill orphan binaries (`pkill -f axiom_ng`, never
only the parent) · never "blast away" jobs before a reset · requeue rule: keep
zombies attempt-unchanged, after exhaustion `attempt=0` · un-rebuilt mutation
probes in the worktree are build hazards.

## Observability — what operations sees

- **Who processes what:** `ingest_jobs.runner_name` (claim time) + `runner=` in
  every phase-log line → distribution, throughput, load balance as SQL.
- **Where a book is:** live stage via `GET /v1/jobs/{id}` +
  `manifest.stage_timings`.
- **Where the dispatcher was when:** phase lines
  claim→submit→completed→resultFetched→staged→persisted→acked.
- **GPU time:** labeled nvidia-smi samplers per runner (30-s cadence), uniquely
  attributable after log merge.
- **Error communication:** terminal codes instead of 404 hammering
  (`ARTIFACTS_EXPIRED` as the pattern: the runner is the source of truth about
  its artifacts).

Details: [Monitoring](../../operations/monitoring.md).

## Tested limits + requirements catalog for the RAG-retrieval epic

**Consciously accepted limits:**

| Limit | Quantification | Consequence |
| --- | --- | --- |
| Laptop card as straggler | avg 17.7 vs. 6 min/book; critical path | Heterogeneity is tolerated but does not speed up — scaling only works with equal cards |
| Marker nondeterminism | 2/16 books (table/heading edge cases) | Irrelevant for retrieval; byte-identical reruns would need deterministic torch algorithms (performance cost) |
| Entity noise | 71.6% one-hit entities; generic nouns (`companies`, `world`); constant relation strength | The graph is a candidate space: querying MUST filter, not trust raw |
| Sparse missing from the OS index | dense-only retrieval proven | Hybrid needs an index field + population |

**Requirements catalog (derived from the findings):**

1. **Sparse embeddings in the OS index** (hybrid retrieval).
2. **Relationship-strength discrimination** (or replacement); until then a
   mention-stability filter as query requirement.
3. **Entity-noun filter:** stoplist of generic nouns + minimum mentions.
4. **(Optional)** heading-chunk merge + bibliography downweighting.

## Conclusion

The pipeline was **mechanically proven** (two independent full runs, 0 failed),
**horizontally scaling** (1.71× heterogeneous measured, 2.9× homogeneous
projected, exclusivity under 3 workers DB-enforced), **observable on three
levels**, and **deterministic around the single nondeterministic component**
(Marker). The operational traps are dead (code), pinned (tests), or preserved as
a checklist — the three-layer offender chain cannot repeat in this form without
turning red.

Continue: [Reports](../benchmarks.md) · [TC2 Parallel Test](tc2-parallel.md)
