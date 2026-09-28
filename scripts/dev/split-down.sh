#!/bin/bash
# split-down.sh — stop the F11 split topology (#305). Leaves the dev
# substrate (DB, index, artifacts) untouched; kills the three RAG
# processes and a runner split-up started itself (a runner ADOPTED from
# a running dev env stays — dev-down.sh owns that one).
set -euo pipefail

STATE="$HOME/.local/state/axiom-dev"
PIDFILE="$STATE/split.pid"

[ -f "$PIDFILE" ] || { echo "split-down: no $PIDFILE — nothing to stop"; exit 0; }

dev_runner_pid() {
    [ -r "$STATE/dev.pid" ] && awk '$1=="runner"{print $2}' "$STATE/dev.pid" || true
}

while read -r name pid; do
    if [ "$name" = "runner" ] && [ "$pid" = "$(dev_runner_pid)" ]; then
        echo "split-down: runner (pgid $pid) belongs to the dev env — leaving it"
        continue
    fi
    if kill -0 "$pid" 2>/dev/null; then
        kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
        echo "split-down: stopped $name (pgid $pid)"
    else
        echo "split-down: $name (pgid $pid) already gone"
    fi
done <"$PIDFILE"

rm -f "$PIDFILE"
echo "split-down: split topology down (dev substrate untouched)"
