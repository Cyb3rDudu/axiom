# L8 End-to-End Analysis — Epic #109 Closure

**Date:** 2026-08-15 · **Issue:** #117 (Epic-Closure-Gate) · **Authors:** Implementor (measurement/text), Hivemind (verification), dudu (operations + GO decisions)
**Purpose:** This document answers the Epic question in one readable pass and preserves the
truth about the system for a year from now. All numbers are reproducible against the
production DBs `axiom_db` (TC2 state) and `axiom_db_tc1_ref` (TC1 backup); the evidence
chains live in MASS_CHUNKING_BENCHMARK.md, TC2_PARALLEL_BENCHMARK.md, CHUNK_QUALITY_ASSESSMENT.md
as well as the issue comments #109–#127.

---

## 1. The Epic question

> Does the system deliver the horizontal end-to-end run — reliably, observably, at what cost?

**Answer in one sentence:** Yes — 16/16 books error-free on 3 heterogeneous GPUs with 1.71×
throughput, distribution provable via SQL, live stage visibility per book, at ~8 GPU-minutes
per book on an RTX 3090; the end-to-end run scales as long as the GPUs are equally fast,
and every operational surface of this goal is now code, test, or checklist.

## 2. The measurement chain (all runs against the same 16-book library)

| Run | Setup | Wall-clock | Result |
| --- | --- | --- | --- |
| Benchmark (day before) | 1× 3090 serial | 2,759 s (~46 min, warm) | 16/16, 4,810 chunks — first full extraction |
| TC1 (L8) | 1× 3090 serial | 72 min (12 books in the final run) | 16/16, 0 failed after culprit-chain fixes |
| TC2 (L8) | 2× 3090 + A3000, 3 dispatchers | **56 min / 16 books** | 16/16, 0 failed, **0 duplicate processing** |

- **TC2 distribution** (per `ingest_jobs.runner_name`, pure SQL): gpu0 6 books/34.1
  compute-min · gpu1 7/43.2 · **A3000 3/53.0 (avg. 17.7 min/book)** — work-conserving without
  a load balancer: the fast cards take more, SKIP LOCKED + claim fencing make it exclusive.
- **Scaling factor:** 1.71× throughput (6.0 → 3.5 min/book) with a heterogeneous third card;
  projected 2.9× homogeneous (3× 3090 ≈ 32 min). The A3000 did not lengthen the wall-clock
  through lack of compute power, but as a straggler tail (74% busy vs 34% on the 3090s —
  it was the critical path).
- **GPU time per book:** 3090 ≈ 6 min full profile (Marker + BGE-M3 + GLiNER + mREBEL),
  A3000 ≈ 17.7 min. Stage breakdown via `manifest.stage_timings`: mREBEL dominates
  (~104 s/book), GLiNER ~34 s, embedding ~57 s — so retrieval expansion starts at the
  relation extractor, if ever needed.
