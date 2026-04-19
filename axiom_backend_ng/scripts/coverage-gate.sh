#!/usr/bin/env bash
# coverage-gate.sh — fail CI when axiom-ng package coverage drops.
#
# Rationale: the Go packages in axiom_backend_ng are held to a high
# coverage bar (target 100%, current floor documented below). The
# remaining gap is almost entirely in library-error wrappers that
# would require mocking standard-library calls (bcrypt at invalid cost,
# crypto/rand failure, JWT HMAC sign failure, pgxpool internal config
# validation) and in defensive panic guards behind RequireAuth that
# cannot be reached at runtime.
#
# FLOOR_TOTAL is the aggregate coverage across all packages measured
# with -coverpkg=./... . Raise it as coverage improves; never lower
# without explicit buy-in.
#
# Env vars:
#   FLOOR_TOTAL (default 85.0)   overall minimum %
#   PACKAGES    (default "./...") go test pattern
set -euo pipefail

FLOOR_TOTAL=${FLOOR_TOTAL:-85.0}
PACKAGES=${PACKAGES:-./...}
OUT=${OUT:-coverage.out}

cd "$(dirname "$0")/.."

go test -count=1 -timeout 600s -coverpkg="$PACKAGES" -coverprofile="$OUT" "$PACKAGES" >/dev/null

total=$(go tool cover -func="$OUT" | awk '/^total:/{gsub("%","",$3); print $3}')
printf 'axiom-ng total coverage: %s%% (floor %s%%)\n' "$total" "$FLOOR_TOTAL"

awk -v got="$total" -v min="$FLOOR_TOTAL" '
    BEGIN { if (got+0 < min+0) { exit 1 } }
' || {
    echo "::error::coverage $total% is below floor $FLOOR_TOTAL%"
    go tool cover -func="$OUT" | awk '$3 != "100.0%"' | tail -20
    exit 1
}
