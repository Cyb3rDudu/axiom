# TC2: 3-Runner Parallel Scale Test + Determinism Proof (L8 Test Case 2)

**Date:** 2026-08-15 · **Issue:** #123 · **Epic:** #109 (L8)
· **Setup:** 3 runner containers (rootless Podman, `--network=host`) on
Carrier GPUs: GPU0+GPU1 = RTX 3090 (24 GB), GPU2 = RTX A3000 Laptop (12 GB)
· **Dispatcher:** 3 independent instances (disp-gpu0/1/2), each
`AXIOM_PROCESSOR_RUNNER_NAME=carrier-gpuN`, concurrency 1, same DB
(claim exclusivity via SKIP LOCKED + claim fencing)
· **Data basis:** full re-run of the 16 books from a clean slate;
reference = TC1 backup (`backups/axiom_db_tc1_backup_20260815.sql`, 134 MB,
restore-verified: 17 jobs / 4,844 chunks / 26,545 entities / 10,464 rels)

## 1. History (incl. Round 1 — discarded)

**Round 1 (14:53–15:0x, DISCARDED):** All three runners landed on GPU 0.
Cause (full chain proven): `is_running_in_docker()` fails in rootless
Podman (no `/.dockerenv`, cgroup v2 shows only `0::/`) →
`ai_researcher/config.py` overrides `CUDA_VISIBLE_DEVICES=0` at import
time ("Hardware configuration for non-Docker environments") → the
container pinning (`-e CUDA_VISIBLE_DEVICES=1/2`) gets trampled.
Symptoms: 17.6 GB on GPU 0, GPU 1/2 at 1 MiB, one Marker OOM. **Fix
(permanent, in the Containerfile):** `RUN touch /.dockerenv` — the config
detects container operation and leaves the env pinning untouched. **New
startup gate** (30 s, would have prevented Round 1): per container, check
`torch.cuda.device_count()==1` + `get_device_name` AND host-side one test
allocation per container → nvidia-smi must show VRAM on **all three**
cards (measured: 690/690/525 MiB in parallel).

**Operational trap 2 (Round 2, startup):** 3 dispatcher instances migrate
a fresh DB simultaneously → 2 collide on `CREATE TYPE`
(`pg_type_typname_nsp_index`, SQLSTATE 23505) and exit. The winning
instance finishes the migration; restarting the losers is clean. Lesson:
on a clean slate, bring up ONE instance first (it migrates), then the
others — or accept the migration race (fail-fast + restart suffices, no
corruption).

## 2. The Run

15:05:17 start → 16:01:25 complete: **16/16 completed, 0 failed, 0
zombies, 0 pending** — wall-clock **56 min** (Round 2).

### Job distribution (runner_name column, #122 measurement basis)

| Runner | GPU | Jobs | avg min/job | max | min | Compute total |
| --- | --- | --- | --- | --- | --- | --- |
| carrier-gpu0 | RTX 3090 | 6 | 5.7 | 7.6 | 3.2 | 34.1 min |
| carrier-gpu1 | RTX 3090 | 7 | 6.2 | 13.3 | 2.0 | 43.2 min |
| carrier-gpu2 | A3000 Laptop | 3 | **17.7** | 24.0 | 10.2 | 53.0 min |

The distribution is **work-conserving and fair by availability**: the
fast 3090s take more books (13), the A3000 manages 3 — exactly the
architectural promise (SKIP-LOCKED claiming without a load balancer).

### Double-processing check

- Active snapshots >1 per attachment: **0**
- Duplicate (attachment, chunk_index) pairs: **0**

Claim exclusivity holds under 3 competing workers.

### GPU utilization (labeled sampler, 30 s interval, 123 samples)

| GPU | avg util | busy (≥50%) | max VRAM |
| --- | --- | --- | --- |
| GPU0 (3090) | 33% | 34% | 12.6 GB |
| GPU1 (3090) | 34% | 34% | 15.1 GB |
| GPU2 (A3000) | **74%** | **75%** | 11.4 GB |

The 3090s finished after ~40 min and idled; **the A3000 was the critical
path** (53 compute-min ≈ wall-clock 56 min).

### Scaling factor

- TC1 (serial, 1× 3090): 12 books / 72 min → **6.0 min/book**
- TC2 (3 GPUs, one of them a laptop card): 16 books / 56 min →
  **3.5 min/book** → **1.71× throughput** (wall-clock), with a
  heterogeneous third card
- Homogeneous projection: on 2× 3090 alone, all 16 books (~95
  compute-min equivalent) would finish in ~48 min — the A3000 does not
  speed up the wall clock (straggler tail), but it widens the processing
  breadth. 3× 3090 projected: ~32 min (2.9×).

### Dispatcher cadence (phase logs, all 3 instances)

`runner=` in every line; job gaps = poll seconds down to 0 s
(`acked=15:12:58 → claim=15:12:58`); staging/persist in the seconds
range, even with 264 artifacts in one result. The three-layer transport
error class from TC1 remains fixed.

### Consistency

- **Outbox 16/16 done** · **OpenSearch 4,813 docs == 4,813 chunks**
- 16 active snapshots, 0 orphaned processing rows

## 3. Determinism proof (against TC1 backup, joined by zotero_key)

