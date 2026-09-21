#!/bin/bash
# golden_baseline.sh — `make golden-baseline` backend (#295).
#
# Runs the frozen v0.1.18 compatibility suite (axiom_ng/internal/baseline)
# against the dev environment. Preconditions enforced here, not in the
# tests: the dev RAG must serve the FREEZE BITS (release mode), otherwise
# the suite would baseline a working-tree debug build — the exact mistake
# this gate exists to prevent.
#
# Determinism DoD: actuals land in dist/baseline-actual/ — two consecutive
# runs must produce byte-identical files there, EXCEPT live_row_counts.txt
# (non-gating live inventory by design: the suite's own ingest probe
# writes rows, so live counts legitimately move between runs). The
# self-check below hashes every other actual and fails on run-over-run
# drift.
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

# --- determinism self-check ------------------------------------------------
# Hash every actual EXCEPT the non-gating live row inventory; the manifest
# hash must be identical run over run. dist/ is gitignored — this is a
# dev-host instrument (CI always starts without the .sha and records it).
ACTUAL_DIR="$REPO/dist/baseline-actual"
SHA_FILE="$REPO/dist/baseline-actual.sha"
new_sum="$(cd "$ACTUAL_DIR" && shasum -a 256 * | grep -v 'live_row_counts\.txt$' | LC_ALL=C sort | shasum -a 256 | awk '{print $1}')"
if [ -f "$SHA_FILE" ]; then
    if [ "$new_sum" != "$(awk 'NR==1{print $1}' "$SHA_FILE")" ]; then
        echo "golden-baseline: determinism check FAILED — actuals differ from the previous run" >&2
        echo "  either real drift (investigate the suite output above) or fixtures were" >&2
        echo "  deliberately updated (then remove $SHA_FILE to re-arm the check)" >&2
        exit 1
    fi
    echo "golden-baseline: determinism self-check green (actuals unchanged run over run)"
else
    printf '%s\n' "$new_sum" >"$SHA_FILE"
    echo "golden-baseline: determinism baseline recorded ($SHA_FILE) — the next run must match it"
fi
