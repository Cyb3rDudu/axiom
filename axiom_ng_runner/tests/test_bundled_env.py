"""#286: bundled_env — env-relative resolution of bundled binaries.

The runner is the CANONICAL home (compute_core/bundled_env.py); the fixer
carries a code-identical vendored mirror. These tests pin the resolution
semantics and the cross-tree identity."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from axiom_ng_runner.compute_core import bundled_env


def _fake_bundled_env(tmp_path, marker: str):
    env = tmp_path / "fakeenv"
    (env / "bin").mkdir(parents=True)
    binfile = env / "bin" / "pandoc"
    binfile.write_text("#!/bin/sh\n" + marker + "\nexit 0\n")
    binfile.chmod(0o755)
    return env


def test_bundled_bin_prefers_env_over_path(tmp_path, monkeypatch):
    fake = _fake_bundled_env(tmp_path, "env-pandoc")
    other = tmp_path / "other"
    other.mkdir()
    (other / "pandoc").write_text("#!/bin/sh\nexit 0\n")
    (other / "pandoc").chmod(0o755)
    monkeypatch.setattr(bundled_env.sys, "prefix", str(fake))
    monkeypatch.setattr(bundled_env.shutil, "which", lambda n: str(other / n))
    assert bundled_env.bundled_bin("pandoc") == str(fake / "bin" / "pandoc")


def test_bundled_bin_path_fallback_without_bundle(tmp_path, monkeypatch):
    fake = _fake_bundled_env(tmp_path, "env-pandoc")
    monkeypatch.setattr(bundled_env.sys, "prefix", str(fake))
    monkeypatch.setattr(bundled_env.shutil, "which", lambda n: None)
    # env without the binary -> PATH fallback -> None here (no host pandoc
    # in the sanitized case) — the honest "not available" answer
    (fake / "bin" / "pandoc").unlink()
    assert bundled_env.bundled_bin("pandoc") is None


def test_child_env_prepends_env_bin(tmp_path, monkeypatch):
    fake = _fake_bundled_env(tmp_path, "env-pandoc")
    monkeypatch.setattr(bundled_env.sys, "prefix", str(fake))
    monkeypatch.setenv("PATH", "/usr/bin:/bin")
    child = bundled_env.child_env(with_tessdata=False)
    assert child["PATH"].startswith(str(fake / "bin") + ":"), child["PATH"]
    assert "TESSDATA_PREFIX" not in child  # pandoc path carries no models


def test_bundled_env_drift_zwischen_den_baeumen():
    """Das Fixer-Mirror muss code-identisch zum Runner-Kanonikat bleiben
    (Gegenstück: pdf_repair_agent/tests/test_ocr_tool.py — derselbe Test
    von der anderen Seite; beide Builds cmp-en zusätzlich)."""
    repo = Path(__file__).resolve().parents[1].parent
    canonical = Path(__file__).resolve().parents[1] / "compute_core" / "bundled_env.py"
    mirror = repo / "axiom_ng" / "tools" / "pdf_repair_agent" / "tools" / "bundled_env.py"
    assert canonical.exists() and mirror.exists()
    assert canonical.read_text() == mirror.read_text(), (
        "bundled_env drift zwischen Runner und Fixer — synchronisieren"
    )
