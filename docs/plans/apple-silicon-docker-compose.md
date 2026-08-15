# Plan: Apple Silicon Docker Compose Stack

**Date:** 2026-03-03
**Branch:** `feat/apple-silicon-docker`
**Status:** Proposed

---

## Problem

The current Docker setup is built around NVIDIA CUDA (`nvidia/cuda:12.9.1-base-ubuntu24.04` base image) and x86_64 assumptions. On Apple Silicon Macs:

- The CUDA base image doesn't exist for ARM64
- NVIDIA GPU deploy reservations cause compose failures
- Running x86_64 images under Rosetta emulation is 3-5x slower and unreliable for heavy ML workloads
- Docker Desktop for Mac runs a Linux ARM64 VM — no Metal/MPS GPU access from containers

The existing `docker-compose.cpu.yml` still references pre-built `axiom-backend` / `axiom-doc-processor` images that were built from the CUDA Dockerfile, so it doesn't actually solve the build problem on ARM64.

## Goals

1. Native ARM64 container builds — no Rosetta emulation
2. CPU-optimized PyTorch (skip 4+ GB of CUDA libraries)
3. Single `docker compose` command to build and run on any Apple Silicon Mac
4. Minimal divergence from the existing stack — same app code, same volumes, same networking

---

## Deliverables

### 1. `axiom_backend/Dockerfile.arm64` — ARM64-native backend image

**Base image:** `python:3.12-slim-bookworm` (official ARM64 builds, ~150 MB vs ~3.6 GB CUDA)

**Key changes from current Dockerfile:**

| Concern | Current (CUDA) | ARM64 |
|---------|----------------|-------|
| Base image | `nvidia/cuda:12.9.1-base-ubuntu24.04` | `python:3.12-slim-bookworm` |
| Python | Installed via apt | Included in base |
| PyTorch | Full CUDA build (~2 GB) | CPU-only (`--index-url https://download.pytorch.org/whl/cpu`) (~200 MB) |
| System deps | Same | Same (WeasyPrint, pandoc, etc.) |
| Image size | ~6-8 GB | ~2-3 GB |

**Structure:**
```dockerfile
# Stage 1: deps
FROM python:3.12-slim-bookworm AS deps
# Install system deps (WeasyPrint, pandoc, dos2unix, build-essential)
# Create venv
# Install CPU-only PyTorch first (separate layer for caching)
# Install remaining requirements.txt

# Stage 2: app
FROM deps AS app
# Copy app code, fix line endings, set PYTHONPATH, expose 8000
```

**PyTorch installation strategy:**
```bash
# Install CPU-only PyTorch before the rest of requirements
pip install torch torchvision torchaudio --index-url https://download.pytorch.org/whl/cpu
# Then install everything else (torch already satisfied, won't re-download CUDA version)
pip install --no-cache-dir -r requirements.txt
```

### 2. `docker-compose.apple-silicon.yml` — Compose file

```yaml
services:
  postgres:
    image: pgvector/pgvector:pg15          # Has ARM64 builds
    # ... same config as docker-compose.cpu.yml

  nginx:
    build: ./nginx                          # nginx:alpine has ARM64 builds
    # ... same config

  backend:
    build:
      context: ./axiom_backend
      dockerfile: Dockerfile.arm64
    platform: linux/arm64
    environment:
      - FORCE_CPU_MODE=true
      - PREFERRED_DEVICE_TYPE=cpu
      - EMBEDDING_BATCH_SIZE=128            # Tuned for Apple Silicon (8-16 cores)
      # ... rest same as cpu.yml
    # NO deploy.resources.reservations (no GPU)

  frontend:
    build: ./axiom_frontend                 # node:20-alpine has ARM64 builds
    platform: linux/arm64

  doc-processor:
    build:
      context: ./axiom_backend
      dockerfile: Dockerfile.arm64
    platform: linux/arm64
    command: ["python", "-u", "services/background_document_processor.py"]
    environment:
      - FORCE_CPU_MODE=true
      - PREFERRED_DEVICE_TYPE=cpu
      - EMBEDDING_BATCH_SIZE=128
      # ... rest same
    # NO deploy.resources.reservations (no GPU)

  cli:
    build:
      context: ./axiom_backend
      dockerfile: Dockerfile.arm64
    platform: linux/arm64
    environment:
      - FORCE_CPU_MODE=true
      - PREFERRED_DEVICE_TYPE=cpu
    profiles: [cli]
    # NO deploy.resources.reservations (no GPU)
```

