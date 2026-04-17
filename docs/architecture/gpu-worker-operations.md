# GPU Worker Operations

Monitoring, troubleshooting, and operational procedures for the GPU worker subprocess.

## Checking Worker Status

### API Endpoint

The backend exposes GPU worker health at `POST /api/system/gpu/unload` (admin-only, also returns status). For a read-only check, query the worker health through the backend logs or use the container exec method below.

### Container Exec

```bash
# Check if the worker process is running
nerdctl exec axiom-backend pgrep -f "gpu_worker.server"

# Get worker health via Python one-liner
nerdctl exec axiom-backend python3 -c "
from ai_researcher.core_rag.gpu_worker_facades import worker_health
import json
print(json.dumps(worker_health(), indent=2))
"
```

A healthy response looks like:

```python
{
  "alive": True,
  "pid": 42,
  "uptime_sec": 3421.2,
  "loaded": {
    "embedder": True,
    "reranker": True,
    "gliner": False
  },
  "vram_mb": 2847.3
}
```

When the worker is idle-killed or has not been spawned yet:

```python
{
  "alive": False,
  "loaded": {"embedder": False, "reranker": False, "gliner": False},
  "vram_mb": 0
}
```

### Monitoring VRAM

```bash
# Real-time GPU memory from the host
watch -n 1 nvidia-smi

# Inside the backend container
nerdctl exec axiom-backend nvidia-smi
```

Worker log lines indicate model loading and unloading:

```
2026-04-15 14:22:01 [gpu-worker] INFO: Loading TextEmbedder...
2026-04-15 14:22:08 [gpu-worker] INFO: TextEmbedder ready
2026-04-15 14:22:08 [gpu-worker] INFO: Loading TextReranker...
2026-04-15 14:22:11 [gpu-worker] INFO: TextReranker ready
```

Idle kill appears as:

```
2026-04-15 14:37:12 [gpu-worker] INFO: GPU worker idle 905s and system not in use; killing subprocess
2026-04-15 14:37:12 [gpu-worker] INFO: Sending SIGTERM to GPU worker (pid=42)
2026-04-15 14:37:12 [gpu-worker] INFO: GPU worker shutting down
2026-04-15 14:37:13 [gpu-worker] INFO: GPU worker exited cleanly
```

## Force Unload via API

To immediately free all GPU memory without restarting the backend:

```bash
curl -X POST http://localhost:8000/api/system/gpu/unload \
  -H "Cookie: session=<your-admin-session-cookie>"
```

This calls `model_cache.unload_all()`, which sends `SIGTERM` to the worker subprocess. The response includes before/after VRAM state:

```python
{
  "status": "ok",
  "loaded_before": "worker: embedder, reranker (2847.3 MB)",
  "loaded_after": "worker: none",
  "system_was_in_use": false,
  "activity_reason": ""
}
```

The next GPU request (search, chat, document import) will transparently respawn the worker.

## Troubleshooting

### Socket Not Found

**Error:** `FileNotFoundError: /tmp/axiom-gpu/axiom-gpu.sock` or `GPU worker socket not found`

**Cause:** The doc-processor cannot reach the backend's GPU worker socket. This happens when:

- The shared volume mount is missing from one of the containers
- The backend has not received any GPU request yet (worker not spawned)
- The worker crashed and the socket file was cleaned up

**Solution:**

1. Verify the shared volume mount exists in both containers:

    ```bash
    # Check the socket directory is mounted
    nerdctl exec axiom-backend ls -la /tmp/axiom-gpu/
    nerdctl exec axiom-doc-processor ls -la /tmp/axiom-gpu/
    ```

2. Trigger a worker spawn by making any search or chat request, or run:

    ```bash
    nerdctl exec axiom-backend python3 -c "
    from ai_researcher.gpu_worker.client import get_client
    print(get_client().health())
    "
    ```

3. If the volume mount is missing, update `start-pod.sh` to include `-v /tmp/axiom-gpu:/tmp/axiom-gpu` on both containers.

!!! info "Self-Heal Behavior"
    The doc-processor waits 5 seconds (`AXIOM_GPU_WORKER_CLIENT_WAIT_SEC`) for the socket to appear. If it does not, the doc-processor falls back to spawning its own worker as a self-heal. This prevents stuck imports when the backend has not started yet.

