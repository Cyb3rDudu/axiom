#!/bin/sh
# build_fixer_artifact.sh — G2 of #205.
# Builds the autarkic pdf_repair_agent artifact:
#   dist/axiom-fixer-<version>-macos-arm64.tar.zst
# Layout: fixer-<version>/{env/,app/}
#   env/  conda-forge python 3.11 + pinned reqs — BUNDLED INTERPRETER via
#         conda-pack (same approach as build_runner_artifact.sh). Fixes the
#         "not autarkic" finding from the v0.1.11 install (issue #208):
#         the previous venv layout symlinked to the build-host python
#         (/Library/Frameworks/...) and broke on every other host.
#   app/  package sources.
# Install-time fixup: run env/bin/conda-unpack ONCE after extracting
# (install_dist.sh does this automatically). The old venv fix-env/
# .build-prefix mechanism is gone with the venv.
# Isolation is proven AGAINST the ARTIFACT: the import_audit guard runs
# with the staged env before tarring.
#
# Host dependencies NOT bundled (documented in docs/operations/services.md):
#   tesseract5 (+deu) and ghostscript must be on PATH for the OCR lane.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$DIST/build/fixer"
VERSION="${1:?usage: build_fixer_artifact.sh <version>}"
ARTIFACT="$DIST/axiom-fixer-$VERSION-macos-arm64.tar.zst"
STAGE="$BUILD/fixer-$VERSION"
PREFIX="$BUILD/env"

cd "$ROOT"
rm -rf "$STAGE"
mkdir -p "$STAGE"

# --- app/: sources without runtime residue -----------------------------------
rsync -a \
    --exclude '.venv' --exclude '__pycache__' --exclude 'runs' \
    axiom_ng/tools/pdf_repair_agent/ "$STAGE/app/"

# fix.sh ships INTO the artifact (#206): the installed /opt/axiom/bin/axiom-fixer
# shim execs it, so EVERY caller (invoker, operator) runs through the same
# per-key lockdir + 30-min timeout — one source of truth, no shim duplicate.
# fix.sh's defaults (AXIOM_FIXER=/opt/axiom/fixer/current/…) match the installed
# layout via the `current` symlink.
install -m 0755 scripts/fix.sh "$STAGE/fix.sh"

# --- micromamba (single static binary, cached under dist/tooling) -----------
# Shared cache with build_runner_artifact.sh — second build reuses the binary.
MM="$DIST/tooling/bin/micromamba"
if [ ! -x "$MM" ]; then
    mkdir -p "$DIST/tooling"
    echo "fixer-artifact: downloading micromamba"
    curl -Ls https://micro.mamba.pm/api/micromamba/darwin-arm64/latest -o "$DIST/tooling/mm.tar.bz2"
    tar -xjf "$DIST/tooling/mm.tar.bz2" -C "$DIST/tooling" bin/micromamba
    rm -f "$DIST/tooling/mm.tar.bz2"
fi
MAMBA_ROOT_PREFIX="$DIST/tooling/mamba-root"
export MAMBA_ROOT_PREFIX

# --- conda env with python 3.11 (lockfile was frozen on 3.11) ----------------
rm -rf "$PREFIX"
"$MM" create -y -p "$PREFIX" -c conda-forge 'python=3.11' pip
PY="$PREFIX/bin/python"

# --- pinned deps (lock wins when present — same rule as bootstrap.sh) -------
REQS="axiom_ng/tools/pdf_repair_agent/requirements.txt"
[ -f axiom_ng/tools/pdf_repair_agent/requirements.lock.txt ] && \
    REQS="axiom_ng/tools/pdf_repair_agent/requirements.lock.txt"
"$PY" -m pip install -q --disable-pip-version-check -r "$REQS"
"$PY" -m pip install -q --disable-pip-version-check conda-pack

# --- pack env (relocatable; conda-unpack fixes prefixes at install) ---------
rm -rf "$STAGE/env"
"$PREFIX/bin/conda-pack" -p "$PREFIX" --n-threads -1 -o "$BUILD/env.tar.gz"
mkdir -p "$STAGE/env"
tar -xzf "$BUILD/env.tar.gz" -C "$STAGE/env"
rm -f "$BUILD/env.tar.gz"

# --- interpreter autarky proof (#208): NO symlink may leave the artifact ----
if find "$STAGE/env/bin" -name 'python*' -type l | while read -r l; do
    tgt=$(readlink "$l")
    case "$tgt" in
        /*) case "$tgt" in "$STAGE"/*) ;; *) echo "non-bundled interpreter link: $l -> $tgt"; exit 1 ;; esac ;;
        *) : ;; # relative link inside env/ (python -> python3.11) is fine
    esac
done; then :; else
    echo "fixer-artifact: interpreter is not bundled — refusing to ship" >&2
    exit 1
fi
[ -x "$STAGE/env/bin/python3.11" ] && ! [ -L "$STAGE/env/bin/python3.11" ] || {
    echo "fixer-artifact: env/bin/python3.11 must be a real binary (conda-pack), not a symlink" >&2
    exit 1
}

# --- isolation proof against the ARTIFACT --------------------------------------
(
    cd "$STAGE/app" && "$STAGE/env/bin/python" - <<'EOF'
import sys
from pathlib import Path
sys.path.insert(0, str(Path(".").resolve()))
from tools import import_audit
r = import_audit.audit()
assert r["clean"], f"isolation violated in artifact: {r['violations']}"
print("fixer-artifact: import_audit clean against artifact env")
EOF
)
# Smoke from a NEUTRAL cwd so we test the env, not the source tree (#209
# lesson: a smoke run from a source dir masks missing installs).
(
    cd / && "$STAGE/env/bin/python" -c 'import pymupdf; print("fixer-artifact: staged pymupdf", pymupdf.__version__)'
)
(cd "$STAGE/app" && "$STAGE/env/bin/python" -m pytest tests/test_import_guard.py -q)

# --- artifact --------------------------------------------------------------------
tar --zstd -C "$BUILD" -cf "$ARTIFACT" "fixer-$VERSION"
(cd "$DIST" && shasum -a 256 "${ARTIFACT##*/}" >"${ARTIFACT##*/}.sha256")
echo "fixer-artifact: $ARTIFACT"
echo "install: extract to /opt/axiom/fixer/$VERSION — install_dist.sh runs env/bin/conda-unpack automatically"
