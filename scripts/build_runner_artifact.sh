#!/bin/sh
# build_runner_artifact.sh — G2 of #205 (supersedes #204 packaging goals).
# Builds a relocatable macOS arm64 runner artifact with micromamba+conda-pack:
#   dist/axiom-runner-<version>-macos-arm64.tar.zst
# Layout inside the tar: runner-<version>/{env/,app/}
#   env/  conda-forge python 3.11 + requirements(-heavy) + installed runner
#   app/  runner sources (reference; the executable is env/bin/axiom-runner)
# Install-time fixup: run env/bin/conda-unpack ONCE after extracting (#204).
# No .venv trees are created in the Git workspace — everything stages under
# dist/ (repo-ignored).
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BUILD="$DIST/build/runner"
VERSION="${1:?usage: build_runner_artifact.sh <version>}"
ARTIFACT="$DIST/axiom-runner-$VERSION-macos-arm64.tar.zst"
PREFIX="$BUILD/env"

cd "$ROOT"

# --- micromamba (single static binary, cached under dist/tooling) ----------
MM="$DIST/tooling/bin/micromamba"
if [ ! -x "$MM" ]; then
    mkdir -p "$DIST/tooling"
    echo "runner-artifact: downloading micromamba"
    curl -Ls https://micro.mamba.pm/api/micromamba/darwin-arm64/latest -o "$DIST/tooling/mm.tar.bz2"
    tar -xjf "$DIST/tooling/mm.tar.bz2" -C "$DIST/tooling" bin/micromamba
    rm -f "$DIST/tooling/mm.tar.bz2"
fi
MAMBA_ROOT_PREFIX="$DIST/tooling/mamba-root"
export MAMBA_ROOT_PREFIX

# --- conda env with python 3.11 --------------------------------------------
if [ ! -x "$PREFIX/bin/python" ]; then
    "$MM" create -y -p "$PREFIX" -c conda-forge 'python=3.11' pip
fi
PY="$PREFIX/bin/python"

# --- deps + runner package --------------------------------------------------
"$PY" -m pip install -q --disable-pip-version-check \
    -r axiom_ng_runner/requirements.txt -r axiom_ng_runner/requirements-heavy.txt
"$PY" -m pip install -q --disable-pip-version-check --no-deps ./axiom_ng_runner
"$PY" -m pip install -q --disable-pip-version-check conda-pack

# --- app/ sources ------------------------------------------------------------
STAGE="$BUILD/runner-$VERSION"
rm -rf "$STAGE"
mkdir -p "$STAGE"
rsync -a --delete \
    --exclude '.venv' --exclude '__pycache__' --exclude 'tests' \
    --exclude 'scripts' --exclude 'shell.nix' \
    --exclude 'build' --exclude '*.egg-info' \
    axiom_ng_runner/ "$STAGE/app/"

# --- pack env (relocatable; conda-unpack fixes prefixes at install) ---------
rm -rf "$STAGE/env"
"$PREFIX/bin/conda-pack" -p "$PREFIX" --n-threads -1 -o "$BUILD/env.tar.gz"
mkdir -p "$STAGE/env"
tar -xzf "$BUILD/env.tar.gz" -C "$STAGE/env"
rm -f "$BUILD/env.tar.gz"

# --- verify entry point from the STAGED env, from a NEUTRAL cwd (#209: a
# source-dir cwd masks a missing install — `import axiom_ng_runner` would hit
# ../axiom_ng_runner on sys.path even when the env shipped 0 module files). ---
(
    cd / && "$STAGE/env/bin/python" -c 'import axiom_ng_runner, torch; print("staged import ok (neutral cwd), torch", torch.__version__, "mps", torch.backends.mps.is_available())'
)

# --- artifact -----------------------------------------------------------------
tar --zstd -C "$BUILD" -cf "$ARTIFACT" "runner-$VERSION"
(cd "$DIST" && shasum -a 256 "${ARTIFACT##*/}" >"${ARTIFACT##*/}.sha256")
echo "runner-artifact: $ARTIFACT"
echo "install: extract to /opt/axiom/runner/$VERSION, then run env/bin/conda-unpack once"