- **Consistency under concurrent writers:** Outbox 16/16 done, OpenSearch doc count ==
  active chunk count (guaranteed since #127 even for force_rebuild via tombstones).

## 3. Data quality (Quality Gate, #120 precursor) — GO

- **Chunk size distribution:** median 382 tokens, 0 monster chunks; 9.2% heading anchors
  (structurally correct, low retrieval value — merge candidate, not an error).
- **Locators:** 100% coverage (4,524 page_span + 286 CFI); counter-check 3/3 **page-exact**
  against the original PDFs.
- **Retrieval (the acid test):** 5/5 realistic DE+EN queries landed on-topic in the
  right books AND sections, cross-lingual; zero semantic garbage in 25 hits.
- **Determinism:** 13/16 books byte-identical across independent runs; 1 further
  (Sonko) path-normalized identical after the tempdir leak fix; **2/16 Marker edge cases**
  (table column count, heading level — GPU float nondeterminism in the layout model).
  **Embeddings bit-exact** (cosine 1.000000 across GPU boundaries) — everything except
  Marker is deterministic.
- **Graph:** 26,353 entities, 55,537 mentions, 10,382 relations — 100% with evidence chunk;
  31% stable edges (both endpoints >1 mention); see §6 (querying filters mandatory).

## 4. The culprit chain — twelve traps and how they died

The L8 story is a staggered debugging chain: three culprits masked each other; each fix
exposed the next. In a year, this will be the most valuable section.

**Pipeline culprits (silent failures):**

1. **Silent exits in the dispatcher poll loop** — jobs rotted unmarked (`processing`
   without a worker). Fix: every exit surface marks retry/terminal + a decoupled
   renewal goroutine over the entire job lifetime (`05f7ddc`, `f55c8de`).
2. **Replay without fence-complete** — the identity-replay branch of PersistResult
   never completed the job row; the lease expired, the re-claim resubmitted to
   an ACKed runner (first a 404 wall, then cleanly `ARTIFACTS_EXPIRED`). This edge
   also retroactively explained the #126 mystery (`befa516`).
3. **force_rebuild double activation** — deactivation was scoped by profile_hash,
   but the force flag changes it → two active generations (counted ESGBS twice). Fix:
   latest-persist-wins per attachment + DB-level unique index 0011 (`a63b5eb`).
4. **OS index served superseded generations** — no tombstone → 252 orphaned docs after
   the force rebuild. Fix: outbox delete ops in the same persist TX + obsolete guards +
   self-heal (`2fe453e`, `1d4dc25`).

**Performance culprits (14–38 min job gaps):**
5. **Serial artifact staging + shared timeout** — 6-way parallelism + per-call budgets
   (`f3b00fb`+`6fc17a7`); side effect: the submit floor `max(30 s, resultBudget)` preserves
   remote source delivery.
6. **Transport ceiling 1: Tailscale utun10** — bulk collapsed to ~123 KB/s under ms-polls →
   direct LAN.
7. **Transport ceiling 2: Podman's "perfectly fine" port mapping** — same signature
   (loopback in the container 122 MB/s, mapped port 123 KB/s) → `--network=host` is
   mandatory; bulk NEVER over a tunnel, Tailscale control plane only.
8. **GLiNER CPU default** — `DEVICE_GLINER=cuda` must be set explicitly: ~1 h/book vs 5 min.
9. **defaultProfile trap** — the profile name alone switches nothing; sync now materializes
   all feature booleans explicitly (`9aaad69`).

**Operations culprits (TC2 round 1 discarded):**
10. **dockerenv trap** — `is_running_in_docker()` fails in rootless Podman (no
    `/.dockerenv`, cgroup v2 only `0::/`) → config stomped `CUDA_VISIBLE_DEVICES=0` over
    the container pinning: all 3 runners on GPU 0, one Marker OOM. Fix:
    `RUN touch /.dockerenv` in the image + **start gate** (torch device count/name per
    container + test allocation on EVERY card — the 30-second check that would have
    prevented the entire failed round). With #118, the override logic itself is dead.
11. **Migration race** — 3 dispatchers migrate a fresh DB simultaneously → 2 crash on
    `pg_type` (fail-fast, a restart suffices). Lesson: clean slate → ONE instance first.
12. **EPUB tempdir leak** — pandoc-`<img src="/tmp/epub_media_<random>/…">` in
    chunk texts made every re-run byte-different (Sonko, 27/252 chunks). Fix:
    basename normalization before chunking (`a65be86`).

**Process lessons (not code):** `go run` leaves orphan binaries behind (`pkill -f
axiom-ng`, never kill only the parent) · NEVER blast jobs away before a reset · requeue
rule: zombies keep their attempt count unchanged, `attempt=0` after exhaustion ·
un-reverted mutation probes in the worktree are build hazards (rsync builds from the
worktree).

## 5. Observability — what operations sees today

- **Who is processing what:** `ingest_jobs.runner_name` (claim time) + `runner=` in
  every phase log line → distribution, throughput, load balance as SQL.
- **Where a book stands:** live stage via `GET /v1/jobs/{id}` (validate_source → … →
  assemble) + `manifest.stage_timings` (retrospective phase reconstruction without
  live observation).
- **When the dispatcher was where:** phase lines claim→submit→completed→resultFetched→
  staged→persisted→acked; job gaps 0 s–poll interval.
- **GPU time:** labeled nvidia-smi samplers per runner (30 s cadence), unambiguously
  attributable after the log merge.
- **Error communication:** terminal codes instead of 404 hammering (`ARTIFACTS_EXPIRED` as
  the pattern: the runner is the source of truth about its artifacts).

## 6. Verified limits + requirements catalog for the RAG retrieval Epic

**Known, deliberately accepted limits:**

| Limit | Quantification | Consequence |
| --- | --- | --- |
| A3000 straggler | avg. 17.7 vs 6 min/book; critical path in TC2 | Heterogeneity is tolerated but accelerates nothing — the scaling math only holds with identical cards |
| Marker nondeterminism | 2/16 books (table/heading edge cases) | Irrelevant for retrieval; byte-identical re-runs would need deterministic torch algorithms (performance cost — decision open) |
| Entity noise | 71.6% one-hit entities; generic nouns (`companies`, `world`); relation strength constant 0.7 | The graph is a candidate space: querying MUST filter, not trust it raw |
| Sparse missing from the OS index | Dense-only retrieval proven | Hybrid needs an index field + population |

**Verified requirements catalog (derived from the Quality Gate findings, each with an
acceptance criterion):**

1. **Sparse embeddings in the OS index** (hybrid retrieval): field + outbox population +
   query merge; acceptance: knn and sparse results reconciled in a single request.
2. **Relationship-strength discrimination** (or replacement): mREBEL delivers no
   confidence — either derive it model-side or strengthen it evidence-based; until then
   the **mention-stability filter** (both endpoints ≥2 mentions ≈ the stable 31%) is a
   query requirement; acceptance: graph queries without one-hit noise edges.
3. **Entity-noun filter:** stoplist of generic nouns + minimum requirement
   (context mentions) at entity onboarding; acceptance: top entity list without
   `companies`/`world`.
4. **(Optional) heading-chunk merge + bibliography down-weighting:**
   retrieval yield per index doc, not a correctness issue.

## 7. Conclusion

The pipeline is **mechanically proven** (two independent full runs, 0 failed),
**horizontally scaling** (1.71× measured heterogeneous, 2.9× projected homogeneous, exclusivity
DB-enforced under 3 workers), **observable on three levels** (SQL/stage/phase log), and
**deterministic around the one nondeterministic component** (Marker), whose residual
risks are quantified. The data quality carries retrieval (5/5 smoke test, locators
page-exact, embeddings bit-reproducible). The operational traps are either dead (code),
pinned (tests), or preserved as a checklist (deployment doc) — the three-layer culprit
chain that cost L8 cannot repeat itself in this form without going red.

**What applies next is in §6 — no longer in this Epic.**

— *End of L8 / Epic #109. Next step: closure + archive branch.*