**Key differences from `docker-compose.cpu.yml`:**

| Aspect | `cpu.yml` | `apple-silicon.yml` |
|--------|-----------|---------------------|
| Images | Pre-built (`image: axiom-backend`) | Built locally (`build:`) |
| Dockerfile | Default (CUDA-based) | `Dockerfile.arm64` |
| Platform | Unspecified | `linux/arm64` explicit |
| Batch size | 256 (server-class CPU) | 128 (Apple Silicon efficiency cores) |
| GPU sections | Absent | Absent |

### 3. Performance tuning defaults

Apple Silicon runs Docker in a Linux VM with allocated resources. Recommended `.env` overrides:

```env
# Apple Silicon optimized defaults
FORCE_CPU_MODE=true
PREFERRED_DEVICE_TYPE=cpu

# Batch sizes tuned for 8-16 ARM64 cores
EMBEDDING_BATCH_SIZE=128

# Thread count — match Docker Desktop allocated CPUs
MAX_WORKER_THREADS=8

# Lower concurrent LLM since this is typically a dev machine
GLOBAL_MAX_CONCURRENT_LLM_REQUESTS=50
MAX_CONCURRENT_REQUESTS=10
```

### 4. `scripts/build-apple-silicon.sh` — Build helper

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "Building Axiom for Apple Silicon (ARM64)..."
docker compose -f docker-compose.apple-silicon.yml build --parallel
echo "Done. Run with: docker compose -f docker-compose.apple-silicon.yml up -d"
```

---

## File Changes Summary

| File | Action | Description |
|------|--------|-------------|
| `axiom_backend/Dockerfile.arm64` | **Create** | ARM64-native backend Dockerfile (CPU-only PyTorch) |
| `docker-compose.apple-silicon.yml` | **Create** | Full compose stack for Apple Silicon |
| `scripts/build-apple-silicon.sh` | **Create** | Convenience build script |
| `.env.example` | **Edit** | Add Apple Silicon section with recommended values |

No changes to existing Dockerfiles or compose files — this is purely additive.

---

## Image Compatibility Matrix

| Service | Image | ARM64 native? | Notes |
|---------|-------|---------------|-------|
| postgres | `pgvector/pgvector:pg15` | Yes | Official multi-arch |
| nginx | `nginx:alpine` | Yes | Official multi-arch |
| backend | Custom build | Yes | New `Dockerfile.arm64` |
| frontend | Custom build (`node:20-alpine`) | Yes | Already ARM64 compatible |
| doc-processor | Same as backend | Yes | Same `Dockerfile.arm64` |
| cli | Same as backend | Yes | Same `Dockerfile.arm64` |

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Some pip packages lack ARM64 wheels | Build fails | `python:3.12-slim-bookworm` includes build tools; add `build-essential` for compilation from source |
| `marker-pdf` / `spacy` ARM64 compat | Possible build issues | Both have ARM64 wheels as of 2025; pin known-good versions if needed |
| `FlagEmbedding` ARM64 | Build from source | Depends on PyTorch which we install CPU-only ARM64; should work |
| Docker Desktop memory limits | OOM during builds | Document minimum 8 GB RAM allocation in Docker Desktop settings |
| No GPU acceleration in containers | Slower embeddings | Expected tradeoff; embeddings still fast on M-series CPU; point users to host-native setup for heavy workloads |

---

## Usage

```bash
# First time — build all images
docker compose -f docker-compose.apple-silicon.yml build

# Start the stack
docker compose -f docker-compose.apple-silicon.yml up -d

# View logs
docker compose -f docker-compose.apple-silicon.yml logs -f

# Stop
docker compose -f docker-compose.apple-silicon.yml down

# CLI access (on-demand)
docker compose -f docker-compose.apple-silicon.yml run --rm cli bash
```

---

## Out of Scope

- **Metal/MPS passthrough** — Docker Desktop for Mac doesn't expose Metal to containers. Users needing GPU acceleration should run the backend natively on macOS.
- **Rosetta fallback** — We explicitly target ARM64. If a dependency truly lacks ARM64 support, we'll address it case-by-case rather than falling back to x86 emulation.
- **Multi-arch image publishing** — This plan covers local builds only. Publishing `linux/amd64` + `linux/arm64` multi-arch images to a registry is a separate effort.
