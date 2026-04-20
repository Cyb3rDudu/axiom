"""GPU worker subprocess entry point.

Loads models lazily on first request, serves RPC over a Unix socket.
Intended to run as a child of the main backend/doc-processor process.
On SIGTERM: finishes in-flight requests, closes socket, exits.
"""

import logging
import os
import signal
import socket
import sys
import threading
import time
import traceback
import uuid
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Dict, Optional


def _reattach_stdio_to_container_init() -> None:
    """Reopen stdout/stderr against PID 1's fds.

    Rationale: when spawned by a short-lived parent (e.g. an ``nerdctl exec``
    debugging session or an interactive script), the worker inherits that
    parent's stdout/stderr pipes. When the parent exits, those pipes close,
    and the next write by the worker (model load progress, logging)
    triggers ``BrokenPipeError`` — which bubbles up to RPC callers as
    "[Errno 32] Broken pipe" and mysteriously crashes otherwise-healthy
    handlers (caught during #9 single-worker rollout).

    Inside a PID-namespaced container, PID 1 is the container's init
    (uvicorn for us) and ``/proc/1/fd/{1,2}`` always points at the
    containerd log pipe. Reopening our own fds 1/2 there makes worker
    stdio survive the death of whoever spawned it.

    No-op outside a container or when /proc isn't mounted.
    """
    for fd in (1, 2):
        try:
            new = os.open(f"/proc/1/fd/{fd}", os.O_WRONLY)
        except OSError:
            continue
        try:
            os.dup2(new, fd)
        finally:
            os.close(new)
    # Reinstall Python-level wrappers so print() and logging see the new fds.
    try:
        sys.stdout = os.fdopen(1, "w", buffering=1, closefd=False)
        sys.stderr = os.fdopen(2, "w", buffering=1, closefd=False)
    except OSError:
        pass


_reattach_stdio_to_container_init()

