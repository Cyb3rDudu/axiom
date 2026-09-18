"""bundled_env — env-relative resolution of bundled binaries (#286).

Canonical file: axiom_ng_runner/compute_core/bundled_env.py
Vendored mirror: axiom_ng/tools/pdf_repair_agent/tools/bundled_env.py

Both files are CODE-IDENTICAL on purpose (the folio_harvest vendoring
pattern): the fixer package is standalone by design (no project imports —
import_audit enforces it), so a shared runtime helper must live in each
tree. Drift fails loudly: the repo's drift tests compare the files
byte-for-byte and both artifact builds cmp them before staging.

Why this exists (bundled-binaries standard, #224/#211 → #286): everything
the pipeline shells out to ships IN the artifact env. Services run under
launchd/container PATHs that never include env/bin, so resolution must be
ENV-RELATIVE (sys.prefix first, PATH as a dev-machine fallback) and every
child process must inherit an environment in which env/bin precedes PATH
(tools like ocrmypdf locate their engines via PATH). Everything here is
computed AT RUNTIME from sys.prefix — relocatable through conda-unpack.
"""

from __future__ import annotations

import os
import shutil
import sys
from pathlib import Path


def bundled_bin(name: str) -> str | None:
    """Resolve <name> env-relatively: sys.prefix/bin/<name> first (the
    packed artifact env), then PATH fallback (dev machine with host
    tools). None = not available anywhere."""
    cand = Path(sys.prefix) / "bin" / name
    if cand.is_file() and os.access(cand, os.X_OK):
        return str(cand)
    return shutil.which(name)


def tessdata_dir() -> str | None:
    """Env-relative tessdata location: sys.prefix/share/tessdata when it
    carries model files (the fixer artifact ships deu+eng there). None in
    a dev venv — then whatever the host tesseract finds on its own."""
    d = Path(sys.prefix) / "share" / "tessdata"
    if d.is_dir() and any(d.glob("*.traineddata")):
        return str(d)
    return None


def child_env(with_tessdata: bool = True) -> dict[str, str]:
    """Environment for child processes that shell out to bundled tools:
    env/bin PREPENDED to PATH (children resolve their engines via PATH —
    e.g. ocrmypdf finds tesseract/gs, the EPUB worker finds pandoc) and,
    when the env bundles models, TESSDATA_PREFIX pointing at them. Both
    are no-ops in a dev venv without a bundle (transparent host PATH)."""
    env = dict(os.environ)
    if with_tessdata:
        td = tessdata_dir()
        if td:
            env["TESSDATA_PREFIX"] = td
    env_bin = Path(sys.prefix) / "bin"
    # prepend only when the env actually bundles tool binaries (else we
    # would needlessly float random venv scripts to the PATH front)
    if (env_bin / "tesseract").exists() or (env_bin / "pandoc").exists():
        env["PATH"] = str(env_bin) + os.pathsep + env.get("PATH", "")
    return env
