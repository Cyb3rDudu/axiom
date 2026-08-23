#!/bin/sh
# fix.sh — invocation convention for the pdf_repair_agent (#205 G3).
#
# The fixer is an EVENT RUNNER (owner decision): one process per Zotero
# attachment key, invoked by the operator / ingest path — NEVER a KeepAlive
# launchd service. Two concurrent instances on the same key would corrupt
# the agent's working directory, so this wrapper serializes per key.
#
# Usage: scripts/fix.sh <zotero-key> [--apply]
#   without --apply the agent runs its forensic dry-run (default).
#
# Lock: lockdir under ~/.local/state/axiom/runs (mkdir is atomic).
# Timeout: 30 min hard cap per invocation (timeout binary on PATH, e.g.
#   nix coreutils; without it the run is unbounded — logged).
set -eu

KEY="${1:?usage: fix.sh <zotero-key> [--apply]}"
shift || true

RUNS="$HOME/.local/state/axiom/runs"
LOCK="$RUNS/fix-$KEY.lock"
mkdir -p "$RUNS"
if ! mkdir "$LOCK" 2>/dev/null; then
    echo "fix: another repair run is active for key $KEY ($LOCK)" >&2
    exit 3
fi
trap 'rmdir "$LOCK" 2>/dev/null || true' EXIT INT TERM

FIXER="${AXIOM_FIXER:-/opt/axiom/fixer/current/env/bin/python}"
APP="${AXIOM_FIXER_APP:-/opt/axiom/fixer/current/app/repair_agent.py}"
[ -x "$FIXER" ] || { echo "fix: fixer not installed at $FIXER (make install / install_release.sh)"; exit 1; }

rc=0
if command -v timeout >/dev/null 2>&1; then
    timeout 1800 "$FIXER" "$APP" --key "$KEY" "$@" || rc=$?
else
    echo "fix: WARNING no timeout binary on PATH — running unbounded" >&2
    "$FIXER" "$APP" --key "$KEY" "$@" || rc=$?
fi
rmdir "$LOCK" 2>/dev/null || true
exit "$rc"
