# VRAM Management

AXIOM runs multiple GPU-accelerated ML models on a single NVIDIA GPU (tested on 12 GB A3000 and 16 GB RTX 4060 Ti). This page describes how GPU memory is shared between processes to avoid out-of-memory errors.

## GPU Memory Budget

All GPU models run in isolated subprocesses. Only one heavyweight process occupies the GPU at a time.

| Process | Models | Approximate VRAM | Lifecycle |
|---|---|---|---|
| **GPU worker** | BGE-M3 embedder (~1.5 GB), BGE-reranker-v2-m3 (~1.1 GB), GLiNER (~0.5 GB) | ~3.1 GB peak | Shared, idle-killed |
| **pdf_worker** | Marker PDF converter (~2-4 GB) | ~4 GB peak | Per-import, exits after use |
| **relation_worker** | mREBEL relation extractor (~2.4 GB) | ~2.4 GB peak | Per-import, exits after use |

Because these processes share the same physical GPU, AXIOM sequences GPU-intensive work during document imports and kills idle processes to reclaim VRAM.

## Subprocess Isolation Pattern

All GPU models run in subprocesses rather than inside the main backend or doc-processor processes. When a subprocess exits, the OS reclaims its entire CUDA context -- there is no residual memory leak. See [GPU Worker Architecture](gpu-worker.md) for the full design rationale.

The three subprocess types serve different purposes:

- **GPU worker** -- long-lived shared process serving the embedder, reranker, and GLiNER over a Unix socket. Used by both the backend (search, chat, missions) and the doc-processor (chunk embedding, entity extraction). Killed on idle to free VRAM.
- **pdf_worker** -- short-lived process that loads Marker, converts one PDF to markdown, writes output to disk, and exits. Spawned once per document import.
- **relation_worker** -- short-lived process that loads mREBEL, extracts relation triples from all chunks, writes output to disk, and exits. Spawned once per document import, after all other GPU work is done.

## Document Processing Pipeline Order

The document processor runs GPU-intensive steps in a specific order to minimize concurrent VRAM usage:

```
1. pdf_worker subprocess          (GPU: ~2-4 GB, then exits -> 0 GB)
   |
2. GPU worker: embed chunks       (GPU: ~1.5 GB, loaded on demand)
   |
3. GPU worker: GLiNER extraction  (GPU: ~0.5 GB, loaded on demand)
   |
4. model_cache.unload_all()       (GPU: ~0 GB, worker subprocess killed)
   |
5. relation_worker subprocess     (GPU: ~2.4 GB, then exits -> 0 GB)
```

The critical step is **step 4**: before spawning the relation worker, the doc-processor kills the GPU worker subprocess entirely. This guarantees a clean GPU with no residual CUDA context when mREBEL loads.

## Idle Unload

The GPU worker client runs a background monitor that kills the worker subprocess when:

1. No RPC call has been made for `AXIOM_GPU_WORKER_IDLE_SEC` seconds (default: 900).
2. The activity detector reports no active missions, document processing, or recent API requests.

When the worker is killed, the main backend process stays alive. The next GPU request (search query, chat message, document import) transparently respawns the worker in approximately 10 seconds.

## GLiNER Lifecycle

GLiNER (`urchade/gliner_multi-v2.1`, ~0.5 GB) is loaded inside the GPU worker and used in two contexts:

1. **Document processing** -- entity extraction runs after embedding, via the GPU worker's `extract_entities` RPC method. The GPU worker stays alive during this phase.
2. **Query-time graph retrieval** -- GLiNER runs on user query text (~5 ms) to extract entities for cross-document lookup. The model loads lazily on first use.

GLiNER is not individually unloadable. When the GPU worker is killed (idle timeout or force unload), all three models are freed together.

## mREBEL Lifecycle

mREBEL (`Babelscape/mrebel-large`, ~2.4 GB) runs in a dedicated `relation_worker` subprocess:

1. **Spawned only after GPU cleanup** -- the doc-processor kills the GPU worker before launching the relation worker.
2. **Exits immediately after** -- the subprocess writes triples to a JSON file and exits, freeing all VRAM.
3. **Non-fatal** -- if the relation worker fails (OOM, model error), the document continues processing without relation extraction.

## Failure Handling

- **GPU worker crash** -- the client retries once, killing the dead worker and spawning a fresh one.
- **pdf_worker failure** -- the document is marked as `failed`. Marker errors are captured in the subprocess stderr.
- **relation_worker failure** -- logged as a non-fatal warning. The document completes without relation extraction.
- **OOM during mREBEL** -- the relation worker subprocess exits and the OS reclaims all VRAM. No cleanup code needed.

## Monitoring VRAM Usage

GPU worker log lines indicate model lifecycle events:

```
[gpu-worker] INFO: Loading TextEmbedder...
[gpu-worker] INFO: TextEmbedder ready
[gpu-worker] INFO: GPU worker idle 905s and system not in use; killing subprocess
```

Document processor log lines show subprocess lifecycle:

```
[doc_id] Spawning pdf_worker: python -m ai_researcher.pdf_worker ...
[doc_id] Spawning relation_worker: python -m ai_researcher.relation_worker ...
```

Monitor from the host:

```bash
# Real-time GPU memory usage
watch -n 1 nvidia-smi

# Inside Docker container
nerdctl exec axiom-backend nvidia-smi
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `AXIOM_GPU_WORKER_IDLE_SEC` | `900` | Floor idle time (seconds) before the GPU worker subprocess is killed. Activity detector must also report idle. |
| `ENABLE_KNOWLEDGE_GRAPH` | `true` | When `false`, skips GLiNER and mREBEL entirely. |
| `DEVICE_EMBEDDER` | `auto` | Device override for the embedder (`auto`, `cuda`, `cpu`, `mps`). |
| `DEVICE_RERANKER` | `auto` | Device override for the reranker. |
| `DEVICE_GLINER` | `auto` | Device override for GLiNER. |
| `DEVICE_MREBEL` | `auto` | Device override for mREBEL. |
| `DEVICE_MARKER` | `auto` | Device override for Marker. |

## Next Steps

- [GPU Worker Architecture](gpu-worker.md) - Subprocess design and RPC protocol
- [GPU Worker Operations](gpu-worker-operations.md) - Monitoring, troubleshooting, force unload
- [RAG and Knowledge Graph](../user-guide/documents/rag-knowledge-graph.md) - How the knowledge graph enhances retrieval
- [Architecture Overview](index.md) - System design and components