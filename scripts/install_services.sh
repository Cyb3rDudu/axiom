#!/bin/sh
# install_services.sh — G3 of #205: installs the launchd service group from
# deploy/launchd/ into ~/Library/LaunchAgents/.
#   · substitutes __HOME__ (launchd never expands $HOME in Standard*Path)
#   · refuses the carrier-bridge template unless --with-bridge AND the
#     __BRIDGE_CMD__ placeholder has been replaced
#   · preflight asserts env files + /opt install BEFORE the prompt
#   · operator confirmation with the full target list BEFORE anything moves
#   · existing agents are bootout'd, then bootstrap'd + kickstart'd
# Services: com.axiom.rag, com.axiom.rag-dispatch-gpu0/1/2, com.axiom.runner
#           (+ com.axiom.carrier-bridge as explicit opt-in template)
# The fixer is NOT a service — event runner via scripts/fix.sh (owner ruling).
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/deploy/launchd"
DEST="$HOME/Library/LaunchAgents"
LOGS="$HOME/.local/state/axiom/logs"
SERVICES="com.axiom.rag com.axiom.rag-dispatch-gpu0 com.axiom.rag-dispatch-gpu1 com.axiom.rag-dispatch-gpu2 com.axiom.runner"
ENV_FILES="rag.env rag-api.env rag-dispatch-gpu0.env rag-dispatch-gpu1.env rag-dispatch-gpu2.env runner.env"
UID_N="$(id -u)"

with_bridge=0
[ "${1:-}" = "--with-bridge" ] && with_bridge=1

# --- preflight: everything the services need must exist BEFORE the prompt ---
# JSON validation runs on the runner env's python (already a required
# prereq below) — no new host dependency, no macOS CLT python3 stub trap.
PY3=/opt/axiom/runner/current/env/bin/python

missing=""
for f in $ENV_FILES; do
    [ -f "$HOME/.config/axiom/$f" ] || missing="$missing\n  ~/.config/axiom/$f (0700, secrets here)"
done
[ -x /opt/axiom/bin/axiom-ng ] || missing="$missing\n  /opt/axiom/bin/axiom-ng (make install / install_release.sh)"
[ -x /opt/axiom/runner/current/env/bin/python ] || missing="$missing\n  /opt/axiom/runner/current/env/bin/python"
if [ "$with_bridge" = 1 ] && grep -q __BRIDGE_CMD__ "$SRC/com.axiom.carrier-bridge.plist" 2>/dev/null; then
    missing="$missing\n  carrier-bridge template still contains __BRIDGE_CMD__ (edit deploy/launchd/com.axiom.carrier-bridge.plist first!)"
fi

# env-json preflight: JSON-valued env keys must PARSE as JSON after sourcing.
# POSIX sh strips double quotes on assignment, so KEY={"a":1} arrives as {a:1}
# — frozen.go validation then rejects every dispatcher claim and all
# dispatchers spin forever (v0.1.12 live finding, 2026-08-24). JSON in env
# files must be SINGLE-quoted. Checked fail-closed BEFORE the confirm prompt,
# same pattern as the #210 DB guard and the #211 zstd preflight.
for f in $ENV_FILES; do
    # subshell: secrets never enter the installer env. set +e +u: a broken
    # env file must degrade to a NAMED preflight error, not kill the
    # installer silently (bash-3.2 sh-mode propagates -e into the
    # substitution — verified live in review).
    if (set +e +u
        # shellcheck disable=SC1090
        . "$HOME/.config/axiom/$f" >/dev/null 2>&1); then
        profile=$( (set +e +u
            # shellcheck disable=SC1090
            . "$HOME/.config/axiom/$f" >/dev/null 2>&1
            printf '%s' "${AXIOM_DISPATCHER_PROFILE:-}"))
    else
        missing="$missing\n  $f: does not source cleanly (syntax/unset-var error)"
        continue
    fi
    [ -n "$profile" ] || continue
    [ -x "$PY3" ] || continue # runner python missing is already a named prereq error
    printf '%s' "$profile" | "$PY3" -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null ||
        missing="$missing\n  $f: AXIOM_DISPATCHER_PROFILE is set but does not parse as JSON — single-quote JSON values: AXIOM_DISPATCHER_PROFILE='{\"jobs\": …}' (sh strips double quotes on assignment)"
done
if [ -n "$missing" ]; then
    echo "preflight FAILED — missing prerequisites:$missing" >&2
    exit 1
fi

echo "services to install into $DEST:"
for s in $SERVICES; do echo "  $s -> $DEST/$s.plist (logs: $LOGS/)"; done
[ "$with_bridge" = 1 ] && echo "  com.axiom.carrier-bridge (template — edit __BRIDGE_CMD__ first!)"
echo "Prereqs: /opt/axiom/bin/axiom-ng + /opt/axiom/runner/current (make install), env files in ~/.config/axiom/*.env (0700)."
echo "Proceed? [yes/No]"
read -r answer
[ "$answer" = "yes" ] || {
    echo "aborted"
    exit 1
}

mkdir -p "$DEST" "$LOGS"

install_one() {
    name="$1"
    [ -f "$SRC/$name.plist" ] || {
        echo "missing template $name"
        exit 1
    }
    launchctl bootout "gui/$UID_N/$name" 2>/dev/null || true
    sed "s|__HOME__|$HOME|g" "$SRC/$name.plist" >"$DEST/$name.plist"
    launchctl bootstrap "gui/$UID_N" "$DEST/$name.plist"
    launchctl kickstart -k "gui/$UID_N/$name"
    echo "installed + started: $name"
}

for s in $SERVICES; do install_one "$s"; done
[ "$with_bridge" = 1 ] && { [ -f "$SRC/com.axiom.carrier-bridge.plist" ] && install_one com.axiom.carrier-bridge; }

echo "status:  launchctl print gui/$UID_N/com.axiom.rag   (etc.)"
echo "stop:    launchctl bootout gui/$UID_N/com.axiom.rag"
echo "restart: launchctl kickstart -k gui/$UID_N/com.axiom.rag"
