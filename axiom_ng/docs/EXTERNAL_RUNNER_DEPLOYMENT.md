# External Runner Deployment (GPU Compute Offload)

How to run the `axiom_ng_runner` (Python processor) on a remote GPU host and
point the local axiom-ng dispatcher at it, so heavy document processing
(Marker conversion, BGE-M3 embeddings, GLiNER entities, mREBEL relationships)
runs on NVIDIA GPUs instead of the local machine.

This is the pattern proven in the Carrier POC (2026-08, 2× RTX 3090 + 1×
A3000, rootless Podman + CDI): a 3-page PDF that takes 110–160 s on Apple MPS
runs in **~30 s warm-cache** on a 3090 (~4–5× faster).

## Architecture

```
Mac (axiom-ng)                        Remote GPU host (runner)
┌─────────────────────┐   HTTP/JSON   ┌──────────────────────────┐
│ Go dispatcher        │ ───────────▶ │ axiom_ng_runner (Python)  │
│ POST /v1/process     │  port 8012   │ Marker + BGE-M3 + GLiNER │
│ polls status/result  │ ◀─────────── │ + mREBEL (all in-container)│
│ persists to Postgres │              └──────────────────────────┘
└─────────────────────┘
```

The runner is **pure compute** — it never touches Postgres, OpenSearch, or
Zotero. All durable state stays on the axiom-ng side. Only the HTTP contract
(`PROCESSOR_CONTRACT.md`) crosses the wire.

## Prerequisites on the GPU host

- NVIDIA GPUs + driver (verify: `nvidia-smi`)
- Podman (rootless is fine)
- NVIDIA CDI integration: `/var/run/cdi/nvidia-container-toolkit.json` must
  exist. If another GPU container (e.g. llama-swap) already runs on the host,
  CDI is set up — reuse that as the reference.

## 1. Ship the code

