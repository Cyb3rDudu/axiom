"""Import-smoke for the vendored compute modules — exists because a syntax
error in a moved module left the suite green (mutation-proven in review)."""

from __future__ import annotations

import importlib
import sys

import pytest

LIGHT_MODULES = [
    "chunker",
    "pdf_health",
    "pdf_processing",
    "pdf_worker",
    "epub_worker",
]
# devices/relation_extractor import torch; embedder adds numpy + FlagEmbedding.
HEAVY_MODULES = ["devices", "embedder", "relation_extractor"]


def _import(name: str):
    return importlib.import_module(f"axiom_ng_runner.compute_core.{name}")


@pytest.mark.parametrize("name", LIGHT_MODULES)
def test_light_module_imports(name):
    _import(name)


@pytest.mark.parametrize("name", HEAVY_MODULES)
def test_heavy_module_imports(name):
    pytest.importorskip("torch")
    if name == "embedder":
        pytest.importorskip("numpy")
    _import(name)


def test_light_modules_pull_no_db_drivers():
    """Source-level DB-freedom for the real compute path: importing the light
    modules must not load any DB driver (mirror of test_no_durable_store_access,
    which covers the reference compute run)."""
    before = set(sys.modules)
    for name in LIGHT_MODULES:
        _import(name)
    banned = ("sqlalchemy", "psycopg", "psycopg2", "asyncpg")
    loaded = sorted(m for m in set(sys.modules) - before if m.split(".")[0] in banned)
    assert not loaded, f"DB drivers pulled by light compute modules: {loaded}"


def test_mps_fallback_env_precedes_torch_import():
    """#277 (review follow-up): PYTORCH_ENABLE_MPS_FALLBACK must be set at
    PACKAGE INIT — torch reads it at import time; a setdefault after torch
    is loaded is dead code (empirical probe on this machine: lstsq on MPS
    falls back when the env precedes `import torch`, raises
    NotImplementedError when set after; the embedder stage loads torch long
    before the entity stage).

    Subprocess sonde in a FRESH interpreter: importing the package must
    (a) set the env and (b) NOT import torch — proving the ordering.
    Mutation probes: remove the setdefault from axiom_ng_runner/__init__.py
    → (a) goes red; move it below a torch import → (b) goes red."""
    import os
    import subprocess
    import sys
    from pathlib import Path

    code = (
        "import os, sys; "
        "import axiom_ng_runner; "
        "print(os.environ.get('PYTORCH_ENABLE_MPS_FALLBACK'), "
        "'torch' in sys.modules)"
    )
    out = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True, text=True, check=True, cwd=str(Path(__file__).resolve().parents[2]),
    ).stdout.strip()
    env_val, torch_loaded = out.rsplit(" ", 1)
    assert env_val == "1", (
        f"package init must set PYTORCH_ENABLE_MPS_FALLBACK (got {env_val!r})"
    )
    assert torch_loaded == "False", (
        "package init imported torch — the fallback env would race the "
        "torch import and be dead code"
    )
    # setdefault semantics (review NIT): an operator override set BEFORE
    # the import stays authoritative — checked in a subprocess, the only
    # place where "before the package import" is still arrangeable.
    out2 = subprocess.run(
        [sys.executable, "-c",
         ("import os; import axiom_ng_runner; "
          "print(os.environ.get('PYTORCH_ENABLE_MPS_FALLBACK'))")],
        capture_output=True, text=True, check=True,
        cwd=str(Path(__file__).resolve().parents[2]),
        env={**os.environ, "PYTORCH_ENABLE_MPS_FALLBACK": "0"},
    ).stdout.strip()
    assert out2 == "0", (
        f"explicit operator override must survive the package init (got {out2!r})"
    )
