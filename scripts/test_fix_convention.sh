#!/bin/sh
# test_fix_convention.sh — proves the fix.sh invocation contract (#205 G3):
# key required, serialization per key, missing-install failure, happy exec.
# Runs with fake interpreter/app, no /opt needed. Wired into `make test`.
set -eu
cd "$(dirname "$0")/.."
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP" # lockdir lands in the sandbox

FAKE="$TMP/fakepy"
printf '#!/bin/sh\necho "FAKE-RUN key=$2 $3"\n' >"$FAKE"
chmod +x "$FAKE"
export AXIOM_FIXER="$FAKE"
export AXIOM_FIXER_APP="$TMP/app.py"

fail=0
# 1) key required
if scripts/fix.sh >/dev/null 2>&1; then
    echo "FAIL: no key accepted"
    fail=1
else echo "ok: key required"; fi
# 2) happy path executes the agent (glob both sides: a no-timeout-binary
#    warning line may precede the output on stock macOS)
out=$(scripts/fix.sh ABC123 --apply 2>&1)
case "$out" in *FAKE-RUN*) echo "ok: exec" ;; *)
    echo "FAIL exec: $out"
    fail=1
    ;;
esac
# 3) lock released after run
[ -d "$TMP/.local/state/axiom/runs/fix-ABC123.lock" ] && {
    echo "FAIL: lock leaked"
    fail=1
} || echo "ok: lock released"
# 4) concurrent same-key is rejected (exit 3); a lock WITHOUT a pid file is
#    treated as actively held (conservative)
mkdir -p "$TMP/.local/state/axiom/runs/fix-ABC123.lock"
if scripts/fix.sh ABC123 >/dev/null 2>&1; then
    echo "FAIL: concurrent run accepted"
    fail=1
else
    rc=$?
    [ "$rc" = 3 ] && echo "ok: serialized (exit 3)" || {
        echo "FAIL rc=$rc"
        fail=1
    }
fi
rmdir "$TMP/.local/state/axiom/runs/fix-ABC123.lock"
# 5) stale lock (dead pid) is auto-recovered, run succeeds
mkdir -p "$TMP/.local/state/axiom/runs/fix-STALE1.lock"
printf '999999\n' >"$TMP/.local/state/axiom/runs/fix-STALE1.lock/pid"
if out=$(scripts/fix.sh STALE1 2>&1); then echo "ok: stale lock recovered"; else
    echo "FAIL stale lock: $out"
    fail=1
fi
# 6) missing install fails fast
AXIOM_FIXER="$TMP/nonexistent" scripts/fix.sh XYZ >/dev/null 2>&1 && {
    echo "FAIL: missing install accepted"
    fail=1
} || echo "ok: missing install rejected"
# 7) the REAL agent CLI keeps the contract shape: --key and --apply exist
AGENT_PY="axiom_ng/tools/pdf_repair_agent/.venv/bin/python"
[ -x "$AGENT_PY" ] || AGENT_PY="python3"
if help=$($AGENT_PY axiom_ng/tools/pdf_repair_agent/repair_agent.py --help 2>&1); then
    case "$help" in *--key*--apply* | *--apply*--key*) echo "ok: agent CLI shape (--key/--apply)" ;; *)
        echo "FAIL: agent help lost --key/--apply"
        fail=1
        ;;
    esac
else
    echo "SKIP: agent CLI probe (no interpreter)"
fi

[ "$fail" = 0 ] && echo "test_fix_convention: ALL OK" || exit 1
