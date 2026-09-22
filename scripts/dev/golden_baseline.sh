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
# self-check below pins the actual FILE SET and per-file hashes and fails
# on any run-over-run drift — including a probe that stopped producing its
# actual.
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

ACTUAL_DIR="$REPO/dist/baseline-actual"
SHA_FILE="$REPO/dist/baseline-actual.sha"

# Start from an EMPTY actual dir (M2): a probe that silently stopped
# writing its actual must show up as a MISSING file in the manifest compare
# below, not survive as a stale leftover from an earlier run.
mkdir -p "$ACTUAL_DIR"
rm -f "$ACTUAL_DIR"/*

# shellcheck disable=SC1091
. "$REPO/scripts/dev/env.sh"
cd "$REPO/axiom_ng"
AXIOM_BASELINE_LIVE=1 AXIOM_BASELINE_ACTUAL="$ACTUAL_DIR" \
    go test ./internal/baseline -count=1 -timeout 30m

# --- determinism self-check ------------------------------------------------
# Per-file manifest over every actual EXCEPT the non-gating live row
# inventory (the suite's own ingest probe writes rows, so live counts
# legitimately move). The compare covers the FILE SET and each hash —
# missing, extra, changed all fail. dist/ is gitignored — this is a
# dev-host instrument (CI always starts without the .sha and records it).
new_manifest="$(cd "$ACTUAL_DIR" && shasum -a 256 ./* | awk '$2 != "./live_row_counts.txt"' | LC_ALL=C sort)"
if [ -z "$new_manifest" ]; then
    echo "golden-baseline: no actuals produced — suite did not run live?" >&2
    exit 1
fi
if [ -f "$SHA_FILE" ]; then
    if ! printf '%s\n' "$new_manifest" | diff - "$SHA_FILE" >/tmp/golden-baseline-manifest.diff 2>&1; then
        echo "golden-baseline: determinism check FAILED — actuals differ from the previous run:" >&2
        sed 's/^/  /' /tmp/golden-baseline-manifest.diff >&2
        echo "  either real drift / a lost probe (investigate above) or fixtures were" >&2
        echo "  deliberately updated (then remove $SHA_FILE to re-arm the check)" >&2
        exit 1
    fi
    echo "golden-baseline: determinism self-check green (file set + hashes unchanged run over run)"
else
    printf '%s\n' "$new_manifest" >"$SHA_FILE"
    echo "golden-baseline: determinism baseline recorded ($SHA_FILE, $(printf '%s\n' "$new_manifest" | wc -l | tr -d ' ') files) — the next run must match it"
fi
