# Deployment: Running a Runner (GPU Compute Offload)

This chapter explains how to run the `axiom_ng_runner` (the Python processor)
on an external GPU host and how to point the `axiom_ng` dispatcher at it. The
purpose: run heavy document processing (Marker conversion, BGE-M3 embeddings,
GLiNER entities, mREBEL relationships) on NVIDIA GPUs instead of on the
dispatcher host.

> **Universal by design:** concrete hostnames, IPs and usernames are replaced
> with placeholders such as `<runner-host>`, `<port>`, or generic descriptions.
> The patterns shown are **requirements and operating rules** — they come from
> measurements but are expressible independent of any particular machine.
> Example ports (e.g. `19542`) are illustrative, not prescriptive.

## Architecture

```text
Dispatcher host (axiom)                Runner host (GPU)
┌──────────────────────────┐  HTTP/JSON ┌──────────────────────────────┐
│ Go dispatcher             │ ──────────▶ │ axiom_ng_runner (Python)      │
│ POST /v1/process          │   Port     │ conversion + embeddings +    │
│ polls status/result       │ ◀───────── │ entity/relation extraction    │
│ persists to Postgres      │            └──────────────────────────────┘
└──────────────────────────┘
```

The runner is **pure compute**: it never touches Postgres, OpenSearch, or
Zotero. All durable state stays with the dispatcher. Only the HTTP contract
(`PROCESSOR_CONTRACT v1`) crosses the wire.

## Prerequisites on the GPU host

- NVIDIA GPU(s) + driver (verify with `nvidia-smi`)
- Podman (rootless works) or an equivalent OCI container runner
- NVIDIA CDI integration: the CDI spec
  (`/var/run/cdi/nvidia-container-toolkit.json`) must be present. If a GPU
  container already runs on the host, CDI is usually set up — reuse that
  configuration as a reference.

## 1. Ship the code to the runner host

The runner is self-contained (the vendored `compute_core` directory is inside
`axiom_ng_runner/`): a single directory tree has to be transferred to the
runner host:

```bash
rsync -av --exclude='.venv' --exclude='__pycache__' --exclude='.pytest_cache' \
  axiom_ng_runner/ <user>@<runner-host>:<path>/axiom_ng_runner/
```

## 2. Containerfile

Build checks (derived from operating experience, stated as requirements):