### Worker Crashes on Startup

**Error:** `GPU worker exited during startup (rc=1)`

**Cause:** The worker subprocess failed before it could bind the socket. Common reasons:

- Missing Python dependencies (`msgpack`, `msgpack-numpy`)
- CUDA driver mismatch
- Corrupted model cache

**Solution:**

1. Check the backend container logs for the `[gpu-worker]` prefix:

    ```bash
    nerdctl logs axiom-backend 2>&1 | grep gpu-worker
    ```

2. Verify CUDA is accessible:

    ```bash
    nerdctl exec axiom-backend python3 -c "import torch; print(torch.cuda.is_available())"
    ```

3. If models are corrupted, delete the cache and restart:

    ```bash
    rm -rf ~/ai-models/hub/models--BAAI--bge-m3/
    rm -rf ~/ai-models/hub/models--BAAI--bge-reranker-v2-m3/
    # Set HF_HUB_OFFLINE=0 temporarily and restart to re-download
    ```

### RPC Timeout

**Error:** `GPU worker RPC embed_query failed after retry: ...`

**Cause:** The worker is alive but not responding within the timeout. This can happen when:

- A large batch of chunks is being embedded (600-second timeout for `embed_chunks`)
- The GPU is fully occupied by another model (Marker, mREBEL)
- Thread pool exhaustion (all 4 RPC threads busy)

**Solution:**

1. Check if a document import is running. Embedding large documents (1000+ chunks) can take several minutes.
2. Monitor GPU utilization:

    ```bash
    nvidia-smi
    ```

3. If the worker is stuck, force-kill it. It will respawn on the next request:

    ```bash
    nerdctl exec axiom-backend pkill -f "gpu_worker.server"
    ```

### Broken Pipe During RPC

**Error:** `BrokenPipeError` or `ConnectionResetError` in logs

**Cause:** The worker died mid-request (OOM kill, CUDA error, or idle timeout race).

**Behavior:** The client automatically retries once. On the retry, it kills the dead worker and spawns a fresh one. If the retry also fails, the error propagates to the caller.

No manual intervention is needed for transient broken pipes. If they persist, check `dmesg` or `journalctl` for OOM killer activity:

```bash
dmesg | grep -i oom
```

### Import Fails with mREBEL OOM

**Error:** `relation_worker exited with code 1` and CUDA out-of-memory in stderr

**Cause:** mREBEL requires approximately 2.4 GB of VRAM. If the GPU worker is still resident (holding embedder + reranker), there may not be enough free VRAM.

**Solution:**

The document processor normally calls `model_cache.unload_all()` before spawning the relation worker. If this is not happening:

1. Force-unload the GPU worker:

    ```bash
    curl -X POST http://localhost:8000/api/system/gpu/unload \
      -H "Cookie: session=<admin-cookie>"
    ```

2. Re-trigger the document import from the UI.

!!! info "Non-Fatal by Design"
    mREBEL failure is non-fatal. The document will complete processing without relation extraction. Entity extraction (GLiNER) and embeddings are unaffected.

## Common Error Messages

| Error | Cause | Resolution |
|---|---|---|
| `connection closed after 0/4 bytes` | Worker exited between connect and response | Automatic retry handles this. Check logs if persistent. |
| `unknown method: foo` | Client sent an RPC method the worker does not implement | Update worker server to add the handler, or fix the caller. |
| `response id mismatch` | Protocol corruption (extremely rare) | Kill and respawn the worker. |
| `GPU worker failed to bind socket in time` | Worker startup took longer than 60 seconds | Increase `AXIOM_GPU_WORKER_SPAWN_TIMEOUT_SEC` or check disk I/O. |

## Next Steps

- [GPU Worker Architecture](gpu-worker.md) - How the subprocess pattern works
- [VRAM Management](vram-management.md) - GPU memory budget and per-model sizing
- [Document Processing Pipeline](document-pipeline.md) - Full pipeline walkthrough
- [Production Deployment](../deployment/lxc-podman-prod.md) - Container configuration