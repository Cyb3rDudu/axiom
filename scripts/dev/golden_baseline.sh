#!/bin/bash
# golden_baseline.sh — `make golden-baseline` backend (#295).
#
# Runs the frozen v0.1.18 compatibility suite (axiom_ng/internal/baseline)
# against the dev environment. Preconditions enforced here, not in the
# tests: the dev RAG must serve the FREEZE BITS (release mode), otherwise
# the suite would baseline a working-tree debug build — the exact mistake
# this gate exists to prevent.
#
# Actuals land in dist/baseline-actual/ — two consecutive runs must
# produce byte-identical files there (determinism DoD; compare with
# `shasum -a 256 dist/baseline-actual/*`).
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
RAG_PORT=8111
FREEZE_BUILD='axiom-ng v0.1.17-59-gbf77410 (commit bf77410, release build)'

health="$(curl -fsS "http://127.0.0.1:$RAG_PORT/api/health" 2>/dev/null || true)"
if ! printf '%s' "$health" | grep -qF "$FREEZE_BUILD"; then
    echo "golden-baseline: dev RAG on :$RAG_PORT is not serving the freeze bits" >&2
    echo "  health: ${health:-<no answer>}" >&2
    echo "  start it first:  scripts/dev/dev-up.sh --release" >&2
    exit 2
fi
echo "golden-baseline: freeze bits confirmed on :$RAG_PORT"

# shellcheck disable=SC1091
. "$REPO/scripts/dev/env.sh"
cd "$REPO/axiom_ng"
AXIOM_BASELINE_LIVE=1 AXIOM_BASELINE_ACTUAL="$REPO/dist/baseline-actual" \
    go test ./internal/baseline -count=1 -timeout 30m
