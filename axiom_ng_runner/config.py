"""Processor runner configuration (env-driven, loopback-first).

All env vars use the ``AXIOM_PROCESSOR_`` prefix (work order §11).
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Settings:
    bind_addr: str = "127.0.0.1"
    port: int = 8537
    work_root: Path = Path("/tmp/axiom_processor_work")
    allowed_source_roots: tuple[str, ...] = ()
    max_concurrent_jobs: int = 1
    result_retention_seconds: float = 3600.0  # 1h default for restart recovery
    compute_backend: str = "reference"  # "reference" | "real"
    log_level: str = "INFO"

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
        result_retention_seconds=_env_float("AXIOM_PROCESSOR_RESULT_RETENTION", 3600.0),
        compute_backend=os.getenv("AXIOM_PROCESSOR_COMPUTE", "reference"),
        log_level=os.getenv("AXIOM_PROCESSOR_LOG_LEVEL", "INFO"),
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
