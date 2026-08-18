"""Import-smoke for the vendored compute modules — exists because a syntax
error in a moved module left the suite green (mutation-proven in review)."""

from __future__ import annotations

import importlib
import sys

import pytest

LIGHT_MODULES = [
    "chunker",
    "entity_extractor",
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
