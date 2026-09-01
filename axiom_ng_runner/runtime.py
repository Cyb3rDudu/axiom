"""Per-job runtime + bounded admission scheduler for the processor runner.

Shared layer serving both #242 (cancellation reaches the real subprocess) and
#243 (bounded admission queue + retention wiring). One ``JobRuntime`` exists
per accepted job and owns the linkage between the compute worker and the real
child processes (the conversion Popen trees). The ``Scheduler`` is the bounded
FIFO admission queue between the HTTP endpoints and the workers.

Design goals (work order I2):

* Cancel must reach the OS level: a cancelled job stops costing CPU/GPU/disk
  within seconds, not when a cooperative in-process poll happens to notice.
  ``JobRuntime.cancel`` terminates the registered subprocess tree
  (SIGTERM -> bounded reap window -> SIGKILL) on a daemon reaper thread so the
  HTTP handler never blocks on a slow reap.
* ``max_concurrent_jobs`` is enforced at spawn/pull time by a fixed worker
  pool; queued jobs do NOT each spawn a thread. A full bounded queue rejects
  the submit (caller maps it to an explicit 429/503).
* Thread-spawn is bounded: exactly ``max_concurrent_jobs`` daemon workers ever
  exist, independent of how many jobs are queued.
"""

from __future__ import annotations

import logging
import os
import queue
import signal
import threading
import time
from collections.abc import Callable
from contextlib import suppress

log = logging.getLogger(__name__)

# Bounded reap window before a SIGTERM'd child escalates to SIGKILL.
_CANCEL_GRACE_SECONDS = 3.0
_CANCEL_REAP_POLL = 0.05


def _terminate_process_group(proc, sig: int) -> bool:
    """Send a signal to the child's whole process group (the conversion worker
    is launched with ``start_new_session=True`` so grand-children like pandoc
    die with it). Returns False if the child already exited."""
    try:
        if proc.poll() is not None:
            return False
        os.killpg(os.getpgid(proc.pid), sig)
        return True
    except (ProcessLookupError, OSError):
        return False


class JobRuntime:
    """Per-job runtime state.

    Holds the compute thread and the current child subprocess handle so cancel
    can reach the real process tree. The in-flight compute (``runner``) calls
    ``register_child`` when it launches a conversion Popen; ``cancel`` then
    terminates that tree. Cooperative (in-process, reference) cancels work via
    the store status flip alone — that path is unchanged.
    """

    def __init__(self, job_id: str, work: Callable[[JobRuntime], None]) -> None:
        self.job_id = job_id
        self._work = work
        self._thread: threading.Thread | None = None
        self._child: object | None = None          # the live Popen (or None)
        self._done = threading.Event()
        self._cancelled = threading.Event()
        self._lock = threading.Lock()

    # --- lifecycle ------------------------------------------------------
    def start(self) -> None:
        """Start the compute in a dedicated worker thread (one per job).

        The Scheduler's worker pool calls this when a job is dequeued, so the
        thread count stays bounded by how many run concurrently."""
        self._thread = threading.Thread(
            target=self._run, name=f"compute-{self.job_id}", daemon=True
        )
        self._thread.start()

    def _run(self) -> None:
        try:
            self._work(self)
        finally:
            self._done.set()

    def is_alive(self) -> bool:
        return self._thread is not None and self._thread.is_alive()

    def join(self, timeout: float | None = None) -> None:  # pragma: no cover
        if self._thread is not None:
            self._thread.join(timeout)

    @property
    def cancelled(self) -> bool:
        return self._cancelled.is_set()

    @property
    def done(self) -> bool:
        return self._done.is_set()

    # --- child subprocess linkage (#242) --------------------------------
    def register_child(self, proc) -> None:
        """Record the live conversion Popen so a later cancel can terminate
        it. Called by the runner right after ``subprocess.Popen``."""
        with self._lock:
            self._child = proc

    def unregister_child(self, proc) -> None:
        with self._lock:
            if self._child is proc:
                self._child = None

    # --- cancel (#242) ---------------------------------------------------
    def cancel(self) -> None:
        """Cancel this job: signal the flag (cooperative path) and terminate
        any registered child process tree on a daemon reaper.

        The reap (SIGTERM -> bounded grace -> SIGKILL) runs off the HTTP path
        so the endpoint returns immediately; a cancelled job still stops within
        the grace window, not minutes later."""
        self._cancelled.set()
        with self._lock:
            child = self._child
        if child is not None:
            _reap_in_background(child)


