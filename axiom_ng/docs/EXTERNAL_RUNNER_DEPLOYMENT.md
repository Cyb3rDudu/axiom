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

The runner needs two source trees: `axiom_ng_runner/` itself and the
`ai_researcher/` compute cores from `axiom_backend/` (chunker, embedder,
entity/relation extractors).

```bash
mkdir -p /tmp/runner_poc
rsync -av --exclude='.venv' --exclude='__pycache__' --exclude='.pytest_cache' \
  <repo>/axiom_ng_runner/ /tmp/runner_poc/axiom_ng_runner/
rsync -av --exclude='.venv' --exclude='__pycache__' \
  <repo>/axiom_backend/ai_researcher/ /tmp/runner_poc/ai_researcher/

ssh <user>@<gpu-host> "mkdir -p ~/Code/runner-poc"
scp -r /tmp/runner_poc/* <user>@<gpu-host>:~/Code/runner-poc/
```

## 2. Containerfile

Key points learned from the POC (all four build iterations):

1. **Mirror the repo layout.** `runner.py` derives
   `_AXIOM_BACKEND = <parent-of-runner>/axiom_backend`, so place
   `ai_researcher/` under `/app/axiom_backend/` in the image.
2. **`database/` package must be present.** `core_rag/__init__` imports
   `pgvector_store`, which imports `from database.database import get_db` at
   module level. Ship an empty stub or the real package + `psycopg2-binary`.
3. **Triton JIT needs gcc + libc6-dev** (`crti.o`). Do not use
   `--no-install-recommends` without adding these explicitly, or the first
   dense-embedding run dies.
4. **Pin versions identical to the reference venv**
   (`axiom_ng_runner/requirements-heavy.txt`), above all
   `marker-pdf==1.10.2`. Divergent versions produce divergent output.

```dockerfile
FROM python:3.11-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc libc6-dev pandoc libglib2.0-0 libgl1 \
    && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir \
    marker-pdf==1.10.2 \
    FlagEmbedding gliner transformers \
    pymupdf4llm fastapi uvicorn pydantic ebooklib \
    psycopg2-binary==2.9.12
COPY axiom_ng_runner/ /app/axiom_ng_runner/
COPY ai_researcher/    /app/axiom_backend/ai_researcher/
WORKDIR /app
ENV PYTHONPATH=/app
EXPOSE 8012
CMD ["python", "-m", "axiom_ng_runner"]
```

## 3. Build and run with GPU

```bash
podman build -t runner-poc ~/Code/runner-poc/

# CDI device injection — mounts driver libs automatically,
# no manual /dev/nvidia* device mapping needed:
podman run -d --name runner-poc \
  --device nvidia.com/gpu=all \
  -e AXIOM_PROCESSOR_COMPUTE=real \
  -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 \
  -e AXIOM_PROCESSOR_PORT=8012 \
  -p 8012:8012 \
  runner-poc
```

`--device nvidia.com/gpu=all` exposes all host GPUs. To pin to one GPU, use
the CDI device name for that index instead of `all` (see
`/var/run/cdi/nvidia-container-toolkit.json` for available names).

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
curl http://<gpu-host>:8012/v1/health
curl http://<gpu-host>:8012/v1/capabilities
```

First run downloads ~3 GB of model weights (Marker surya + GLiNER + mREBEL);
subsequent runs are warm-cache.

## 5. Wire the dispatcher to it

On the axiom-ng host (Mac):

```bash
export AXIOMNG_DISPATCHER_ENABLED=true
export AXIOMNG_PROCESSOR_URL=http://<gpu-host>:8012
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

## Runtime knobs (runner env vars)

| Env var | Default | Meaning |
| --- | --- | --- |
| `AXIOM_PROCESSOR_BIND_ADDR` | `127.0.0.1` | `0.0.0.0` for remote access |
| `AXIOM_PROCESSOR_PORT` | `8537` | HTTP port (8012 in our setups) |
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
