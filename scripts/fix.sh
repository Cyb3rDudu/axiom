#!/bin/sh
# fix.sh — invocation convention for the pdf_repair_agent (#205 G3).
#
# The fixer is an EVENT RUNNER (owner decision): one process per Zotero
# attachment key, invoked by the operator / ingest path — NEVER a KeepAlive
# launchd service. Two concurrent instances on the same key would corrupt
# the agent's working directory, so this wrapper serializes per key.
#
# Usage: scripts/fix.sh <zotero-key> [--apply] [--format pdf|epub] [--source PATH]
#   without --apply the agent runs its forensic dry-run (default)
#   --format routes the repair arm (#220): pdf (default, unchanged
#     pdf_repair_agent path) or epub — the mechanical EPUB toolbelt in
#     the runner env (normalize/spine/corpses, red→green proof)
#   --source passes the attachment's local path to the EPUB arm (the
#     invoker knows it; the PDF agent resolves its own sources).
#
# Lock: lockdir under ~/.local/state/axiom/runs (mkdir is atomic).
# Timeout: 30 min hard cap per invocation (timeout binary on PATH, e.g.
#   nix coreutils; without it the run is unbounded — logged).
set -eu

KEY="${1:?usage: fix.sh <zotero-key> [--apply] [--format pdf|epub] [--source PATH]}"
shift || true

FORMAT="pdf"
SOURCE=""
EXTRA=""
while [ $# -gt 0 ]; do
    case "$1" in
        --format) FORMAT="${2:?--format needs a value}"; shift 2 ;;
        --source) SOURCE="${2:?--source needs a value}"; shift 2 ;;
        *) EXTRA="$EXTRA $1"; shift ;;
    esac
done
[ -n "$EXTRA" ] && set -- $EXTRA

RUNS="$HOME/.local/state/axiom/runs"
LOCK="$RUNS/fix-$KEY.lock"
mkdir -p "$RUNS"
if ! mkdir "$LOCK" 2>/dev/null; then
    # Stale-lock recovery: SIGKILL/power loss can leave the lockdir behind.
    # If the recorded pid is gone, the lock is dead — remove it and retry once.
    oldpid="$(cat "$LOCK/pid" 2>/dev/null || true)"
    if [ -n "$oldpid" ] && ! kill -0 "$oldpid" 2>/dev/null; then
        rm -f "$LOCK/pid"
        rmdir "$LOCK" 2>/dev/null || true
        mkdir "$LOCK" 2>/dev/null || {
            echo "fix: could not recover lock $LOCK — manual: rmdir $LOCK" >&2
            exit 3
        }
    else
        echo "fix: another repair run is active for key $KEY ($LOCK, pid $oldpid)" >&2
        echo "fix: stale after a crash? manual recovery: rmdir $LOCK" >&2
        exit 3
    fi
fi
printf '%s\n' "$$" >"$LOCK/pid"
trap 'rm -f "$LOCK/pid" 2>/dev/null; rmdir "$LOCK" 2>/dev/null || true' EXIT INT TERM

FIXER="${AXIOM_FIXER:-/opt/axiom/fixer/current/env/bin/python}"
APP="${AXIOM_FIXER_APP:-/opt/axiom/fixer/current/app/repair_agent.py}"
[ -x "$FIXER" ] || {
    echo "fix: fixer not installed at $FIXER (make install / install_release.sh)"
    exit 1
}

rc=0
if [ "$FORMAT" = "epub" ]; then
    # #220: mechanical EPUB repair — runner env, no agent, no models.
    RUNNER_PY="${AXIOM_RUNNER_PYTHON:-/opt/axiom/runner/current/env/bin/python}"
    [ -x "$RUNNER_PY" ] || {
        echo "fix: runner python not installed at $RUNNER_PY (EPUB arm)" >&2
        exit 1
    }
    [ -n "$SOURCE" ] || { echo "fix: --format epub needs --source PATH" >&2; exit 1; }
    if command -v timeout >/dev/null 2>&1; then
        timeout 1800 "$RUNNER_PY" -m axiom_ng_runner.compute_core.epub_repair_cli \
            --key "$KEY" --source "$SOURCE" --work-root "$RUNS" "$@" || rc=$?
    else
        "$RUNNER_PY" -m axiom_ng_runner.compute_core.epub_repair_cli \
            --key "$KEY" --source "$SOURCE" --work-root "$RUNS" "$@" || rc=$?
    fi
    exit "$rc"
fi

if command -v timeout >/dev/null 2>&1; then
    timeout 1800 "$FIXER" "$APP" --key "$KEY" "$@" || rc=$?
else
    echo "fix: WARNING no timeout binary on PATH — running unbounded" >&2
    "$FIXER" "$APP" --key "$KEY" "$@" || rc=$?
fi
exit "$rc"
