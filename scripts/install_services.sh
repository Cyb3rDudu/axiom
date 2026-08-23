#!/bin/sh
# install_services.sh — G3 of #205: installs the launchd service group from
# deploy/launchd/ into ~/Library/LaunchAgents/.
#   · substitutes __HOME__ (launchd never expands $HOME in Standard*Path)
#   · refuses the carrier-bridge template unless --with-bridge + real cmd
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
UID_N="$(id -u)"

with_bridge=0
[ "${1:-}" = "--with-bridge" ] && with_bridge=1

echo "services to install into $DEST:"
for s in $SERVICES; do echo "  $s -> $DEST/$s.plist (logs: $LOGS/)"; done
[ "$with_bridge" = 1 ] && echo "  com.axiom.carrier-bridge (template — edit __BRIDGE_CMD__ first!)"
echo "Prereqs: /opt/axiom/bin/axiom-ng + /opt/axiom/runner/current (make install), env files in ~/.config/axiom/*.env (0700)."
echo "Proceed? [yes/No]"
read -r answer
[ "$answer" = "yes" ] || { echo "aborted"; exit 1; }

mkdir -p "$DEST" "$LOGS"

install_one() {
    name="$1"
    [ -f "$SRC/$name.plist" ] || { echo "missing template $name"; exit 1; }
    launchctl bootout "gui/$UID_N/$name" 2>/dev/null || true
    sed "s|__HOME__|$HOME|g" "$SRC/$name.plist" > "$DEST/$name.plist"
    launchctl bootstrap "gui/$UID_N" "$DEST/$name.plist"
    launchctl kickstart -k "gui/$UID_N/$name"
    echo "installed + started: $name"
}

for s in $SERVICES; do install_one "$s"; done
[ "$with_bridge" = 1 ] && { [ -f "$SRC/com.axiom.carrier-bridge.plist" ] && install_one com.axiom.carrier-bridge; }

echo "status:  launchctl print gui/$UID_N/com.axiom.rag   (etc.)"
echo "stop:    launchctl bootout gui/$UID_N/com.axiom.rag"
echo "restart: launchctl kickstart -k gui/$UID_N/com.axiom.rag"