The runner is self-contained since the compute_core vendor move (#118):
`axiom_ng_runner/` carries its own compute cores (chunker, embedder,
entity/relation extractors, Marker path, workers) under
`axiom_ng_runner/compute_core/`. One tree to ship:

```bash
ssh <user>@<gpu-host> "mkdir -p ~/Code/runner-poc/axiom_ng_runner"
rsync -av --exclude='.venv' --exclude='__pycache__' --exclude='.pytest_cache' \
  <repo>/axiom_ng_runner/ <user>@<gpu-host>:~/Code/runner-poc/axiom_ng_runner/
```

## 2. Containerfile

Key points learned from the POC (all four build iterations, plus the
vendor-move simplification):

1. **Self-contained runner.** Since #118 the image copies ONLY
   `axiom_ng_runner/` (compute_core included). No `ai_researcher/`, no
   `database/` stub, no `psycopg2-binary` — the DB-driver import chain
   stayed behind with the old tree.
2. **Triton JIT needs gcc + libc6-dev** (`crti.o`). Do not use
   `--no-install-recommends` without adding these explicitly, or the first
   dense-embedding run dies.
3. **Pin versions identical to the reference venv**
   (`axiom_ng_runner/requirements-heavy.txt`), above all
   `marker-pdf==1.10.2`. Divergent versions produce divergent output.
4. **`RUN touch /.dockerenv`** — see the trap section below.

```dockerfile
FROM python:3.11-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libc6-dev pandoc libglib2.0-0 libgl1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir -r requirements-heavy.txt
COPY axiom_ng_runner/ /app/axiom_ng_runner/
WORKDIR /app
ENV PYTHONPATH=/app
EXPOSE 8012
RUN touch /.dockerenv
CMD ["python", "-m", "axiom_ng_runner"]
```

## 3. Build and run with GPU

**Use `--network=host` — this is the critical setting.** The measured
breakthrough (2026-08-15, L8 mass-chunking):

| Path | Result-fetch throughput (17 MB result) |
| --- | --- |
| Port-mapped (`-p 19542:8012`, rootless Podman via **passt**) | **~123 KB/s** — bulk collapses while loopback inside the container serves 122 MB/s |
| **`--network=host`** | **full LAN speed (~40 MB/s)** — result transfers ~0.5 s instead of 2+ min |

Rootless Podman's userspace port forwarder (passt) exhibits the same
small-packets-fine/bulk-collapses signature as a bad VPN tunnel. Polls and
health checks look healthy (ms latency) while multi-MB result JSONs and
artifact bodies crawl. If you see "GPU idle + no errors + jobs stuck in
post-compute", check the serving path first — loopback-fast ≠
mapped-port-fast.

```bash
podman build -t runner-poc ~/Code/runner-poc/

# Host network + CDI device injection — mounts driver libs automatically,
# no manual /dev/nvidia* device mapping needed. No -p mapping required:
# the runner binds the host port directly.
podman run -d --name runner-poc \
  --network=host \
  --device nvidia.com/gpu=all \
  -e AXIOM_PROCESSOR_COMPUTE=real \
  -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
  -e AXIOM_PROCESSOR_PORT=19542 \
  -e AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/nonexistent \
  -e DEVICE_GLINER=cuda \
  localhost/runner-poc
```

`--device nvidia.com/gpu=all` exposes all host GPUs. To pin to one GPU, use
the CDI device name for that index instead of `all` (see
`/var/run/cdi/nvidia-container-toolkit.json` for available names). For
per-GPU parallel runners (one runner per GPU), set
`CUDA_VISIBLE_DEVICES=<n>` per container — torch's `cuda:0` then maps to
that physical GPU (see the L8 parallel test case).

**Port choice:** pick a dedicated high port (e.g. 19542) and open it in the
host firewall (NixOS: `networking.firewall.allowedTCPPorts`). The dispatcher
points at `http://<host-ip>:<port>`. Both directions must be directly
reachable — source downloads (runner pulls from axiom-ng) and
result/artifact fetches (axiom-ng pulls from runner) are both multi-MB bulk
flows that need real LAN throughput.

**GLiNER device:** `DEVICE_GLINER=cuda` must be set explicitly — the default
(in `compute_core/devices.py`, preserved from the old config) is `cpu` and a
CPU GLiNER eats ~1 h per book
that takes ~5 min on GPU (measured: 12/14 cores saturated, 3 jobs died of
result-fetch timeouts under the load).

**Bind address:** `0.0.0.0` is required for remote access. Only do this on
LAN-only hosts — the runner has no authentication (by design, work order
§18: loopback or trusted network only).

## 4. Verify

```bash
# CUDA inside the container:
podman exec runner-poc python -c \
  "import torch; print('cuda:', torch.cuda.is_available(), '| devices:', torch.cuda.device_count())"

# GLiNER loads and predicts:
podman exec runner-poc python -c "
from gliner import GLiNER
m = GLiNER.from_pretrained('urchade/gliner_multi-v2.1')
print(m.predict_entities('Steve Jobs founded Apple.', ['PERSON']))"

# Health endpoint:
curl http://<gpu-host>:19542/v1/health
curl http://<gpu-host>:19542/v1/capabilities
```

First run downloads ~3 GB of model weights (Marker surya + GLiNER + mREBEL);
subsequent runs are warm-cache.

## Transport rule (learned the hard way, 2026-08-15)

Three layered transport traps were debugged during L8, each masking the next:

1. **Serial artifact fetches** (fixed in code: bounded-parallel staging, per-call timeouts) — hundreds of round-trips per book.
2. **Tailscale utun10 bulk collapse** (~35-83 KB/s on multi-MB flows, ms-latency on small packets, despite "direct" connection) — see issue #121 for the full analysis. Tailscale stays fine for control-plane (health, source uploads of small files) but is NOT for bulk.
3. **Rootless Podman passt port-mapping** — the same collapse signature ON THE CONTAINER LAYER: loopback inside the container served 122 MB/s while the `-p`-mapped port crawled at ~123 KB/s. Fixed by `--network=host`.

**Operating rule:** dispatcher↔runner bulk flows (result JSON, artifact
bodies) require **direct LAN reachability on both directions** —
`AXIOM_PROCESSOR_URL` to the runner's host port, and the runner's
`source_url` base pointing at axiom-ng's LAN address. Tailscale works for
control-plane and is the fallback when no direct path exists (accept the
throughput penalty or wait for #121 resolution). Symptom signature of a
transport trap: GPU idle, no dispatcher errors, jobs stuck between
compute-done and persisted for minutes — check the serving path (loopback
vs mapped port vs tunnel) before blaming compute.

## 5. Wire the dispatcher to it

On the axiom-ng host (Mac):

```bash
export AXIOMNG_DISPATCHER_ENABLED=true
export AXIOM_PROCESSOR_URL=http://<gpu-host>:19542   # direct LAN — see transport note
```

The dispatcher performs capability negotiation against the remote runner on
startup and will fail fast if it is unreachable or contract-incompatible.
Test with one small document before batch processing.

## 5b. Source delivery over source_url (no shared Zotero mount)

The remote runner does NOT need access to the Zotero storage. The dispatcher
attaches an HMAC-signed download URL (`attachment.source_url`) to every
process request and the runner pulls the bytes over HTTP (contract §3
additive v1 extension). Configure on the axiom-ng host:

```bash
# Shared HMAC secret (dispatcher signs, /api/processor/source verifies).
# Empty = feature off on BOTH sides.
export AXIOMNG_PROCESSOR_SOURCE_SECRET='<random-hex>'
# The base URL the runner can use to reach axiom-ng — the Tailnet address,
# NOT 127.0.0.1 (the runner resolves it on its own host):
export AXIOMNG_PROCESSOR_SOURCE_BASE_URL=http://<mac-tailnet-ip>:8011
# axiom-ng must listen on an interface the runner can reach:
export AXIOMNG_BIND_ADDR=0.0.0.0
# POST /v1/process waits for the synchronous download; the dispatcher's result
# budget also floors the submit call to cover it (default 300s):
export AXIOMNG_PROCESSOR_TIMEOUT=180s
```

Runner side: keep `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` unset or pointing
at a nonexistent path — local delivery is then impossible by construction
and every source arrives via the signed URL. The URL expires with the job's
lease; downloaded bytes run the same hash gate as local files and die with
the ACK (contract §18/§19-13).

> The earlier rsync-bridge / sshfs-mount experiments are superseded by this
> mechanism — do not stage Zotero copies on the GPU host (§4.15).

Smoke-verified 2026-08-14 (Carrier): 163 kB EPUB end-to-end over source_url
with zero Zotero access on the runner — completed, 34 chunks, outbox done,
OpenSearch indexed, work dir (incl. the downloaded source) empty after ACK.

## 5b-bis. The /.dockerenv trap (TC2 round-1 lesson, 2026-08-15)

Rootless Podman fails `is_running_in_docker()` checks: no `/.dockerenv`,
cgroup v2 shows only `0::/`. The old vendored config then treated the
container as bare metal and OVERWRITES `CUDA_VISIBLE_DEVICES=0` at import —
trampling any per-container GPU pinning. Symptom: every runner stacks on GPU
0 (VRAM pileup + Marker OOM) while pinned GPUs stay empty.

Fix (in the Containerfile, durable): `RUN touch /.dockerenv`.

Start gate before any parallel run (30 s, would have saved round 1):
per container `python -c "import torch; print(torch.cuda.device_count(),
torch.cuda.get_device_name(0))"` must show exactly ONE device with the
expected name, plus a host-side test allocation must light up VRAM on EVERY
pinned card simultaneously.

## 5c. Runner identity + GPU sampler labels (TC2 parallel operation)

With multiple runners (TC2), every log line and every job row must say WHICH
runner produced it. Two pieces (issue #122):

```bash
# Dispatcher side: name each runner explicitly (empty = processor URL host,
# e.g. "192.168.1.2:19542" — usable, but a name is readable).
export AXIOMNG_PROCESSOR_RUNNER_NAME=carrier-gpu0
```

This lands in the phases log line (`phases[ok]: runner=carrier-gpu0 job=…`)
and in `ingest_jobs.runner_name` at claim time — the TC2 scale proof
(throughput/distribution per runner) is then pure SQL:
`SELECT runner_name, count(*), avg(completed_at-started_at) FROM ingest_jobs
WHERE status='completed' GROUP BY 1;`. The column is deliberately NOT
`processor_name` (that one holds the processor software identity written at
completion and must not be clobbered).

GPU sampler with the runner label — one sampler per runner container, label
first so lines stay attributable after merge:

```bash
nohup sh -c 'while true; do echo "carrier-gpu0 $(date +%s) $(nvidia-smi --query-gpu=index,memory.used,utilization.gpu --format=csv,noheader)"; sleep 30; done' \
  > /tmp/gpu_sampler_carrier-gpu0.log 2>&1 &
```

With `CUDA_VISIBLE_DEVICES` pinning per container, GPU index + label identify
the runner unambiguously.

## Runtime knobs (runner env vars)

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_PROCESSOR_BIND_ADDR` | `127.0.0.1` | `0.0.0.0` for remote access |
| `AXIOM_PROCESSOR_PORT` | `8537` | HTTP port (19542 with host-network on carrier) |
| `AXIOM_PROCESSOR_COMPUTE` | `reference` | `real` for GPU pipeline |
| `AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS` | `1` | Marker+models are VRAM-heavy; keep 1 per GPU |
| `AXIOM_PROCESSOR_WORK_ROOT` | `/tmp/axiom_processor_work` | Temp job state |
| `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` | — | Host paths the runner may read |
| `AXIOM_PROCESSOR_RESULT_RETENTION` | `3600` | Seconds before unacked results expire |

## Mass-chunking with the remote runner

1. Keep dispatcher `Concurrency=1` (the runner enforces
   `MAX_CONCURRENT_JOBS=1` anyway; parallel jobs would contend for VRAM).
2. Expect ~30 s per small document warm-cache on a 3090; large scanned books
   scale with OCR load (the Dubs Euler Rueegg OCR stress test is the upper
   bound).
3. The runner holds results until ACK; axiom-ng's ack-retry pass recovers if
   the dispatcher restarts mid-batch.

## Known gaps (as of this writing)

- `capabilities.models.dense_embedding.name` reports `reference-bge-m3`
  even in real mode (cosmetic; the vectors themselves are genuine 1024-dim
  BGE-M3). Deferred review suggestion.
- The runner loads all three model families per process; VRAM footprint is
  ~2.8 GB — trivially fits a 12 GB GPU, leaves room on 24 GB cards for a
  second pinned runner if you ever want per-GPU parallelism.

---

*POC reference: Carrier (`dudu@192.168.1.2`), rootless Podman 5.8.4, CDI
device injection, verified 2026-08-14. Container `runner-poc`, port 8012.*
