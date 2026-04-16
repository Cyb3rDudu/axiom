"""Client for the GPU worker subprocess.

Singleton that:
  - spawns the worker on first call (unless `client_mode=True`, which only
    connects to an externally-managed worker — used by doc-processor)
  - opens a fresh Unix socket connection per call (stateless, simple)
  - monitors idle time and kills the worker subprocess after a threshold
  - transparently respawns on next call

Thread-safe: `_call()` is concurrent-safe. The subprocess itself uses a
ThreadPoolExecutor to serve concurrent clients.
"""

import logging
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import uuid
from typing import Any, Optional

from ai_researcher.gpu_worker.protocol import (
    ProtocolError,
    make_request,
    recv_frame,
    send_frame,
)

logger = logging.getLogger(__name__)

DEFAULT_SOCKET_DIR = os.getenv("AXIOM_GPU_WORKER_SOCKET_DIR", "/tmp")
DEFAULT_IDLE_SEC = int(os.getenv("AXIOM_GPU_WORKER_IDLE_SEC", "900"))
SPAWN_WAIT_TIMEOUT = int(os.getenv("AXIOM_GPU_WORKER_SPAWN_TIMEOUT_SEC", "60"))


class GpuWorkerError(Exception):
    """Raised when the worker returns an error response."""


