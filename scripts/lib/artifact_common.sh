#!/bin/sh
# artifact_common.sh — shared build plumbing for the two autarkic artifacts
# (fixer + runner), sourced by build_fixer_artifact.sh and
# build_runner_artifact.sh (#286 follow-up: ONE policy, no parallel rules).
#
# Sourcing script must set: ROOT, DIST. Exports: MM, MAMBA_ROOT_PREFIX.
#
# Shared policies (identical in both builds — divergence is a review
# finding, not a feature):
#   * micromamba binary + package cache under dist/tooling (shared, so the
#     second build reuses both),
#   * env REUSE: the prefix is created only when missing (the runner's
#     semantics). Changing a conda spec line requires ONE manual
#     `rm -rf dist/build/<side>/env` — deliberate, so cache-hardlinked
#     package files are never silently overwritten,
#   * conda-pack staging into <stage>/env (relocatable; conda-unpack at
#     install rewrites prefixes),
#   * bundled_env drift guard: the runner-canonical and the fixer-mirror
#     copy of bundled_env.py must be byte-identical before anything ships.

# artifact_mm_bootstrap — download micromamba once into dist/tooling and
# point MAMBA_ROOT_PREFIX at the shared package cache.
artifact_mm_bootstrap() {
    MM="$DIST/tooling/bin/micromamba"
    if [ ! -x "$MM" ]; then
        mkdir -p "$DIST/tooling"
        echo "artifact: downloading micromamba"
        curl -Ls https://micro.mamba.pm/api/micromamba/darwin-arm64/latest -o "$DIST/tooling/mm.tar.bz2"
        tar -xjf "$DIST/tooling/mm.tar.bz2" -C "$DIST/tooling" bin/micromamba
        rm -f "$DIST/tooling/mm.tar.bz2"
    fi
    MAMBA_ROOT_PREFIX="$DIST/tooling/mamba-root"
    export MAMBA_ROOT_PREFIX
}

# artifact_pack_env <prefix> <stage-dir> — conda-pack the env and unpack it
# into <stage-dir>/env (the relocatable artifact layout).
artifact_pack_env() {
    _prefix="$1"
    _stage="$2"
    _build="$(dirname "$_stage")"
    rm -rf "$_stage/env"
    "$_prefix/bin/conda-pack" -p "$_prefix" --n-threads -1 -o "$_build/env.tar.gz"
    mkdir -p "$_stage/env"
    tar -xzf "$_build/env.tar.gz" -C "$_stage/env"
    rm -f "$_build/env.tar.gz"
}

# artifact_assert_bundled_env_identical — the shared runtime helper lives
# in BOTH trees (folio_harvest vendoring pattern); drift must never ship.
artifact_assert_bundled_env_identical() {
    _canonical="$ROOT/axiom_ng_runner/compute_core/bundled_env.py"
    _mirror="$ROOT/axiom_ng/tools/pdf_repair_agent/tools/bundled_env.py"
    cmp -s "$_canonical" "$_mirror" || {
        echo "artifact: bundled_env drift — runner canonical and fixer mirror differ:" >&2
        echo "  diff $_canonical $_mirror" >&2
        exit 1
    }
}
