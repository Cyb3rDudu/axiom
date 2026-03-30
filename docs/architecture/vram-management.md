# VRAM Management

AXIOM runs multiple GPU-accelerated ML models on a single NVIDIA GPU (tested on 16 GB RTX 4060 Ti). This page describes how GPU memory is shared between the backend and document processor services to avoid out-of-memory errors.

## GPU Memory Budget

The 16 GB VRAM budget is shared between two services:

| Service | Models | Approximate VRAM |
|---|---|---|
| **axiom-backend** | BGE-M3 embedder (~1.5 GB), BGE-reranker-v2-m3 (~1.1 GB), GLiNER (~0.5 GB) | ~3.1 GB peak |
| **doc-processor** | Marker PDF converter (~2-4 GB), mREBEL relation extractor (~2.4 GB) | ~4 GB peak |

Because both services share the same physical GPU, AXIOM uses several strategies to keep total VRAM usage within budget.

## Model Cache with Idle Timeout

The backend's `ModelCache` (in `core_rag/model_cache.py`) is a thread-safe singleton that manages the embedder and reranker lifecycle:

- Models are **loaded on first use** and kept in memory for fast subsequent requests.
- After **120 seconds of inactivity** (configurable via `IDLE_TIMEOUT_SECONDS`), a timer fires and unloads both models, calling `torch.cuda.empty_cache()` and `gc.collect()`.
- This ensures that during long document processing jobs, the backend releases GPU memory that the document processor needs for Marker and mREBEL.

## Document Processing Pipeline Order

The document processor runs GPU-intensive steps in a specific order to minimize concurrent VRAM usage:

```
1. Marker PDF conversion        (GPU: ~2-4 GB)
   |
2. Embed chunks with BGE-M3     (GPU: ~1.5 GB)
   |
3. GLiNER entity extraction     (GPU: ~0.5 GB)
   |
4. ── Unload ALL GPU models ──  (GPU: ~0 GB)
   |  - model_cache.clear_cache()  (embedder + reranker)
   |  - unload_gliner()
   |  - Delete Marker converter and model_dict
   |  - gc.collect() + torch.cuda.empty_cache()
   |
5. mREBEL relation extraction   (GPU: ~2.4 GB)
   |
6. unload_mrebel()              (GPU: ~0 GB)
   |
7. Image embedding (CLIP)       (GPU: ~0.5 GB, if enabled)
```

The critical step is **step 4**: before loading mREBEL, the processor aggressively frees all other GPU models. This is necessary because mREBEL alone requires approximately 2.4 GB, and loading it alongside Marker or the embedder would exceed the 16 GB budget.

## GLiNER Lifecycle

GLiNER (`urchade/gliner_multi-v2.1`, ~0.5 GB) is used in two contexts:

1. **Document processing** -- entity extraction runs after embedding, then GLiNER is unloaded before mREBEL.
2. **Query-time graph retrieval** -- GLiNER runs on the user's query text (~5 ms) to extract entities for cross-document lookup. The model is lazy-loaded and kept in memory (managed by the backend's model cache idle timeout).

The `unload_gliner()` function explicitly deletes the model and calls `torch.cuda.empty_cache()`.

## mREBEL Lifecycle

mREBEL (`Babelscape/mrebel-large`, ~2.4 GB) is the most VRAM-intensive model after Marker. It is:

1. **Never kept resident** -- loaded on demand via `load_mrebel()`, used for extraction, then immediately unloaded via `unload_mrebel()`.
2. **Loaded only after cleanup** -- the document processor explicitly frees all other GPU models before calling `load_mrebel()`.
3. **Logged** -- on load, the current VRAM usage is logged to help diagnose memory issues.

## Failure Handling

If mREBEL loading or extraction fails (e.g., due to insufficient VRAM), the error is caught and logged as a non-fatal warning. The document continues processing without relation extraction. The `unload_mrebel()` call runs in the `except` block to ensure cleanup even on failure.

## Monitoring VRAM Usage

Check current GPU memory allocation in the document processor logs:

```
[doc_id] GPU freed: 0.3GB used
[doc_id] mREBEL loaded on cuda (2.7GB VRAM)
[doc_id] mREBEL extracted 42 unique triples from 15 chunks
[doc_id] mREBEL unloaded, GPU memory freed
```

You can also monitor from the host:

```bash
# Real-time GPU memory usage
watch -n 1 nvidia-smi

# Inside Docker container
docker exec axiom-doc-processor nvidia-smi
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `IDLE_TIMEOUT_SECONDS` | `120` | Seconds before idle backend models are unloaded (in `model_cache.py`) |
| `ENABLE_KNOWLEDGE_GRAPH` | `true` | When `false`, skips GLiNER and mREBEL entirely |

## Next Steps

- [RAG and Knowledge Graph](../user-guide/documents/rag-knowledge-graph.md) - How the knowledge graph enhances retrieval
- [Architecture Overview](index.md) - System design and components