# Configure logging AFTER stdio reattachment so the StreamHandler binds to
# the stable fds, not the spawner's doomed pipes.
logging.basicConfig(
    level=os.getenv("LOG_LEVEL", "INFO"),
    format="%(asctime)s [gpu-worker] %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger(__name__)

from ai_researcher.gpu_worker.protocol import (
    ProtocolError,
    make_response_error,
    make_response_ok,
    recv_frame,
    send_frame,
)


def _vram_allocated_mb() -> Optional[float]:
    try:
        import torch
        if torch.cuda.is_available():
            return round(torch.cuda.memory_allocated() / 1e6, 1)
    except Exception:
        pass
    return None


class GpuWorkerServer:
    """Loads GPU models on demand and dispatches RPC calls."""

    MAX_WORKERS = int(os.getenv("AXIOM_GPU_WORKER_THREADS", "4"))

    def __init__(self) -> None:
        self._embedder: Optional[Any] = None
        self._reranker: Optional[Any] = None
        self._gliner: Optional[Any] = None
        self._load_lock = threading.Lock()
        self._shutdown = threading.Event()
        self._start_time = time.time()

    # ── Lazy model loaders ────────────────────────────────────────────

    def _get_embedder(self):
        if self._embedder is None:
            with self._load_lock:
                if self._embedder is None:
                    from ai_researcher.core_rag.embedder import TextEmbedder
                    logger.info("Loading TextEmbedder...")
                    self._embedder = TextEmbedder()
                    logger.info("TextEmbedder ready")
        return self._embedder

    def _get_reranker(self):
        if self._reranker is None:
            with self._load_lock:
                if self._reranker is None:
                    from ai_researcher.core_rag.reranker import TextReranker
                    logger.info("Loading TextReranker...")
                    self._reranker = TextReranker()
                    logger.info("TextReranker ready")
        return self._reranker

    def _get_gliner(self):
        if self._gliner is None:
            with self._load_lock:
                if self._gliner is None:
                    try:
                        import os as _os
                        from gliner import GLiNER
                        # hardware_detection lives at axiom_backend/ai_researcher/hardware_detection.py
                        # Use the full package path so the import works in a fresh
                        # subprocess where the legacy sys.path-polluting imports
                        # (embedder/reranker/processor) haven't run yet.
                        from ai_researcher.hardware_detection import hardware_detector

                        cache_dir = _os.getenv("HF_HOME", "/root/.cache/huggingface/hub")
                        device = hardware_detector.get_model_device("gliner")
                        logger.info(f"Loading GLiNER on {device}...")
                        self._gliner = GLiNER.from_pretrained(
                            "urchade/gliner_multi-v2.1",
                            cache_dir=cache_dir,
                        ).to(device)
                        logger.info("GLiNER ready")
                    except ImportError as exc:
                        logger.warning(f"GLiNER not available: {exc}")
                        return None
        return self._gliner

    # ── RPC handlers ──────────────────────────────────────────────────

    def handle_health(self, **_ignored) -> dict:
        return {
            "pid": os.getpid(),
            "uptime_sec": round(time.time() - self._start_time, 1),
            "loaded": {
                "embedder": self._embedder is not None,
                "reranker": self._reranker is not None,
                "gliner": self._gliner is not None,
            },
            "vram_mb": _vram_allocated_mb(),
        }

    def handle_embed_query(self, text: str) -> Any:
        return self._get_embedder().embed_query(text)

    def handle_embed_chunks(self, chunks: list) -> list:
        return self._get_embedder().embed_chunks(chunks)

    def handle_rerank(self, query: str, items: list, top_n: Optional[int] = None) -> list:
        """Return a list of [score, original_index] pairs sorted by score desc.

        We deliberately do NOT return the original items — caller has them
        and can map indexes back. Keeps serialization small and avoids
        Pydantic round-trip issues.
        """
        reranker = self._get_reranker()
        # Reranker accepts dicts with 'text' or objects with 'content'; we
        # build wrappers that carry the original index as an attribute.
        wrapped = []
        for idx, item in enumerate(items):
            if isinstance(item, dict):
                text_val = item.get("text") or item.get("content") or str(item)
            else:
                text_val = str(item)
            wrapped.append({"_idx": idx, "text": text_val})
        scored = reranker.rerank(query, wrapped, top_n=top_n)
        return [[float(score), wrapped_item["_idx"]] for score, wrapped_item in scored]

    def handle_extract_entities(
        self,
        text: str,
        labels: list,
        threshold: float = 0.45,
        multi_label: bool = True,
    ) -> list:
        model = self._get_gliner()
        if model is None:
            return []
        return model.predict_entities(text, labels, threshold=threshold, multi_label=multi_label)

    def handle_shutdown(self, **_ignored) -> dict:
        """Graceful shutdown request from client."""
        logger.info("Shutdown requested via RPC")
        self._shutdown.set()
        return {"ok": True}

    def handle_unload_models(self, **_ignored) -> dict:
        """Drop all loaded models and release their VRAM.

        Callers use this to reclaim GPU memory before spawning a peer
        subprocess (e.g. the Marker pdf_worker) that needs several GB of
        contiguous VRAM on a shared card. The models are lazily reloaded on
        the next RPC that needs them, so this is cheap to call — one cold
        reload (~5–10 s) beats an OOM crash.
        """
        import gc

        before_mb = _vram_allocated_mb()
        with self._load_lock:
            had = {
                "embedder": self._embedder is not None,
                "reranker": self._reranker is not None,
                "gliner": self._gliner is not None,
            }
            self._embedder = None
            self._reranker = None
            self._gliner = None
        gc.collect()
        try:
            import torch
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
                torch.cuda.ipc_collect()
        except Exception as exc:
            logger.warning(f"torch cache clear failed: {exc}")
        after_mb = _vram_allocated_mb()
        logger.info(
            f"unload_models: dropped {sum(had.values())} models "
            f"(vram {before_mb}→{after_mb} MB)"
        )
        return {"unloaded": had, "vram_before_mb": before_mb, "vram_after_mb": after_mb}

    # ── Dispatch ──────────────────────────────────────────────────────

    _HANDLERS = {
        "health": "handle_health",
        "embed_query": "handle_embed_query",
        "embed_chunks": "handle_embed_chunks",
        "rerank": "handle_rerank",
        "extract_entities": "handle_extract_entities",
        "unload_models": "handle_unload_models",
        "shutdown": "handle_shutdown",
    }

    def _dispatch(self, method: str, args: Dict[str, Any]) -> Any:
        handler_name = self._HANDLERS.get(method)
        if handler_name is None:
            raise ValueError(f"unknown method: {method}")
        handler = getattr(self, handler_name)
        return handler(**args)

    def _handle_connection(self, conn: socket.socket) -> None:
        try:
            while not self._shutdown.is_set():
                try:
                    req = recv_frame(conn)
                except ProtocolError:
                    return  # client closed
                req_id = req.get("id") or str(uuid.uuid4())
                method = req.get("method", "")
                args = req.get("args") or {}
                try:
                    result = self._dispatch(method, args)
                    send_frame(conn, make_response_ok(req_id, result))
                except Exception as exc:
                    logger.error(f"RPC {method} failed: {exc}", exc_info=True)
                    send_frame(
                        conn,
                        make_response_error(req_id, str(exc), traceback.format_exc()),
                    )
        finally:
            try:
                conn.close()
            except Exception:
                pass

    # ── Server loop ───────────────────────────────────────────────────

    def run(self, socket_path: str) -> None:
        # Install signal handlers before binding.
        signal.signal(signal.SIGTERM, lambda *_: self._shutdown.set())
        signal.signal(signal.SIGINT, lambda *_: self._shutdown.set())

        # Clean up stale socket if present.
        try:
            os.unlink(socket_path)
        except FileNotFoundError:
            pass

        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(socket_path)
        srv.listen(16)
        srv.settimeout(1.0)  # so we can check _shutdown regularly
        os.chmod(socket_path, 0o660)  # group-readable for shared-volume access

        logger.info(f"GPU worker listening on {socket_path} (pid={os.getpid()})")

        pool = ThreadPoolExecutor(max_workers=self.MAX_WORKERS, thread_name_prefix="gpu-rpc")
        try:
            while not self._shutdown.is_set():
                try:
                    conn, _ = srv.accept()
                except socket.timeout:
                    continue
                pool.submit(self._handle_connection, conn)
        finally:
            logger.info("GPU worker shutting down")
            try:
                srv.close()
            except Exception:
                pass
            try:
                os.unlink(socket_path)
            except FileNotFoundError:
                pass
            pool.shutdown(wait=True, cancel_futures=False)
            logger.info("GPU worker exited cleanly")


def main() -> None:
    if len(sys.argv) < 2:
        print("usage: python -m ai_researcher.gpu_worker.server <socket_path>", file=sys.stderr)
        sys.exit(2)
    socket_path = sys.argv[1]
    GpuWorkerServer().run(socket_path)


if __name__ == "__main__":
    main()