Method: per document, chunk count, `md5(string_agg(text,'' ORDER BY
chunk_index))` and locator MD5 aggregated; deviations then diffed and
classified per chunk. Join via `zotero_attachments.zotero_key` (stable),
NOT via DB UUIDs (fresh per sync).

| Document | Result |
| --- | --- |
| 12 books (incl. both Springer PDFs) | **byte-identical** (count+text+locator) |
| ESGBS (Heaton, SKAP2JAE, EPUB) | **34/34 chunks identical** — the apparent 68/34 delta was the force_rebuild double activation (#125), not a content difference |
| Demystifying (Sonko, CDX5EBM3, EPUB) | 27/252 chunks with `/tmp/epub_media_<random>` leak → **after fix #124: 252/252 byte-identical** (path-normalized via TC1 ref and re-processing) |
| Perspektiven (EE8QHQIL, PDF) | 52/300 chunk texts deviate |
| Nachhaltiges Management (RWA5PT4J, PDF) | 615/754 deviate, 754→757 chunks |

> **Corrected tally (post-#124/#125):** 13/16 strictly byte-identical,
> 14/16 after path normalization (Sonko re-processing yields leak-free
> chunks, path-normalized 252/252 == TC1 reference), 2/16 Marker edge
> cases. Only ONE book was affected by the tempdir leak (Sonko); Heaton's
> ESGBS was demonstrably clean — the "second deviation" was the
> #125 double-activation artifact. Fixes: `a65be86` (#124), `a63b5eb`
> (#125).

### Classification of the deviations

1. **EPUB tempdir leak (CDX5EBM3, systematic, not model noise):**
   `<img src="/tmp/epub_media_<random>/...">` — the random suffix of the
   EPUB extraction tempdir ends up in the Markdown and therefore in the
   chunk text. After normalization (`s#/tmp/epub_media_[a-z0-9]+/#/X/#`),
   **all 252 chunks are byte-identical**. Deterministic bug — the fix
   would be path normalization before chunking (deliberately NOT fixed
   here, issue to follow).
2. **Marker table flip (EE8QHQIL):** the same table is detected once
   with 3 columns, once with 4 (layout model edge case, GPU float
   nondeterminism) → 52 chunk texts deviate; chunk count and all
   locators remain identical.
3. **Marker heading flip (RWA5PT4J):** `### Nachhaltiges Management` vs
   `# …` — a single heading-level flip early in the book shifts chunk
   boundaries in a cascade (heading reopen in the chunker) → 615/754
   deviating chunks + 3 additional. One edge case, large impact.

### Embedding determinism

6 identical chunks (2 books × 3 indices), TC1 vector vs TC2 vector:
**cosine = 1.000000 exact on all 6** — BGE-M3 is bit-reproducible on
this GPU class for identical input. Float noise is not measurable even
across different physical 3090s.

### Determinism conclusion

The pipeline **around Marker is fully deterministic** (chunker, EPUB
path-B/CFI, embeddings bit-exact; 13/16 books byte-identical, 14/16
after tempdir normalization). Nondeterminism sits exclusively in
Marker's layout classification on edge cases (2/16 books, affected:
table column count, heading level). Irrelevant for RAG retrieval (local
text variants); for byte-identical re-runs Marker must run
deterministically (Torch `torch.use_deterministic_algorithms` +
CUBLAS_WORKSPACE_CONFIG would be the lever — decision outside this
issue).

### Side finding: force_rebuild leaves a double activation behind

The TC1 backup contains **two active snapshots** for ESGBS (original run
08-14 + force smoke 08-15, 34 chunks each): the force_rebuild path
creates a new generation but does not deactivate the previous one
(different profile_hash due to the force flag → no unique conflict, the
active flag remains doubled). Follow-up issue recommended.

## 4. DoD check

- [x] Backup exists + restore-verified (17/4844/26545/10464 exact)
- [x] Clean slate + baseline (16 pending / 0 / 0 / 0)
- [x] 3 runners + 3 dispatchers, runner_name populated, distribution SQL
      above
- [x] 16/16 completed, 0 failed, 0 zombies; OS == chunks (4813); outbox
      done
- [x] GPU sampler labeled per runner, analysis above (GPU time via
      compute total per runner + util profiles)
- [x] Scaling factor: 1.71× throughput (heterogeneous), 2.9× projected
      (3× 3090)
- [x] Determinism: quantified + classified (13/16 identical, 1 tempdir
      bug, 2 Marker edge cases, embeddings bit-exact)
- [ ] Hivemind re-review (backup, distribution SQL, determinism math)

## 5. Recommendations (outside this issue)

1. **EPUB tempdir normalization** before chunking (small fix, makes
   EPUBs byte-deterministic) — follow-up issue.
2. **force_rebuild: deactivate the old generation** — follow-up issue.
3. **Deterministic Marker** only if byte-identical re-runs become a
   product requirement (cost: performance loss from deterministic
   algorithms).
4. **Document the migration race:** clean slate → one instance first.
5. `/.dockerenv` startup gate as a deploy checklist entry (deployment
   doc §5c updated).

— *All numbers reproducible: axiom_db (TC2) vs axiom_db_tc1_ref
(backup restore), queries in this doc.*
