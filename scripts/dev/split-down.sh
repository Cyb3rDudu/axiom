#!/bin/bash
# split-down.sh — stop the F11 split topology (#305). Leaves the dev
# substrate (DB, index, artifacts) untouched; kills the three RAG
# processes and any runner THIS topology started (a runner adopted from
# a running dev env is never recorded in split.pid — dev-down owns it).
set -euo pipefail

STATE="$HOME/.local/state/axiom-dev"
PIDFILE="$STATE/split.pid"

[ -f "$PIDFILE" ] || { echo "split-down: no $PIDFILE — nothing to stop"; exit 0; }

# The split.pid runner entry exists ONLY when split-up started the runner
# itself (an adopted dev runner is never recorded) — every entry is ours.
while read -r name pid; do
    if kill -0 "$pid" 2>/dev/null; then
        kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
        echo "split-down: stopped $name (pgid $pid)"
    else
        echo "split-down: $name (pgid $pid) already gone"
    fi
done <"$PIDFILE"

rm -f "$PIDFILE"
echo "split-down: split topology down (dev substrate untouched)"
