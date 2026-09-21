#!/bin/bash
# dev-down.sh — stop the dev environment started by dev-up.sh.
# Kills both process groups (each service is its own session via setsid),
# verifies nothing is left on :8111/:8112, removes the pid file.
# Leaves state (logs, artifacts, dev index, axiom_dev) untouched.

set -euo pipefail

STATE="$HOME/.local/state/axiom-dev"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
RAG_PORT=8111
RUNNER_PORT=8112

die() {
    echo "dev-down: $*" >&2
    exit 1
}
note() { echo "dev-down: $*"; }

[ -f "$STATE/dev.pid" ] || die "no dev environment running ($STATE/dev.pid missing)"

# --- stop both groups --------------------------------------------------------

stop_group() { # $1 = label, $2 = pid (== pgid, setsid makes pid the group leader)
    local label="$1" pid="$2"
    if ! kill -0 "$pid" 2>/dev/null; then
        note "$label (pid $pid) already gone"
        return
    fi
    kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
    for _ in $(seq 1 20); do
        kill -0 "$pid" 2>/dev/null || {
            note "$label (pid $pid) stopped"
            return
        }
        sleep 1
    done
    kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    sleep 1
    kill -0 "$pid" 2>/dev/null && die "$label (pid $pid) survived SIGKILL" || note "$label (pid $pid) killed"
}

while read -r label pid; do
    [ -n "${label:-}" ] && [ -n "${pid:-}" ] && stop_group "$label" "$pid"
done <"$STATE/dev.pid"

rm -f "$STATE/dev.pid"

# --- verify nothing is left --------------------------------------------------

left=0
for p in "$RAG_PORT" "$RUNNER_PORT"; do
    if lsof -i ":$p" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "dev-down: WARNING port $p still listening:" >&2
        lsof -i ":$p" -sTCP:LISTEN >&2
        left=1
    fi
done
pgrep -f "$STATE/bin/axiom-ng-dev" >/dev/null 2>&1 && {
    echo "dev-down: WARNING dev RAG process still alive" >&2
    left=1
}
# release mode (#295): freeze-bit binaries live under $STATE/release instead
pgrep -f "$STATE/release" >/dev/null 2>&1 && {
    echo "dev-down: WARNING release-mode dev process still alive" >&2
    left=1
}
pgrep -f "$REPO/axiom_ng_runner/.venv" >/dev/null 2>&1 && {
    echo "dev-down: WARNING dev runner process still alive" >&2
    left=1
}

[ "$left" = 0 ] && note "clean — no dev processes left (ports $RAG_PORT/$RUNNER_PORT free)" || exit 1