1. **Self-contained runner:** The image copies **only** `axiom_ng_runner/`
   (including `compute_core`). No DB adapter, no legacy module — the DB-driver
   import chain is no longer in the runner since the compute-core vendor split
   (#118).
2. **Triton JIT needs a compiler + libc:** install `gcc` and `libc6-dev`
   explicitly (otherwise the first dense-embedding run fails on a missing
   `crti.o`). Do not use `--no-install-recommends` without these packages.
3. **Pin versions identical to the reference venv** (see
   `axiom_ng_runner/requirements-heavy.txt`), above all
   `marker-pdf==1.10.2`. Divergent versions produce divergent output.
4. **`RUN touch /.dockerenv`** — see Trap 10 in the
   [L8 Throughput Analysis](../references/benchmarks/l8-durchstich.md).

```dockerfile
FROM python:3.11-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libc6-dev pandoc libglib2.0-0 libgl1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir -r requirements-heavy.txt
COPY axiom_ng_runner/ /app/axiom_ng_runner/
WORKDIR /app
ENV PYTHONPATH=/app
EXPOSE <port>
RUN touch /.dockerenv
CMD ["python", "-m", "axiom_ng_runner"]
```

## 3. Build and run with GPU

**Use `--network=host` — this is the critical setting.** A port forward
(e.g. `-p <mapped>:<port>`) through the rootless-Podman userspace port
forwarder shows the same failure signature as a slow tunnel: small packets
(polls, health checks) respond in milliseconds while multi-MB result JSONs and
artifact bodies crawl. A loopback test **inside** the container measures only
the internal fast path, not the mapped path. Symptom of a transport trap: GPU
idle, no dispatcher errors, jobs stuck "after compute done". Then check the
serving path first, not the compute.

```bash
podman build -t runner <path>/

# Host network + CDI device injection — binds the host port directly; no -p mapping needed.
podman run -d --name runner \
  --network=host \
  --device nvidia.com/gpu=all \
  -e AXIOM_PROCESSOR_COMPUTE=real \
  -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
  -e AXIOM_PROCESSOR_PORT=<port> \
  -e AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/nonexistent \
  -e DEVICE_GLINER=cuda \
  localhost/runner
```

- `--device nvidia.com/gpu=all` exposes all host GPUs. To pin to one GPU use
  the CDI device name of the desired index card instead of `all` (names are in
  `/var/run/cdi/nvidia-container-toolkit.json`).
- For parallel one-GPU runners (one runner per card) set `CUDA_VISIBLE_DEVICES=<n>`
  per container — `cuda:0` in PyTorch then maps to that physical GPU.
- **Port choice:** pick a dedicated high port and open it in the host firewall.
  Both directions must be directly reachable: source download (runner pulls
  from the dispatcher) and result/artifact fetch (dispatcher pulls from the
  runner) are both MB-sized bulk flows that need real LAN throughput.
- **GLiNER device:** `DEVICE_GLINER=cuda` must be set explicitly. The default is
  `cpu` — a CPU GLiNER costs ~1 hour per book instead of ~5 minutes on GPU.

**Per-model device knobs** (source of truth: `compute_core/devices.py`,
`_MODEL_DEVICE_ENV`):

| Env var | Model | Default |
| --- | --- | --- |
| `DEVICE_EMBEDDER` | BGE-M3 | `auto` |
| `DEVICE_MARKER` | Marker | `auto` |
| `DEVICE_MREBEL` | mREBEL | `auto` |
| `DEVICE_GLINER` | GLiNER | `cpu` |

**Bind address:** `0.0.0.0` is required for remote access. Only do this on
LAN-only hosts — the runner deliberately has no authentication (it runs by
design only on loopback or a trusted network, see contract §18).

### Alternative: run on the dispatcher host (Apple MPS)

A GPU run is not necessarily external. On an Apple Mac with MPS the complete
`real` pipeline can run (validation #128):

```bash
DEVICE_GLINER=mps PYTORCH_ENABLE_MPS_FALLBACK=1 \
  AXIOM_PROCESSOR_COMPUTE=real .venv/bin/python -m axiom_ng_runner
```

- Device resolution needs no env for marker/embedder/mREBEL (`auto` → mps);
  GLiNER wants explicit `DEVICE_GLINER=mps`.
- Known MPS limitation: surya's table recognition
  (`TableRecEncoderDecoderModel`) is not MPS-compatible and falls back to CPU
  with a warning — table-heavy PDFs pay extra.
- MPS is **complete but slow** (measured ~13 s/page vs. ~0.7–1.2 s/page on an
  RTX-3090-class card). For production mass processing use external GPUs.

## 4. Verify

```bash
# CUDA inside the container:
podman exec runner python -c \
  "import torch; print('cuda:', torch.cuda.is_available(), '| devices:', torch.cuda.device_count())"

# GLiNER loads and predicts:
podman exec runner python -c "
from gliner import GLiNER
m = GLiNER.from_pretrained('urchade/gliner_multi-v2.1')
print(m.predict_entities('Steve Jobs founded Apple.', ['PERSON']))"

# Health/endpoints:
curl http://<runner-host>:<port>/v1/health
curl http://<runner-host>:<port>/v1/capabilities
```

The first run downloads ~3 GB of model weights (Marker/surya + GLiNER + mREBEL);
later runs are warm-cache.

## Transport rule (measured lesson)

Two transport layers in sequence masked one problem during a mass run:

1. **Tunnel bulk collapse** on the control path (ms latency on small packets,
   but ~35–83 KB/s on MB flows despite a "direct" connection).
2. **Userspace port forwarding** in rootless Podman — the same collapse
   signature at the container layer (`--network=host` fixes it).

**Operating rule:** dispatcher↔runner bulk flows (result JSON, artifact bodies)
require **direct LAN reachability in both directions** — `AXIOM_PROCESSOR_URL`
to the runner host port, and the runner's `source_url` base pointing at the
dispatcher's LAN address. A tunnel works for the control plane and is the
fallback when no direct path exists (accepting the throughput penalty).
**Symptom signature of a transport trap:** GPU idle, no dispatcher errors, jobs
hung for minutes between compute-done and persisted — check the serving path
(loopback vs. mapped port vs. tunnel) before blaming compute.

## 5. Wire up the dispatcher

On the dispatcher host:

```bash
export AXIOM_DISPATCHER_ENABLED=true
export AXIOM_PROCESSOR_URL=http://<runner-host>:<port>   # direct LAN — see transport rule
```

The dispatcher negotiates capabilities against the remote runner at startup and
fails fast if it is unreachable or contract-incompatible. Test with one small
document before batch processing.

### Source delivery over `source_url` (no shared Zotero mount)

The remote runner does **not** need access to the Zotero storage. The dispatcher
attaches an HMAC-signed download URL (`attachment.source_url`) to every process
request; the runner pulls the bytes over HTTP (contract §3, additive v1
extension). Configure on the dispatcher side:

```bash
# Shared HMAC secret (dispatcher signs, .../source verifies). Empty = feature off on both sides.
export AXIOM_PROCESSOR_SOURCE_SECRET='<random-hex>'
# Base URL the runner uses to reach the dispatcher — NOT 127.0.0.1 (the runner resolves on its own host):
export AXIOM_PROCESSOR_SOURCE_BASE_URL=http://<dispatcher-lan-ip>:<dispatcher-port>
# The dispatcher must listen on a reachable interface:
export AXIOM_BIND_ADDR=0.0.0.0
# POST /v1/process waits for the synchronous download; the result budget floors the submit call:
export AXIOM_PROCESSOR_TIMEOUT=180s
# Note the near-identical pair (different scope):
#   AXIOM_PROCESSOR_TIMEOUT        — DISPATCHER-side result-fetch budget
#   AXIOM_PROCESSOR_SOURCE_TIMEOUT — RUNNER-side source-download budget (default 120s)
```

On the runner side leave `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` unset or
pointing at a nonexistent path — then local delivery is constructively
impossible and every source arrives via the signed URL. The URL expires with the
job's lease; downloaded bytes run the same hash gate as local files and die with
the ACK (contract §18/§19 test 13).

> Earlier experiments with an rsync bridge or sshfs mount are superseded by this
> mechanism — do not stage Zotero copies on the GPU host (contract §15 / §19 test 12).

## 6. Runner identity + GPU sampler labels

With multiple runners, every log line and every job row must say which runner
produced it:

```bash
export AXIOM_PROCESSOR_RUNNER_NAME=<runner-label>
```

The label lands in the phase log line (`phases[ok]: runner=<label> job=…`) and
in `ingest_jobs.runner_name` at claim time. Distribution is then pure SQL:

```sql
SELECT runner_name, count(*), avg(completed_at - started_at)
FROM ingest_jobs WHERE status = 'completed' GROUP BY 1;
```

> The column is deliberately **not** `processor_name` (that one carries the
> processor software identity written at completion and must not be clobbered).

GPU sampler per runner (30-s cadence), label first so lines stay attributable
after log merge:

```bash
nohup sh -c 'while true; do echo "<runner-label> $(date +%s) $(nvidia-smi --query-gpu=index,memory.used,utilization.gpu --format=csv,noheader)"; sleep 30; done' \
  > /tmp/gpu_sampler_<runner>.log 2>&1 &
```

With `CUDA_VISIBLE_DEVICES` pinning per container, GPU index + label identify
the runner unambiguously.

## Runner env variables (reference)

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_PROCESSOR_BIND_ADDR` | `127.0.0.1` | `0.0.0.0` for remote access |
| `AXIOM_PROCESSOR_PORT` | `8537` | HTTP port |
| `AXIOM_PROCESSOR_COMPUTE` | `reference` | `real` for the GPU pipeline |
| `AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS` | `1` | Marker+models are VRAM-heavy; keep 1 per GPU |
| `AXIOM_PROCESSOR_WORK_ROOT` | `/tmp/axiom_processor_work` | Temporary job state |
| `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` | — | Local host paths the runner may read |
| `AXIOM_PROCESSOR_RESULT_RETENTION` | `3600` | Seconds before unacked results expire |

## Mass processing with the remote runner

1. Keep dispatcher `Concurrency=1` (the runner enforces
   `MAX_CONCURRENT_JOBS=1` anyway; parallel jobs would contend for VRAM).
2. Expect ~30 s per small document on a warm cache on a 3090-class card; large
   scanned books scale with the OCR load.
3. The runner holds results until ACK; the dispatcher's ack-retry pass recovers
   if the dispatcher restarts mid-batch.

## Known limitations (without referencing specific machines)

- `capabilities.models.dense_embedding.name` reports `reference-bge-m3` even in
  real mode (cosmetic; the vectors themselves are genuine 1024-dim BGE-M3).
  Deferred.
- The runner loads all three model families per process; the VRAM footprint is
  ~2.8 GB — fits a 12 GB card, leaves room on 24 GB cards for a second pinned
  runner per card.

Continue: [Monitoring](monitoring.md) · [Troubleshooting](troubleshooting.md) ·
[PROCESSOR_CONTRACT v1](../developer-guide/processor-contract.md)
