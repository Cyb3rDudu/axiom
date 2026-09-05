"""Processor runner configuration (env-driven, loopback-first).

All env vars use the ``AXIOM_PROCESSOR_`` prefix (work order §11).
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


# Single source of truth for the source-download budget (#250): the
# dataclass default AND the env-loader fallback MUST share this value —
# a diverging pair shipped 120s as the real budget while the diff
# claimed 600s (found in production 2026-09-05).
SOURCE_DOWNLOAD_TIMEOUT_DEFAULT = 600.0

@dataclass(frozen=True)
class Settings:
    bind_addr: str = "127.0.0.1"
    port: int = 8537
    work_root: Path = Path("/tmp/axiom_processor_work")
    allowed_source_roots: tuple[str, ...] = ()
    max_concurrent_jobs: int = 1
    # #243: bounded FIFO admission queue — how many jobs may wait for a
    # compute slot before a new POST is rejected with 429. 0 = accept only up
    # to max_concurrent_jobs (no waiting). A real bound must exist so the
    # runner never silently spawns unbounded work.
    admission_queue_capacity: int = 8
    result_retention_seconds: float = 3600.0  # 1h default for restart recovery
    compute_backend: str = "reference"  # "reference" | "real"
    log_level: str = "INFO"
    # Remote source delivery: total download budget for one source_url pull.
    # #250: 600s default — a large book under full local compute must
    # transfer, not race the clock (the budget spans all internal retries).
    source_download_timeout: float = SOURCE_DOWNLOAD_TIMEOUT_DEFAULT
    # Query endpoints (epic #130): hard server caps; /v1/embed max_texts may
    # only lower these, never raise them.
    max_query_texts: int = 16
    rerank_max_texts: int = 64
    # #216 cold-start warmup: preload the query models (BGE-M3 + reranker) at
    # server startup so the FIRST real embed/rerank request is already warm
    # instead of paying the ~90s MPS model load on query one. On by default.
    warmup: bool = True
    # #220: external epubcheck command for the EPUB preflight gate. Empty
    # = auto-detect `epubcheck` on PATH; the light built-in checks run
    # regardless (epubcheck reported as not_available when absent).
    epubcheck_cmd: str = ""
    # #225: wall-clock budget for the relationships stage (seconds). mREBEL
    # over a full book is inherently slow on MPS (#179); exceeding the
    # budget ends the stage HONESTLY (partial result, named reason) instead
    # of an eternal lease. 0 disables the budget.
    relationships_budget_seconds: float = 900.0
    # #230: wall-clock budget for the image_captions stage (seconds) and a
    # per-image timeout. A hung model call costs minutes, never the
    # pipeline; exceeding the budget ends the stage HONESTLY (uncaptioned
    # images stay empty — no placeholders, #241 discipline). 0 disables.
    image_captions_budget_seconds: float = 900.0
    image_caption_timeout_seconds: float = 60.0

    @property
    def bind(self) -> tuple[str, int]:
        return self.bind_addr, self.port


def _env_int(name: str, default: int) -> int:
    try:
        return int(os.getenv(name, default))
    except ValueError:
        return default


def _env_float(name: str, default: float) -> float:
    try:
        return float(os.getenv(name, default))
    except ValueError:
        return default


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


def _env_roots(name: str) -> tuple[str, ...]:
    raw = os.getenv(name, "")
    if not raw:
        return ()
    return tuple(p for p in raw.split(os.pathsep) if p)


def load_settings() -> Settings:
    return Settings(
        bind_addr=os.getenv("AXIOM_PROCESSOR_BIND_ADDR", "127.0.0.1"),
        port=_env_int("AXIOM_PROCESSOR_PORT", 8537),
        work_root=Path(
            os.getenv("AXIOM_PROCESSOR_WORK_ROOT", "/tmp/axiom_processor_work")
        ),
        allowed_source_roots=_env_roots("AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS"),
        max_concurrent_jobs=max(1, _env_int("AXIOM_PROCESSOR_MAX_CONCURRENT_JOBS", 1)),
        admission_queue_capacity=max(
            0, _env_int("AXIOM_PROCESSOR_ADMISSION_QUEUE", 8)
        ),
        result_retention_seconds=_env_float("AXIOM_PROCESSOR_RESULT_RETENTION", 3600.0),
        compute_backend=os.getenv("AXIOM_PROCESSOR_COMPUTE", "reference"),
        log_level=os.getenv("AXIOM_PROCESSOR_LOG_LEVEL", "INFO"),
        source_download_timeout=_env_float(
            "AXIOM_PROCESSOR_SOURCE_TIMEOUT", SOURCE_DOWNLOAD_TIMEOUT_DEFAULT
        ),
        max_query_texts=max(1, _env_int("AXIOM_PROCESSOR_MAX_QUERY_TEXTS", 16)),
        rerank_max_texts=max(1, _env_int("AXIOM_PROCESSOR_RERANK_MAX_TEXTS", 64)),
        warmup=_env_bool("AXIOM_PROCESSOR_WARMUP", True),
        epubcheck_cmd=os.getenv("AXIOM_PROCESSOR_EPUBCHECK_CMD", ""),
        relationships_budget_seconds=_env_float(
            "AXIOM_PROCESSOR_RELATIONSHIPS_BUDGET_SECONDS", 900.0
        ),
        image_captions_budget_seconds=_env_float(
            "AXIOM_PROCESSOR_IMAGE_CAPTIONS_BUDGET_SECONDS", 900.0
        ),
        image_caption_timeout_seconds=_env_float(
            "AXIOM_PROCESSOR_IMAGE_CAPTION_TIMEOUT_SECONDS", 60.0
        ),
    )


class Overrides:
    """Mutable bag selecting the active Settings so tests can point at one
    config without smuggling settings through every constructor."""

    def __init__(self) -> None:
        self._current = load_settings()

    def set(self, s: Settings) -> None:
        self._current = s

    def get(self) -> Settings:
        return self._current


settings = Overrides()