class GpuWorkerClient:
    """Singleton lifecycle manager for the GPU worker subprocess."""

    _instance: Optional["GpuWorkerClient"] = None
    _instance_lock = threading.Lock()

    @classmethod
    def instance(cls) -> "GpuWorkerClient":
        """Build the singleton using environment-derived defaults.

        - ``AXIOM_GPU_WORKER_SOCKET`` — socket path shared by both the owner
          (backend) and the connect-only client (doc-processor). When set,
          both containers agree on where the socket lives.
        - ``AXIOM_GPU_WORKER_CLIENT_MODE=true`` — tell this process *not* to
          spawn a worker itself; just connect to the one the owner created.
          doc-processor sets this so backend alone owns the subprocess.
        """
        with cls._instance_lock:
            if cls._instance is None:
                env_client_mode = (
                    os.getenv("AXIOM_GPU_WORKER_CLIENT_MODE", "false").lower() == "true"
                )
                cls._instance = cls(client_mode=env_client_mode)
        return cls._instance

    def __init__(
        self,
        socket_path: Optional[str] = None,
        client_mode: bool = False,
        idle_sec: Optional[int] = None,
    ) -> None:
        """
        Parameters
        ----------
        socket_path : Optional[str]
            Override socket path. Otherwise the ``AXIOM_GPU_WORKER_SOCKET``
            env var is honored; falls back to ``/tmp/axiom-gpu-{pid}.sock``
            in owner mode, or ``/tmp/axiom-gpu.sock`` in client mode.
        client_mode : bool
            If True, never spawn the worker — only connect to an existing
            socket. Used by doc-processor which shares the backend's worker.
        idle_sec : Optional[int]
            Override idle threshold. Default: AXIOM_GPU_WORKER_IDLE_SEC env.
        """
        self._client_mode = client_mode
        self._idle_sec = idle_sec if idle_sec is not None else DEFAULT_IDLE_SEC

        env_socket = os.getenv("AXIOM_GPU_WORKER_SOCKET")
        if socket_path:
            self._socket_path = socket_path
        elif env_socket:
            # Explicit shared path — both owner and client read it. Used in
            # LXC prod where backend + doc-processor mount the same dir.
            self._socket_path = env_socket
        elif client_mode:
            self._socket_path = os.path.join(DEFAULT_SOCKET_DIR, "axiom-gpu.sock")
        else:
            self._socket_path = os.path.join(DEFAULT_SOCKET_DIR, f"axiom-gpu-{os.getpid()}.sock")

        self._proc: Optional[subprocess.Popen] = None
        self._proc_lock = threading.Lock()
        self._last_used_at: float = 0.0
        self._idle_monitor_started = False

    # ── Lifecycle ─────────────────────────────────────────────────────

    def _worker_alive(self) -> bool:
        return self._proc is not None and self._proc.poll() is None

    def _ensure_worker(self) -> None:
        """Spawn the worker if not running. Client-mode: only verify socket exists."""
        if self._client_mode:
            # Doc-processor case: backend owns the worker. Wait for socket.
            deadline = time.time() + SPAWN_WAIT_TIMEOUT
            while not os.path.exists(self._socket_path):
                if time.time() > deadline:
                    raise GpuWorkerError(
                        f"GPU worker socket not found at {self._socket_path} "
                        f"(client_mode=True — the backend should own the worker)"
                    )
                time.sleep(0.5)
            return

        with self._proc_lock:
            if self._worker_alive():
                return

            # Ensure the parent directory exists (matters when the socket
            # lives under a shared bind-mount like /tmp/axiom-gpu/ that was
            # created fresh by the container start script).
            sock_dir = os.path.dirname(self._socket_path)
            if sock_dir:
                os.makedirs(sock_dir, exist_ok=True)

            # Clean any stale socket from a previous run.
            try:
                os.unlink(self._socket_path)
            except FileNotFoundError:
                pass

            logger.info(f"Spawning GPU worker subprocess -> {self._socket_path}")
            self._proc = subprocess.Popen(
                [
                    sys.executable,
                    "-m",
                    "ai_researcher.gpu_worker.server",
                    self._socket_path,
                ],
                env=os.environ.copy(),
                # Inherit stdout/stderr so worker logs land in container logs.
            )

            # Wait for the socket to appear.
            deadline = time.time() + SPAWN_WAIT_TIMEOUT
            while not os.path.exists(self._socket_path):
                if self._proc.poll() is not None:
                    raise GpuWorkerError(
                        f"GPU worker exited during startup (rc={self._proc.returncode})"
                    )
                if time.time() > deadline:
                    self._proc.kill()
                    raise GpuWorkerError("GPU worker failed to bind socket in time")
                time.sleep(0.2)
            logger.info(f"GPU worker ready (pid={self._proc.pid})")

        # Lazy-start the idle monitor only once we've spawned at least once.
        self._ensure_idle_monitor()

    def _ensure_idle_monitor(self) -> None:
        if self._idle_monitor_started or self._client_mode:
            return
        self._idle_monitor_started = True

        def monitor() -> None:
            logger.info(
                f"GPU worker idle monitor started (threshold={self._idle_sec}s)"
            )
            while True:
                time.sleep(60)
                try:
                    if not self._worker_alive():
                        continue
                    idle = time.time() - self._last_used_at
                    if idle < self._idle_sec:
                        continue
                    # Defer to the system activity detector (covers missions,
                    # doc pipeline, recent API calls) before killing.
                    try:
                        from services.activity_detector import is_system_in_use
                        in_use, reason = is_system_in_use(
                            max_request_idle_sec=self._idle_sec
                        )
                        if in_use:
                            continue
                    except Exception:
                        pass
                    logger.info(
                        f"GPU worker idle {idle:.0f}s and system not in use; "
                        f"killing subprocess"
                    )
                    self.shutdown_worker()
                except Exception as exc:
                    logger.error(f"Idle monitor error: {exc}", exc_info=True)

        t = threading.Thread(target=monitor, daemon=True, name="gpu-worker-idle")
        t.start()

    def shutdown_worker(self, timeout: int = 15) -> None:
        """Terminate the worker subprocess. No-op in client_mode or if not owned."""
        if self._client_mode:
            return
        with self._proc_lock:
            if not self._worker_alive():
                return
            assert self._proc is not None
            logger.info(f"Sending SIGTERM to GPU worker (pid={self._proc.pid})")
            self._proc.terminate()
            try:
                self._proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                logger.warning("Worker did not exit in time — killing")
                self._proc.kill()
                self._proc.wait(timeout=5)
            self._proc = None
            try:
                os.unlink(self._socket_path)
            except FileNotFoundError:
                pass

    # ── RPC ───────────────────────────────────────────────────────────

    def _call(self, method: str, timeout: int = 60, **kwargs) -> Any:
        """Send an RPC call with one retry on broken pipe."""
        last_exc: Optional[Exception] = None
        for attempt in range(2):
            try:
                self._ensure_worker()
                return self._send_request(method, kwargs, timeout=timeout)
            except (BrokenPipeError, ConnectionResetError, ProtocolError, FileNotFoundError) as exc:
                logger.warning(f"GPU worker RPC {method} failed ({exc!r}); retrying once")
                last_exc = exc
                # Force respawn on retry — treat the worker as dead.
                if not self._client_mode:
                    with self._proc_lock:
                        if self._proc and self._proc.poll() is None:
                            self._proc.kill()
                            self._proc = None
                continue
        raise GpuWorkerError(f"GPU worker RPC {method} failed after retry: {last_exc}")

    def _send_request(self, method: str, args: dict, timeout: int) -> Any:
        req_id = uuid.uuid4().hex
        payload = make_request(method, args, req_id)
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(timeout)
            s.connect(self._socket_path)
            send_frame(s, payload)
            resp = recv_frame(s)
        if resp.get("id") != req_id:
            raise GpuWorkerError(f"response id mismatch: {resp.get('id')} != {req_id}")
        if not resp.get("ok"):
            raise GpuWorkerError(resp.get("error", "unknown error"))
        self._last_used_at = time.time()
        return resp.get("result")

    # ── Public API (mirrors the TextEmbedder/TextReranker/GLiNER APIs) ──

    def embed_query(self, text: str, timeout: int = 60) -> Any:
        return self._call("embed_query", text=text, timeout=timeout)

    def embed_chunks(self, chunks: list, timeout: int = 600) -> list:
        return self._call("embed_chunks", chunks=chunks, timeout=timeout)

    def rerank(
        self, query: str, items: list, top_n: Optional[int] = None, timeout: int = 60
    ) -> list:
        """Returns list of [score, original_index] pairs."""
        return self._call("rerank", query=query, items=items, top_n=top_n, timeout=timeout)

    def extract_entities(
        self,
        text: str,
        labels: list,
        threshold: float = 0.45,
        multi_label: bool = True,
        timeout: int = 60,
    ) -> list:
        return self._call(
            "extract_entities",
            text=text,
            labels=labels,
            threshold=threshold,
            multi_label=multi_label,
            timeout=timeout,
        )

    def health(self, timeout: int = 10) -> dict:
        return self._call("health", timeout=timeout)


# Convenience module-level accessor (don't instantiate at import time —
# wait until first use to avoid spawning the worker during test collection).
def get_client() -> GpuWorkerClient:
    return GpuWorkerClient.instance()
