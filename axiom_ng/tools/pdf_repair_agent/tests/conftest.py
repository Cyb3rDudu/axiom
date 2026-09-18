"""Fixer-Test-conftest (#286 review Runde 3): Sandbox-Storage als EINE
Quelle.

Die committeten Fixture-PDFs sind da, fixtures/storage/ (Sandbox-Kopien)
aber absichtlich ignoriert und regenerierbar. Bisher erzeugten sie nur
später einsammelnde Testdateien per _ensure() — im Fresh-Checkout (CI)
liefen früher gesammelte Tests (test_executable_doc, test_repair_agent)
vor der Erzeugung und fielen. Dieses autouse-Session-Fixture stellt die
Storage VOR jedem Test bereit; die per-Modul-_ensure-Aufrufe bleiben als
idempotente Gürtel bestehen.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
PKG = HERE.parent
sys.path.insert(0, str(PKG))


@pytest.fixture(scope="session", autouse=True)
def _sandbox_storage() -> None:
    sentinel = PKG / "fixtures" / "storage" / "DDDD4444" / "scan_folios.pdf"
    if not sentinel.exists():
        from fixtures import generate_fixtures

        generate_fixtures.ensure_storage()
