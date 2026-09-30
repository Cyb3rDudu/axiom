#!/bin/bash
# qos-probe.sh — reproducible scheduling-class sonde (F14 #308, from the
# diagnostics 2026-09-21 OCRmyPDF background-QoS report).
#
# What it measures: whether the CURRENT process tree gets full-core
# scheduling. It runs N parallel CPU-bound children (pure sha256 loops,
# no I/O, no models), then reports wall time, summed CPU time, and mean
# core occupancy. Under a launchd Background coalition macOS pins the
# work largely to efficiency cores — a 12-child probe that would occupy
# ~9-11 cores under Standard collapses to ~2-3 mean cores under
# Background (the report's controlled A/B/C: 82.56 s vs 8.81 s vs 7.45 s;
# 9.4x Standard-to-Background slowdown).
#
# Usage:
#   deploy/launchd/qos-probe.sh                     # measure in place
#   deploy/launchd/qos-probe.sh --launchd Standard  # measure under a
#   deploy/launchd/qos-probe.sh --launchd Background  # temporary launchd
#   deploy/launchd/qos-probe.sh --launchd Interactive # job (A/B/C ride)
#
# The A/B an operator runs before trusting a ProcessType change:
#   qos-probe.sh --launchd Background
#   qos-probe.sh --launchd Standard
# and compares the two "mean cores" lines — the gap IS the scheduling
# class, everything else (binary, input, machine state) is identical.
#
# Dependencies: python3 (the repo's compute-worker floor), launchd
# (macOS; --launchd mode only). No product code is touched.
set -euo pipefail

PROBE="$(cd "$(dirname "$0")" && pwd)/qos-probe.sh"
CHILDREN="${AXIOM_QOS_PROBE_CHILDREN:-12}"
ROUNDS="${AXIOM_QOS_PROBE_ROUNDS:-4000000}"

die() { echo "qos-probe: $*" >&2; exit 1; }
command -v python3 >/dev/null || die "python3 required"

# --- the CPU-bound measurement (in-place) ------------------------------------
#
# Each child burns ROUNDS sha256 updates and prints its own process CPU
# seconds; the parent sums them against the wall clock. Children run in
# their own process group so a Ctrl-C reaps everything.
measure() {
    local label="$1" t0 t1 wall cpu
    # time.time() (not monotonic): macOS python's monotonic clock is
    # per-process — the two subprocesses would compare unrelated bases.
    t0=$(python3 -c 'import time; print(time.time_ns())')
    cpu=$(
        for _ in $(seq 1 "$CHILDREN"); do
            python3 - "$ROUNDS" <<'EOF' &
import hashlib, sys, time
rounds = int(sys.argv[1])
h = hashlib.sha256()
start = time.process_time_ns()
block = b"qos-probe: a cpu-bound child of the axiom scheduling sonde"
for _ in range(rounds):
    h.update(block)
print((time.process_time_ns() - start) / 1e9)
EOF
        done | python3 -c 'import sys; print(f"{sum(float(l) for l in sys.stdin):.2f}")'
    )
    t1=$(python3 -c 'import time; print(time.time_ns())')
    wall=$(python3 -c "print(f'{($t1 - $t0) / 1e9:.2f}')")
    wait
    python3 - "$label" "$CHILDREN" "$wall" "$cpu" <<'EOF'
import sys
label, children, wall, cpu = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
cores = cpu / wall if wall else 0.0
print(f"qos-probe [{label}]: children={children} wall={wall:.2f}s cpu={cpu:.2f}s mean-cores={cores:.2f}")
EOF
}

# --- launchd mode: same measurement inside a temp job of a given class -------
# The launchd job does NOT inherit the caller's environment — the A/B/C
# legs always run the DEFAULT workload (12 children, default rounds),
# which is exactly what comparability wants: identical work, only the
# scheduling class differs.
launchd_measure() {
    local ptype="$1" uid line i
    uid="$(id -u)"
    PROBE_LABEL="com.axiom.qos-probe"
    PROBE_OUT="$(mktemp -t qos-probe).log"
    PROBE_PLIST="$(mktemp -t qos-probe).plist"
    trap 'rm -f "$PROBE_OUT" "$PROBE_PLIST"; launchctl bootout "gui/$(id -u)/$PROBE_LABEL" 2>/dev/null || true' EXIT
    cat >"$PROBE_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$PROBE_LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$PROBE</string>
        <string>--quiet-measure</string>
        <string>launchd-$ptype</string>
    </array>
    <key>ProcessType</key><string>$ptype</string>
    <key>RunAtLoad</key><true/>
    <key>StandardOutPath</key><string>$PROBE_OUT</string>
    <key>StandardErrorPath</key><string>$PROBE_OUT</string>
</dict>
</plist>
EOF
    launchctl bootstrap "gui/$uid" "$PROBE_PLIST" 2>/dev/null ||
        die "launchctl bootstrap failed (run as the console user, not root)"
    # the job is one-shot (RunAtLoad, no KeepAlive): done when the result
    # line lands
    for i in $(seq 1 300); do
        line="$(grep -h 'mean-cores=' "$PROBE_OUT" 2>/dev/null || true)"
        [ -n "$line" ] && { echo "$line"; break; }
        [ "$i" = 300 ] && die "probe job did not report within 5 min (see $PROBE_OUT)"
        sleep 1
    done
    # bootout is ASYNCHRONOUS: bootstrapping the same label again (the
    # A/B/C legs run back-to-back) races the teardown with EIO. Tear
    # down here and wait for the label to actually leave the domain.
    launchctl bootout "gui/$uid/$PROBE_LABEL" 2>/dev/null || true
    for i in $(seq 1 60); do
        launchctl print "gui/$uid/$PROBE_LABEL" >/dev/null 2>&1 || break
        sleep 0.5
    done
}

case "${1:-}" in
"") measure "in-place" ;;
--quiet-measure) measure "${2:-launchd}" ;;
--launchd)
    case "${2:-}" in
    Background | Standard | Interactive) ;;
    *) die "usage: qos-probe.sh --launchd Background|Standard|Interactive" ;;
    esac
    launchd_measure "$2"
    ;;
*) die "usage: qos-probe.sh [--launchd Background|Standard|Interactive]" ;;
esac
