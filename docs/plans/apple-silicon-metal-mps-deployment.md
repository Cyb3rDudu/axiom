# Plan: Apple Silicon Metal/MPS Deployment with Native GPU Acceleration

**Date:** 2026-03-03
**Branch:** `feat/apple-silicon-deploy`
**Status:** Implemented (April 2026) — hybrid native+container stack ships as `docker-compose.macos.yml` plus macOS-native backend, documented in [DEV_MACOS.md](../DEV_MACOS.md). This plan is retained as historical context; see the DEV_MACOS page for the current setup.
**Prerequisite:** [Apple Silicon Docker Compose Stack Plan](apple-silicon-docker-compose.md) (CPU-only fallback)

---

## Problem

Docker Desktop and Apple Containers (`apple/container`) both **cannot pass through Metal GPU** to Linux containers on Apple Silicon:

| Runtime | GPU support | Notes |
|---------|------------|-------|
| **Docker Desktop** | None | Linux VM, no Metal exposure |
| **Apple Containers** (`apple/container`) | None | [Officially wontfix](https://github.com/apple/containerization/issues/46); requires macOS 26 |

PyTorch's MPS backend (`torch.backends.mps`) requires **direct Metal API access** which is only available on native macOS — not inside any container VM. This means the 5 local ML models used by Axiom cannot benefit from Apple Silicon GPU acceleration when containerized.

**Bottom line:** To use the GPU on this Mac for testing, the ML-heavy services must run **natively on macOS**, not in containers.

---

## Architecture: Hybrid Native + Container

```
┌─────────────────────────────────────────────────────────┐
│                    macOS (Native)                        │
│                                                         │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │   backend    │  │doc-processor │  │     cli       │  │
│  │  (FastAPI)   │  │  (embeddings │  │  (ingestion)  │  │
│  │  port 8000   │  │   & marker)  │  │               │  │
│  │  MPS GPU ✓   │  │  MPS GPU ✓   │  │  MPS GPU ✓    │  │
│  └──────┬───────┘  └──────┬───────┘  └───────────────┘  │
│         │                 │                              │
│  ═══════╪═════════════════╪══════════════════════════    │
│         │    Docker / Apple Containers                   │
│  ┌──────┴───────┐  ┌─────┴────┐  ┌──────────────┐      │
│  │   postgres    │  │  nginx   │  │   frontend   │      │
│  │  (pgvector)   │  │ port 80  │  │   port 3000  │      │
│  │  port 5432    │  │          │  │              │      │
│  └──────────────┘  └──────────┘  └──────────────┘      │
└─────────────────────────────────────────────────────────┘
```

**Native (macOS):** backend, doc-processor, cli — full MPS GPU access
**Containerized:** postgres, nginx, frontend — no GPU needed

---

## Local ML Model Inventory & MPS Compatibility

### 1. Text Embedder: `BAAI/bge-m3` (FlagEmbedding)

| Attribute | Value |
|-----------|-------|
| **File** | `core_rag/embedder.py` |
| **Framework** | `FlagEmbedding.BGEM3FlagModel` |
| **Output** | 1024-dim dense + sparse (lexical weights) |
| **MPS support** | **Partial / Untested** |
| **Issue** | FlagEmbedding internally uses PyTorch ops; some may lack MPS kernels. No official MPS testing from BAAI. Runtime errors possible (e.g., `NotImplementedError: MPS does not support...`) |
| **Action needed** | Test on MPS; fallback to CPU if unsupported ops are hit. Consider wrapping with `PYTORCH_ENABLE_MPS_FALLBACK=1` env var to auto-fallback unsupported ops to CPU. |

### 2. Reranker: `BAAI/bge-reranker-v2-m3` (FlagEmbedding)

| Attribute | Value |
|-----------|-------|
| **File** | `core_rag/reranker.py` |
| **Framework** | `FlagEmbedding.FlagReranker` |
| **MPS support** | **Partial / Untested** |
| **Issue** | Same as embedder — FlagReranker uses cross-encoder architecture. Already uses FP16 on MPS (line 48). |
| **Action needed** | Test on MPS with `PYTORCH_ENABLE_MPS_FALLBACK=1`. If stable, keep. If not, force CPU for reranker only. |

### 3. Vision Embedder: `clip-ViT-B-32` (sentence-transformers)

| Attribute | Value |
|-----------|-------|
| **File** | `core_rag/vision_embedder.py` |
| **Framework** | `sentence_transformers.SentenceTransformer` |
| **Output** | 512-dim image embeddings |
| **MPS support** | **Yes — well tested** |
| **Notes** | CLIP models are widely used on MPS. sentence-transformers passes `device="mps"` directly to PyTorch. Works out of the box. |
| **Action needed** | None — already compatible. |

### 4. PDF Processing: marker-pdf (multi-model pipeline)

| Attribute | Value |
|-----------|-------|
| **File** | `core_rag/processor.py` |
| **Framework** | `marker.models.create_model_dict(device=...)` |
| **Sub-models** | Layout detection, table detection, OCR, formula detection, segmentation |
| **MPS support** | **Yes — officially supported** |
| **Notes** | marker-pdf explicitly lists Apple MPS as a supported device alongside CUDA and CPU. |
| **Action needed** | Pass `device="mps"` to `create_model_dict()`. Already handled by hardware_detector. |

### 5. NER: spaCy `en_core_web_lg`

| Attribute | Value |
|-----------|-------|
| **File** | `core_rag/entity_extractor.py` |
| **Framework** | spaCy 3.7.2 |
| **MPS support** | **N/A — CPU only** |
| **Notes** | spaCy NER pipelines run on CPU by default. The model is lightweight (~560 MB). No GPU benefit for inference at this scale. |
| **Action needed** | None. Already CPU. |

### Summary

| Model | Framework | MPS Ready? | Action |
|-------|-----------|------------|--------|
| `BAAI/bge-m3` | FlagEmbedding | Partial | Test + MPS fallback env var |
| `BAAI/bge-reranker-v2-m3` | FlagEmbedding | Partial | Test + MPS fallback env var |
| `clip-ViT-B-32` | sentence-transformers | Yes | None |
| marker-pdf pipeline | marker | Yes | None |
| `en_core_web_lg` | spaCy | N/A (CPU) | None |

### Model Replacement Candidates (if MPS fails)

If FlagEmbedding models prove unstable on MPS, these are drop-in alternatives with verified MPS support:

| Current | Replacement | Framework | MPS Status | Trade-off |
|---------|-------------|-----------|------------|-----------|
| `BAAI/bge-m3` (FlagEmbedding) | `BAAI/bge-m3` (sentence-transformers) | sentence-transformers | Verified | Loses sparse/lexical embeddings; dense-only |
| `BAAI/bge-m3` (FlagEmbedding) | `BAAI/bge-large-en-v1.5` | sentence-transformers | Verified | English-only, 1024-dim dense, no sparse |
| `BAAI/bge-reranker-v2-m3` | `cross-encoder/ms-marco-MiniLM-L-12-v2` | sentence-transformers CrossEncoder | Verified | Slightly lower accuracy, much smaller model |

**Recommendation:** Try FlagEmbedding on MPS first with `PYTORCH_ENABLE_MPS_FALLBACK=1`. Only replace if there are stability issues or unacceptable performance.

---

## Code Changes Required

### Phase 1: Fix CUDA-specific code paths for MPS

The codebase has several `torch.cuda.*` calls that will crash on MPS. These all need MPS-aware branching:

#### 1a. Memory monitoring — `embedder.py`, `vision_embedder.py`

**Current** (lines 117-132 in embedder.py, 112-127 in vision_embedder.py):
```python
def _get_gpu_memory_usage(self) -> float:
    device_info = hardware_detector.detect_hardware()
    if device_info["device_type"] not in ["cuda", "rocm"]:
        return 0.0  # ← MPS returns 0, skips all memory management
```

**Required:** Add MPS memory tracking via `torch.mps.current_allocated_memory()` and `torch.mps.driver_allocated_memory()` (available since PyTorch 2.1):
```python
if device_info["device_type"] == "mps":
    try:
        allocated = torch.mps.current_allocated_memory()
        driver_allocated = torch.mps.driver_allocated_memory()
        # Unified memory — use system total as denominator
        total = info.get("memory_gb", 8) * (1024**3)
        return allocated / total
    except:
        return 0.0
```

#### 1b. Cache cleanup — `embedder.py` (lines 134-161), `vision_embedder.py` (lines 129-156)

**Current:**
```python
def _cleanup_gpu_memory(self, force=False):
    if device_info["device_type"] not in ["cuda", "rocm"]:
        return  # ← MPS never cleaned up
    torch.cuda.empty_cache()
```

**Required:** Add MPS cache clearing:
```python
elif device_info["device_type"] == "mps":
    torch.mps.empty_cache()
    gc.collect()
```

#### 1c. Inline `torch.cuda.empty_cache()` calls

These appear in 6 locations across embedder.py (lines 233, 346, 367) and vision_embedder.py (lines 243, 307, 318). Each needs:
```python
if device_info["device_type"] == "mps":
    torch.mps.empty_cache()
elif device_info["device_type"] in ["cuda", "rocm"]:
    torch.cuda.empty_cache()
```

**Better approach:** Add a helper method to `hardware_detection.py`:
```python
class HardwareDetector:
    def empty_cache(self):
        """Device-agnostic cache cleanup."""
        info = self.detect_hardware()
        if info["device_type"] in ["cuda", "rocm"]:
            torch.cuda.empty_cache()
        elif info["device_type"] == "mps":
            torch.mps.empty_cache()
        gc.collect()
```

Then replace all 6+ callsites with `hardware_detector.empty_cache()`.

#### 1d. CUDA OOM error handling — `embedder.py` (line 361), `vision_embedder.py` (line 312)

**Current:**
```python
except RuntimeError as re:
    if "CUDA out of memory" in str(re):
```

**Required:** Also catch MPS OOM:
```python
if "CUDA out of memory" in str(re) or "MPS backend out of memory" in str(re):
```

### Phase 2: Environment & configuration

#### 2a. New env var for MPS fallback

Add to `.env.example` and backend startup:
```env
# Apple Silicon: auto-fallback unsupported MPS ops to CPU
# Recommended: true for initial testing, false once stable
PYTORCH_ENABLE_MPS_FALLBACK=1
```

#### 2b. MPS-specific batch sizes in `hardware_detection.py`

Currently (line 262-264):
```python
elif device_type == "mps":
    return base_batch_size // 2
```

This is conservative. Apple Silicon unified memory is typically 16-128 GB. Consider:
```python
elif device_type == "mps":
    memory_gb = info.get("memory_gb", 16)
    if memory_gb >= 32:
        return base_batch_size  # Full batch on high-memory Macs
    else:
        return base_batch_size // 2
```

### Phase 3: Deployment scripts

#### 3a. `docker-compose.infra.yml` — Container-only infrastructure

```yaml
# Only postgres, nginx, frontend — no backend, no doc-processor
services:
  postgres:
    image: pgvector/pgvector:pg15
    ports:
      - "5432:5432"
    # ...same as existing

  nginx:
    build: ./nginx
    ports:
      - "${AXIOM_PORT:-80}:80"
    # ...same but backend upstream points to host.docker.internal:8000

  frontend:
    build: ./axiom_frontend
    # ...same
```

**Nginx change:** Backend upstream must route to the host machine, not a container:
```nginx
location /api/ {
    proxy_pass http://host.docker.internal:8000;
    # ...rest same
}
location /ws {
    proxy_pass http://host.docker.internal:8000;
    # ...rest same
}
```

#### 3b. `scripts/start-native-backend.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

cd axiom_backend

# Activate venv (or create if needed)
if [ ! -d ".venv" ]; then
    python3 -m venv .venv
    source .venv/bin/activate
    pip install -r requirements.txt
else
    source .venv/bin/activate
fi

# MPS configuration
export PREFERRED_DEVICE_TYPE=mps
export FORCE_CPU_MODE=false
export PYTORCH_ENABLE_MPS_FALLBACK=1
export EMBEDDING_BATCH_SIZE=64

# Database (connect to containerized postgres)
export DATABASE_URL="postgresql://${POSTGRES_USER:-axiom_user}:${POSTGRES_PASSWORD:-axiom_password}@localhost:5432/${POSTGRES_DB:-axiom_db}"

# Start backend
python -m uvicorn main:app --host 0.0.0.0 --port 8000 --reload
```

#### 3c. `scripts/start-native-doc-processor.sh`

Same env vars, runs `python services/background_document_processor.py`.

---

## Apple Containers vs Docker for Infrastructure Services

Apple's `container` CLI (`apple/container`) is a potential replacement for Docker Desktop for the non-ML services (postgres, nginx, frontend):

| Feature | Docker Desktop | Apple Containers |
|---------|---------------|-----------------|
| macOS requirement | Any | **macOS 26 only** |
| Compose orchestration | `docker compose` | None (single container per VM) |
| OCI image compat | Full | Full |
| GPU passthrough | No | **No** ([wontfix](https://github.com/apple/containerization/issues/46)) |
| Maturity | Production | Pre-1.0, breaking changes expected |
| Resource usage | ~2-4 GB VM | Lightweight VMs (sub-second start) |
| Registry support | Full | Full |

**Recommendation for now:** Use Docker Desktop (or OrbStack) for the infrastructure containers. Apple Containers is not yet viable:
- Requires macOS 26 (current system is Darwin 25.3.0 / macOS Sequoia 15)
- No compose-like orchestration for multi-service stacks
- Pre-1.0 with breaking changes between minor versions

**When to revisit:** Once macOS 26 ships and `apple/container` reaches 1.0, it would be a lighter-weight alternative to Docker Desktop for the 3 infrastructure services. GPU passthrough is irrelevant here since only postgres/nginx/frontend are containerized.

---

## File Changes Summary

| File | Action | Description |
|------|--------|-------------|
| `axiom_backend/ai_researcher/hardware_detection.py` | **Edit** | Add `empty_cache()` helper, MPS memory tracking |
| `axiom_backend/ai_researcher/core_rag/embedder.py` | **Edit** | Replace `torch.cuda.*` with device-agnostic helpers |
| `axiom_backend/ai_researcher/core_rag/vision_embedder.py` | **Edit** | Same as embedder |
| `axiom_backend/ai_researcher/core_rag/reranker.py` | **Edit** | Minor: MPS OOM handling |
| `docker-compose.infra.yml` | **Create** | Infrastructure-only compose (postgres, nginx, frontend) |
| `nginx/nginx.host-backend.conf` | **Create** | Nginx config routing to `host.docker.internal` |
| `scripts/start-native-backend.sh` | **Create** | Native macOS backend launcher with MPS |
| `scripts/start-native-doc-processor.sh` | **Create** | Native macOS doc-processor launcher |
| `.env.example` | **Edit** | Add MPS section, `PYTORCH_ENABLE_MPS_FALLBACK` |

---

## Testing Plan

### Step 1: Validate MPS model loading
```bash
# Quick smoke test — does each model load on MPS without crashing?
PREFERRED_DEVICE_TYPE=mps PYTORCH_ENABLE_MPS_FALLBACK=1 python3 -c "
from FlagEmbedding import BGEM3FlagModel
model = BGEM3FlagModel('BAAI/bge-m3', use_fp16=False)
result = model.encode(['test sentence'], return_dense=True, return_sparse=True)
print('BGE-M3 OK:', result['dense_vecs'].shape)
"
```

### Step 2: Validate embedding quality
- Compare MPS vs CPU embeddings for identical inputs
- Cosine similarity should be >0.999 (numerical precision differences only)

### Step 3: Performance benchmark
```
Metric              | CPU (M-series) | MPS (M-series) | Expected speedup
--------------------|---------------|----------------|------------------
BGE-M3 encode 100   |      ?s       |      ?s        | 2-5x
Reranker 100 pairs   |      ?s       |      ?s        | 2-3x
CLIP 10 images       |      ?s       |      ?s        | 3-5x
marker-pdf 1 doc     |      ?s       |      ?s        | 2-4x
```

### Step 4: End-to-end integration
- Start infra containers + native backend
- Upload a PDF, verify processing pipeline works
- Run a research mission, verify embeddings + reranking + writing

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| FlagEmbedding crashes on MPS | Embeddings/reranking broken | `PYTORCH_ENABLE_MPS_FALLBACK=1` auto-routes unsupported ops to CPU; or force CPU for these models only |
| MPS numerical differences | Slightly different search results vs CUDA/CPU | Expected and acceptable; validate with cosine similarity test |
| Native Python env management | Dev friction (no container isolation) | Provide venv setup scripts; pin requirements.txt |
| `host.docker.internal` DNS | Doesn't resolve in all Docker versions | Fallback to `172.17.0.1` (default Docker bridge gateway) |
| PyTorch MPS memory leaks | Growing memory over time | `torch.mps.empty_cache()` + periodic `gc.collect()`; monitor with Activity Monitor |
| marker-pdf sub-model MPS issues | PDF processing fails | marker-pdf officially supports MPS; if issues arise, force `device="cpu"` for marker only |

---

## Future: Full Apple Containers Stack (macOS 26+)

Once macOS 26 ships and `apple/container` matures:

```
┌──────────────────────────────────────────┐
│  apple/container (macOS 26+)             │
│  ┌──────────┐ ┌────────┐ ┌───────────┐  │
│  │ postgres  │ │ nginx  │ │ frontend  │  │
│  └──────────┘ └────────┘ └───────────┘  │
└──────────────────────────────────────────┘
        ↕ localhost networking ↕
┌──────────────────────────────────────────┐
│  macOS Native (MPS GPU)                  │
│  ┌──────────┐ ┌──────────────┐           │
│  │ backend   │ │doc-processor │           │
│  └──────────┘ └──────────────┘           │
└──────────────────────────────────────────┘
```

Benefits over Docker Desktop:
- Sub-second container startup
- Lower memory overhead (no full Linux VM)
- Native Apple silicon optimization
- Still no GPU passthrough, but that's fine — ML runs natively

This hybrid approach is the **long-term architecture** regardless of container runtime, because Metal/MPS will never be available inside a Linux container on macOS.
