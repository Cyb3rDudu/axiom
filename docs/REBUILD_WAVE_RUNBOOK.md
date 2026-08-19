# W9 Rebuild-Wave Runbook — full-corpus re-chunk on the Carrier (#188)

**Status:** planning artifact (W9 preparation) · **Firing authority:** Hivemind + dudu, after W7 terminal states and the pre-flight checklist pass · **Branch:** `feat/w9-runbook`
**Scope:** the full active corpus — **121 documents as surveyed** (the Hivemind planning figure was 126; the firing SELECT is authoritative and the W7 outcome may shift the count) — `force_rebuild` wave, new chunker (W2 section-trail fix + W12 chapter ordinals + W4-ready locators) on carrier main @78558d5+.

Nothing in this document mutates production by itself. Every command that
changes state is marked **[MUTATES]** and belongs to the firing sequence.

---

## 1. Deploy plan — carrier runners on the new train

### 1.1 Current carrier state (surveyed 2026-08-18, read-only)

| Item | State | W9 consequence |
| --- | --- | --- |
| `~/Code/axiom` (carrier clone) | **stale @79913fc** (R3 era, #133) | fetch + checkout the merge-train SHA before anything else |
| `~/Code/runner-poc/` (deploy dir: `Containerfile` + baked `axiom_ng_runner/` copy) | chunker has **neither** `current_chunk_headings` (W2) **nor** `page_chapter_map` (W12) | refresh the baked copy from the new clone |
| image `localhost/runner-poc:latest` | built 18 h ago from the stale copy | rebuild, **tag by SHA** (`runner-poc:<short-sha>`) |
| `runner-carrier-gpu1` (Up 14 h, GPU1, :19543, 4.3 GB VRAM) | OLD code, serving the Mac dispatcher's ingest URL | keep until cutover; stop at firing time (see 3.3) |
| `runner-carrier`, `-2`, `-3` (Exited 41 h) | TC2-era trio | remove or ignore; names freed |
| `study-minirunner` (Exited 17 h) + `~/Code/axiom-study`, `~/models` | #171 research leftovers | not W9 business; teardown rides the #171 plan |
| GPUs | GPU0 3090 idle · GPU1 3090 (old runner) · GPU2 A3000 idle | wave uses all three after cutover |
| Disk | 898 G free on `/` | ample (models + work roots) |

### 1.2 Refresh + build (on the carrier)

```bash
ssh dudu@192.168.1.2
cd ~/Code/axiom
git fetch origin && git checkout main && git pull --ff-only
git log --oneline -1          # MUST be >= 78558d5 (merge train: W2+W3+W4+W12)

# refresh the deploy dir's baked source (path-preserving, exactly what the
# Containerfile COPYs):
rsync -a --delete ~/Code/axiom/axiom_ng_runner/ ~/Code/runner-poc/axiom_ng_runner/

cd ~/Code/runner-poc
podman build -t runner-poc:78558d5 -t runner-poc:latest .
```

The Containerfile pins the exact production stack (torch 2.13.0,
marker-pdf 1.10.2, FlagEmbedding 1.4.0, gliner 0.2.28, transformers 4.57.6,
pymupdf 1.28.2; pandoc 3.7.0.2 byte-matched to the Mac). Do not "improve"
pins during W9 — divergence between Mac fallback runner and carrier is a
determinism risk (#124 class).

### 1.3 Start the three wave runners (TC2-proven shape)

Per-runner: host network (measured 300× throughput vs passt port-mapping),
8G shm (BGE-M3 fp32 dies at the 64 MB rootless default), CDI all-GPU
injection + `CUDA_VISIBLE_DEVICES` pinning (torch `cuda:0` maps to the
pinned card), distinct ports.

```bash
for spec in "0 19542 w9-gpu0" "1 19543 w9-gpu1" "2 19544 w9-a3000"; do
  set -- $spec
  podman run -d --name runner-carrier-$3 \
    --network=host --shm-size=8g \
    --device nvidia.com/gpu=all \
    -e CUDA_VISIBLE_DEVICES=$1 \
    -e AXIOM_PROCESSOR_COMPUTE=real \
    -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
    -e AXIOM_PROCESSOR_PORT=$2 \
    -e AXIOM_PROCESSOR_WORK_ROOT=/work \
    -e AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/data \
    -e DEVICE_GLINER=cuda \
    runner-poc:78558d5
done
```

Notes:
- Port 19543 collides with the OLD `runner-carrier-gpu1` (still serving).
  Either stop the old container first (§3.3 cutover) or move w9-gpu1 to
  19545 until cutover — the dispatcher only switches at firing time anyway.
- `DEVICE_GLINER=cuda` is load-bearing (CPU GLiNER ≈ 1 h/book, measured).
- No Zotero mounts: production runners PULL sources from axiom-ng via the
  job's `source_url` (dispatcher-built from `AXIOM_PROCESSOR_SOURCE_BASE_URL`);
  downloads land in `work_root/.incoming` (`app.py`). `ALLOWED_SOURCE_ROOTS`
  only gates local-path delivery (reference mode) — same pattern the running
  GPU1 container proves.
- PID-file convention: containers are named, `podman ps` is the registry;
  Mac-side dispatcher processes keep `/tmp/axiom_runs/*.pid` per convention.

### 1.4 Isolation mechanism (confirmed by code, not assumption)

The runner is a **passive HTTP server** (`POST /v1/process`, `GET
/v1/jobs/{id}`, `/v1/health`, `/v1/capabilities`). It never pulls work.
All pulling lives in the Mac-side dispatcher: it polls `ingest_jobs`,
leases with SKIP LOCKED, and pushes to `AXIOM_PROCESSOR_URL`. Therefore a
started wave runner on a NEW port is **inert** until (a) pending jobs
exist AND (b) a dispatcher's `AXIOM_PROCESSOR_URL` points at it. Neither
holds before the firing sequence — that IS the isolation.

### 1.5 Sanctioned dry-run (once, safe)

```bash
# on the carrier, after §1.2/§1.3 (use 19542 — nothing points at it yet):
curl -s http://127.0.0.1:19542/v1/health          # {"status":"ok"}
curl -s http://127.0.0.1:19542/v1/capabilities | head -c 400

# NEW-CHUNKER PROOF (the honest version marker — __version__ is a static
# "0.1.0" and proves nothing): probe the baked source for the W2/W12
# symbols directly inside the container:
podman exec runner-carrier-w9-gpu0 python - <<'EOF'
import inspect, axiom_ng_runner.compute_core.chunker as c
import axiom_ng_runner.compute_core.page_trust as pt
src = inspect.getsource(c)
assert "current_chunk_headings" in src, "W2 section-trail fix missing"
assert "page_chapter_map" in src, "W12 chapter stamping missing"
assert "page_chapter_map" in inspect.getsource(pt), "W12 trust map missing"
print("new-chunker proof: W2+W12 symbols present")
EOF

nvidia-smi --query-gpu=index,memory.used --format=csv,noheader   # baseline
```

Then either leave the runners up (they idle at <100 MB VRAM until first
job) or `podman stop` them until firing. If stopped, re-run §1.3 at firing.

---

## 2. Wave runbook (the firing sequence)

### 2.1 Preconditions — see §4 checklist, all boxes green.

### 2.1a MANDATORY post-W7 projection sync **[MUTATES]**

W7 heals with `AXIOM_FIXSVC_NO_SYNC=1` (wave mode): the heal writes Zotero
(delete + create/upload) but skips the RAG sync. Until one sync runs, the
RAG DB still shows the OLD attachments as live with their ACTIVE old
snapshots — and the NEW healed attachments do not exist in the DB at all.
The §2.3 wave INSERT reads those RAG rows: fired without this sync it
would enqueue force_rebuild for the OLD, broken attachments (their
quarantined local_path files survive — quarantine copies, never moves)
and never chunk the healed bytes.

After W7 reaches terminal states (checklist 4.5) and BEFORE §2.3:

```bash
curl -s -X POST http://localhost:8011/api/zotero/sync | jq .   # ONE sync
```

The same-tx reconcile (retire-in-same-sync, #184) deactivates the old
snapshots and projects the new attachments — both sides of the stale
projection resolve in this one call. Then re-run the §2.3 dry SELECT and
verify the healed books appear under their NEW attachment keys (4.12).
Sync discipline resumes (quiesce) from that moment until the wave ends.

### 2.2 Dispatcher topology (TC2-proven 3-dispatcher pattern)

One dispatcher per runner (Mac side), so `ingest_jobs.runner_name` cleanly
attributes books and SKIP LOCKED + claim fencing keep the split exclusive
(work-conserving: fast cards simply take more). Per dispatcher:

```text
AXIOM_DISPATCHER_ENABLED=true
AXIOM_PROCESSOR_URL=http://192.168.1.2:<port>     # 19542 / 19543 / 19544
AXIOM_PROCESSOR_RUNNER_NAME=carrier-w9-gpu0|w9-gpu1|w9-a3000
AXIOM_DISPATCHER_WORKER_ID=axiom-ng-w9-<n>
AXIOM_DISPATCHER_CONCURRENCY=1                    # per-GPU serial, TC2 shape
AXIOM_DISPATCHER_LEASE=5m                         # default, proven
AXIOM_DISPATCHER_PROFILE=full-rag-v1              # see profile note below
# Source-pull path (review W1 — without these the runner gets Mac local_paths,
# rejects them (not under /data) and every job dies 422 SOURCE_NOT_FOUND):
AXIOM_PROCESSOR_SOURCE_BASE_URL=http://192.168.1.47:8011   # live production value
AXIOM_PROCESSOR_SOURCE_SECRET=<from the running axiom-ng env — never in docs>
# Standalone dispatcher processes also need (no defaults):
AXIOM_DATABASE_URL=<Mac Postgres DSN>
AXIOM_ARTIFACT_ROOT=<artifact dir — full-rag-v1 extracts images; validation fails without it>
```

Profile note (Flutgate lesson, MASS_CHUNKING_BENCHMARK §Profile Finding):
the Zotero-sync default profile materialized all feature booleans as
`false` — 0 embeddings, 0 entities on the first library run. The profile
lives dispatcher-side (`AXIOM_DISPATCHER_PROFILE`) and is attached to
every claim — there is no per-job profile column. Verify the staged
dispatcher config resolves to the full-feature profile, and after the
first completed book check the snapshot's `profile_hash` + the presence
of fresh dense embeddings (checklist §4.8).

### 2.3 Firing — enqueue the force_rebuild wave **[MUTATES]**

There is no production API for force jobs (by design: sync enqueues with
`force_rebuild=false` and the partial unique index dedups). The wave
insert is direct SQL on the Mac DB — pattern proven by the dispatcher ITs:

```sql
-- EXPECT exactly the active-book count (~126; verify with the SELECT first)
-- Columns per schema 0001: the profile is NOT a job column — the dispatcher
-- attaches it to every /v1/process request at claim time
-- (AXIOM_DISPATCHER_PROFILE), so the wave inherits whatever the dispatchers
-- are configured with. Checklist §4.8 pins that config.
INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash,
                         status, force_rebuild)
SELECT a.source_id, a.document_id, a.id, a.content_hash, 'pending', true
FROM zotero_attachments a
WHERE NOT a.deleted
  AND EXISTS (SELECT 1 FROM processing_snapshots s
              WHERE s.attachment_id = a.id AND s.active);
```

Run the SELECT alone first, eyeball the list against the W7 outcome (every
healed attachment present, every blocked book consciously included or
excluded per Hivemind decision), then INSERT inside a transaction with the
count asserted.

### 2.4 Throughput estimate (from TC2 / L8 / Flutgate measurements)

| Basis | Number |
| --- | --- |
| TC2 measured, 3 heterogeneous GPUs, 16 books | 56 min → **3.5 min/book wall** |
| Compute-bound floor (Σ per-GPU rates: 2× 6 min + 1× 17.7 min per book on 3090s/A3000) | ~2.6 min/book |
| **126 books, expected wall** | **~5.5–8 h** (3.5 min/book ≈ 7.4 h; A3000 straggler tail dominates the band's upper end) |
| Cold start | first ~2 jobs +3–5 min (model load + Triton JIT) |
| VRAM per runner (POC reference) | ~2.8 GB (Marker+GLiNER+mREBEL) — fits all three cards trivially |

W2/W12 add pure-Python work per chunk (trail snapshot, phys tracking) —
measured noise vs the GPU stages (mREBEL ~104 s/book dominates).

### 2.5 Checkpoint / restart semantics

- **Crash of a runner:** its in-flight leases expire after 5 m and re-lease
  to another runner; `attempt` counts up against `max_attempts`. Restart =
  `podman restart runner-carrier-<n>` (or §1.3 again). No manual requeue.
- **Idempotency:** snapshot identity (attachment, hash, processor,
  version, profile) + force generation — a re-run either dedups onto the
  new snapshot or creates the next generation; chunks/embeddings/entities
  persist atomically with the active-flag flip (§10.2, Gate 4).
- **OS parity:** guaranteed even for force_rebuild via tombstones (#127,
  L8-verified): old OS docs die with the superseded snapshot, outbox
  replays the new chunks. Expect `OS docs == active chunks` at rest.
- **Mid-wave abort:** stop dispatchers (not runners) — the queue simply
  stops draining; completed books stay valid. Resume = restart dispatchers.

### 2.6 Monitoring one-liners (Mac DB unless noted)

```sql
-- progress by state and card (attribution via runner_name):
SELECT status, coalesce(runner_name,'—') AS card, count(*) FROM ingest_jobs
WHERE force_rebuild GROUP BY 1,2 ORDER BY 1;

-- live book-level stage visibility (runner-side job store — the runner
-- reports per-stage progress on GET /v1/jobs/{id}, L8 §2; there is no
-- phases column in ingest_jobs):
ssh dudu@192.168.1.2 'for p in 19542 19543 19544; do
  curl -s http://127.0.0.1:$p/v1/jobs | head -c 300; echo; done'
```

```bash
# OpenSearch == Postgres parity (the W10 census prelude; 0 while draining,
# equal counts at rest):
OS=$(curl -s localhost:9200/axiom-ng-chunks-v1/_count | jq .count)
PG=$(psql axiom_db -tAc "SELECT count(*) FROM processing_chunks c
     JOIN processing_snapshots s ON s.id=c.snapshot_id WHERE s.active")
echo "OS=$OS PG=$PG"

# carrier GPUs (straggler watch — A3000 was the TC2 critical path):
watch -n 30 'ssh dudu@192.168.1.2 nvidia-smi --query-gpu=index,utilization.gpu,memory.used --format=csv,noheader'

# runner logs (one per card):
ssh dudu@192.168.1.2 'podman logs -f --tail 50 runner-carrier-w9-gpu0'
```

### 2.7 Expected end-state (verification targets for W10)

- `121/121 completed` (per surveyed count; re-check at firing), 0 failed (attempt=1 for all but genuine straggler re-leases).
- Active snapshots: one per attachment, new generation; old snapshots immutable.
- Chunks ≈ 35.3 k ± Marker edge nondeterminism (L8: 2/16-class layout drift).
- **#186 census: normalized fencepost 3,631 → 0; strict 18,045 → 0** (queries
  documented in issue #186).
- Chapter-relative healed books: locators carry `chapter`, folio_verified
  per chapter (W12); `page_span` physical anchors from Marker truth (C1 fix).
- OS docs == active chunks; embeddings bit-exact across cards (L8 evidence).
- KG: entity/relation counts re-extracted; #185 corroboration ranking live.

### 2.7a Precision-wave generation (SHA discipline)

The precision wave runs ONE consistent generation: carrier image, Mac
server and all three dispatchers baked from the SAME main SHA. Current
wave SHA: **e7344db** (v2.1 extractor + #194 chunker/paragraph_pages +
Go epilogue + citation offset parsing). Symbol probes at startup:
page_trust `_ELI_BARE`/`BLIND` (v2.1), chunker `paragraph_pages`,
`axiom-ng -consolidate-entities` mode present. Never fire on mixed
builds — the first wave proved silent version skew is real.

### 2.8 Standard epilogue — entity consolidation (#193)

After the drain and the OS==PG parity check, every wave runs the
generation-time entity consolidation (owner decision: merging, no
migration, no read-layer workaround; exact canonical-form match only):

```bash
AXIOM_DATABASE_URL=<dsn> ./axiom-ng -consolidate-entities
# epilogue: entity consolidation complete: N entities merged
```

Idempotent by construction; pinned by the seeded IT TestIT_ConsolidateEntities.
Re-run after any post-epilogue straggler persist — a late book re-creates
its per-doc duplicate entities by design (drains lie; §2.5). Moved
mentions keep counting toward the survivor even if their source snapshot
later turns inactive — generation-time merging keeps dead-snapshot
evidence until retention removes the old snapshots (owner decision).

---

## 3. Cutover + rollback

### 3.1 Cutover order at firing time **[MUTATES]**
1. Preflight §4 all green.
2. Stop old `runner-carrier-gpu1` (pre-train code; frees GPU1 + 19543):
   `ssh dudu@192.168.1.2 podman stop runner-carrier-gpu1`.
3. Start/confirm the three w9 runners (§1.3).
4. Start the three dispatchers (Mac, §2.2, PID files per convention).
5. Enqueue the wave (§2.3) with count assertion.

### 3.2 Rollback honesty
Snapshots are immutable and the active flag flips per attachment at
completion — there is no in-place "un-wave". Rollback = another force wave
from the old image (rollback-of-last-resort, needs a Hivemind decision).
Cheap safety instead: mid-wave abort (§2.5) leaves completed books better
and pending books untouched.

### 3.3 Post-wave carrier hygiene
Stop w9 runners or leave as the new standing ingest fleet (dispatcher
topology then collapses back to one primary URL + Mac fallback per
EXTERNAL_RUNNER_DEPLOYMENT §failover). Remove TC2-era exited containers.
#171 leftovers (study-minirunner, axiom-study, ~/models) ride the #171
teardown plan.

---

## 4. Pre-flight checklist (all verifiable before firing)

| # | Check | Command | Expected |
| --- | --- | --- | --- |
| 4.0 | Post-W7 projection sync done | §2.1a executed once; healed books show NEW attachment keys | old snapshots retired; new attachments active-eligible |
| 4.0b | Stale-attachment re-point (satellite finding) | script asserts: DUJQJ2RN/NU8SS6HG deleted + no active snapshot; DNC73IVL/PC9U5YEX present, not deleted | both old keys retired, both new keys projected (live pre-fix state: exactly the violation) |
| 4.1 | Merge train on carrier clone | `ssh … 'cd ~/Code/axiom && git log --oneline -1'` | ≥ `78558d5` |
| 4.2 | New-chunker proof in image | §1.5 container feature probe | both W2+W12 assertions pass |
| 4.3 | Runners healthy | `curl :19542/19543(45)/19544 /v1/health` ×3 | `{"status":"ok"}` |
| 4.4 | Queue state | `SELECT status, count(*) FROM ingest_jobs WHERE status IN ('pending','processing') GROUP BY 1` | EITHER `0`, OR exactly the known heal-projections (8 pending on new content hashes, live-verified 2026-08-18: 0 leases, held since 09:15) — held for the wave; anything `processing` = a dispatcher is draining on OLD code: STOP it before cutover. The §2.3 wave INSERT force-rows make the 8 pendings redundant (same attachments, force generation) — resolve them via the wave, not a pre-wave drain |
| 4.5 | W7 terminal | `SELECT status, count(*) FROM repair_cases GROUP BY 1` | no `queued`/`in_repair`; healed+blocked+closed only. W7 ran with `AXIOM_FIXSVC_NO_SYNC` set (truthy check — any non-empty value incl. `0` enables, same pattern as `AXIOM_FIXSVC_DUMP_HEALED`) |
| 4.6 | Zotero quiesce | no sync scheduled; sync API idle | no new `zotero_*` writes during wave (manual discipline + checklist at firing) |
| 4.7 | OS cluster health | `curl localhost:9200/_cluster/health` | `green`, no unassigned shards |
| 4.8 | Full-feature profile | staged `AXIOM_DISPATCHER_PROFILE` env resolves to dense+entities+relations; after first book: `SELECT profile_hash FROM processing_snapshots ORDER BY created_at DESC LIMIT 1` + dense-embeddings row count > 0 | full-feature hash; embeddings grow |
| 4.9 | Mac DB disk headroom | `df -h` on the Postgres volume | ≥ 2× current DB size free (new generation doubles chunk tables transiently) |
| 4.10 | Carrier disk | `ssh … df -h /` | ≥ 898 G free (surveyed) |
| 4.11 | VRAM baseline | §1.5 nvidia-smi | GPU0/2 ≈ idle; GPU1 freed after cutover |
| 4.12 | Expected book list | §2.3 dry SELECT (surveyed 2026-08-18: **121**) | matches W7 outcome; any delta explained |
| 4.13 | Dispatcher dry config | §2.2 env staged, not started | 3 configs reviewed |

---


## 5. Sources

- `axiom_ng/docs/benchmarks/MASS_CHUNKING_BENCHMARK.md` — Flutgate-class
  reference run (16 docs, serial, 20.9 docs/h, profile-finding, shm/network traps).
- `axiom_ng/docs/benchmarks/L8_DURCHSTICHS_ANALYSE.md` — TC2 3-GPU parallel
  proof (1.71×, work-conserving, straggler tail, determinism).
- `axiom_ng/docs/EXTERNAL_RUNNER_DEPLOYMENT.md` — container recipe, network/shm
  rationale, CDI pinning, failover topology.
- Issues #186 (chunker fix + census), #185 (KG), #188 (wave decisions D1/D2,
  W1–W12 sequencing); merge train main @78558d5.
- Carrier survey 2026-08-18 (read-only): containers, clones, image age,
  GPU state — §1.1.