def _reap_in_background(child) -> None:
    """SIGTERM the child's process group, wait up to the grace window, then
    SIGKILL if it survived. Runs on a daemon thread so it never blocks the
    cancel endpoint; zombies are reaped too."""
    def _reap() -> None:
        try:
            if not _terminate_process_group(child, signal.SIGTERM):
                return
            deadline = time.monotonic() + _CANCEL_GRACE_SECONDS
            while time.monotonic() < deadline:
                if child.poll() is not None:
                    return
                time.sleep(_CANCEL_REAP_POLL)
            if child.poll() is None:
                log.warning(
                    "child %s ignored SIGTERM; SIGKILL", getattr(child, "pid", "?")
                )
                _terminate_process_group(child, signal.SIGKILL)
                with suppress(Exception):              # reap is best-effort
                    child.wait(timeout=_CANCEL_GRACE_SECONDS)
        except Exception:  # cancel reap must never raise into the caller
            log.warning("cancel reap failed: %r", child, exc_info=True)

    threading.Thread(target=_reap, name="cancel-reap", daemon=True).start()


class Scheduler:
    """Bounded FIFO admission queue.

    A fixed pool of ``max_concurrent_jobs`` daemon workers pulls from a bounded
    FIFO ``queue.Queue``. ``submit`` is non-blocking: it enqueues and returns
    True, or returns False when the queue is full (the caller returns an
    explicit 429/503 with a retry hint — never a silent thread-spawn). FIFO is
    guaranteed by ``queue.Queue`` under the GIL with a mutex.
    """

    def __init__(
        self, max_concurrent: int, queue_capacity: int, work: Callable[[JobRuntime], None]
    ) -> None:
        if max_concurrent < 1:
            raise ValueError("max_concurrent must be >= 1")
        self._max = max_concurrent
        # #243: the admission bound is max_concurrent (running) + the waiting
        # capacity. An explicit ``_outstanding`` counter enforces it (never the
        # queue's maxsize, which would be UNBOUNDED for 0 in Python): capacity 0
        # means no waiting room at all — only max_concurrent_jobs are accepted.
        self._capacity = max(0, queue_capacity)
        self._queue: queue.Queue[JobRuntime] = queue.Queue()  # FIFO (bounded by _outstanding)
        self._work = work
        self._running: dict[str, JobRuntime] = {}
        self._running_lock = threading.Lock()
        self._outstanding = 0  # accepted-but-not-yet-completed jobs
        self._workers: list[threading.Thread] = []
        self._started = False

    # --- lifecycle -------------------------------------------------------
    def start(self) -> None:
        if self._started:
            return
        self._started = True
        for i in range(self._max):
            w = threading.Thread(
                target=self._loop, name=f"compute-worker-{i}", daemon=True
            )
            w.start()
            self._workers.append(w)

    def _loop(self) -> None:
        while True:
            rt: JobRuntime = self._queue.get()
            try:
                if rt.cancelled:
                    # #242: cancel survives the queue — a job cancelled while
                    # waiting must never start. The store already settled it to
                    # cancelled; silently drop it (no compute, no spawn).
                    log.info("job %s cancelled while queued; skipping compute", rt.job_id)
                    continue
                rt.start()          # run compute in its own per-job thread
                rt.join()
            finally:
                with self._running_lock:
                    self._running.pop(rt.job_id, None)
                    self._outstanding -= 1
                self._queue.task_done()

    # --- registry --------------------------------------------------------
    def get(self, job_id: str) -> JobRuntime | None:
        with self._running_lock:
            return self._running.get(job_id)

    def is_relevant(self, job_id: str) -> bool:
        """True if a job is currently tracked (queued or running) — used to
        decide whether a recovered job needs a relaunch."""
        with self._running_lock:
            return job_id in self._running

    # --- admission (#243) -------------------------------------------------
    def submit(self, rt: JobRuntime) -> bool:
        """Admit a job for compute. Returns False (bounded queue full — i.e.
        max_concurrent + capacity already accepted) with no thread spawned;
        the caller maps that to an explicit 429/503.

        The bound check and the insert-into-_running happen under one lock so
        concurrent POSTs cannot both take the last slot."""
        with self._running_lock:
            if self._outstanding >= self._max + self._capacity:
                return False
            self._outstanding += 1
            self._running[rt.job_id] = rt
            self._queue.put_nowait(rt)  # never Full: internal FIFO is unbounded
        return True

    def accepting(self) -> bool:
        """True if a new job can be admitted (fewer than max_concurrent +
        capacity jobs are outstanding). Advisory: submit() is authoritative
        and re-checks under the lock, closing the check/put race."""
        with self._running_lock:
            return self._outstanding < self._max + self._capacity
