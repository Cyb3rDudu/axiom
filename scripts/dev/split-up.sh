#!/bin/bash
# split-up.sh — start the F11 three-process split topology (#305) from the
# working tree, next to the dev environment's shared substrate.
#
#   api     : serve api     127.0.0.1:8111  (the ONE public base URL —
#                                                 golden-suite target)
#   library : serve library 127.0.0.1:8113  + internal edge :8211
#   store   : serve store   127.0.0.1:8114  + internal edge :8212
#                                                 (dispatcher lives here)
#   runner  : the dev compute worker       :8112  (started here unless the
#                                                 dev env already runs it)
#
# Same dev substrate as dev-up.sh: dev DB (axiom_dev), dev index, dev
# artifacts/quarantine, fake Zotero write key, read-only Zotero local API.
# The all-in-one dev RAG must NOT run at the same time (port 8111 is the
# public edge in both topologies — the preflight refuses).
#
# Split acceptance (DoD re-decision 2026-09-28, option b): the CONTRACT-CLASS
# smoke over this topology's public edge — no frozen fixtures here; the freeze
# witness stays all-in-one + release-only (`make golden-baseline` untouched).
#
#   scripts/dev/split-smoke.sh
#
# It proves the typed public shapes (health, search hits, passage translation),
# runs the kill probe (library process killed → typed unavailable envelope,
# leak-free), and verifies split-down.sh leaves nothing behind. The
# AXIOM_BASELINE_EXPECT_BUILD escape in the baseline suite remains as a
# documented 0.2.0 run mode but is NOT part of the split acceptance.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
STATE="$HOME/.local/state/axiom-dev"

API_PORT=8111
RUNNER_PORT=8112
LIB_PORT=8113
STORE_PORT=8114
LIB_EDGE=8211
STORE_EDGE=8212
DEV_DB="axiom_dev"
DEV_INDEX="axiom-dev-chunks-v1"

die() { echo "split-up: $*" >&2; exit 1; }
note() { echo "split-up: $*"; }

RAG_ENV="${AXIOM_DEV_RAG_ENV:-/run/agenix/axiom-rag.env}"
RAG_API_ENV="${AXIOM_DEV_RAG_API_ENV:-/run/agenix/axiom-rag-api.env}"
RUNNER_ENV="${AXIOM_DEV_RUNNER_ENV:-/run/agenix/axiom-runner.env}"
for f in "$RAG_ENV" "$RAG_API_ENV" "$RUNNER_ENV"; do
    [ -r "$f" ] || die "env file not readable: $f"
done
command -v jq >/dev/null || die "jq required"
command -v lsof >/dev/null || die "lsof required (port preflight + pid derivation)"
# The runner venv is only needed when THIS script must start a runner — an
# already-running dev runner (or one from another worktree) is adopted.
RUNNER_VENV="$REPO/axiom-compute-worker/.venv/bin/python"
if ! lsof -i ":$RUNNER_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    [ -x "$RUNNER_VENV" ] || die "runner venv missing: $RUNNER_VENV (and no runner on :$RUNNER_PORT to adopt)"
fi

if [ -f "$STATE/split.pid" ] && kill -0 "$(awk '$1=="api"{print $2}' "$STATE/split.pid")" 2>/dev/null; then
    die "split topology already running ($STATE/split.pid) — run split-down.sh first"
fi
for p in "$API_PORT" "$LIB_PORT" "$STORE_PORT" "$LIB_EDGE" "$STORE_EDGE"; do
    lsof -i ":$p" -sTCP:LISTEN >/dev/null 2>&1 && die "port $p already in use (the all-in-one dev RAG must be down: scripts/dev/dev-down.sh)"
done

mkdir -p "$STATE"/{logs,artifacts,runner,quarantine,bin,cache/captions}
echo "source" >"$STATE/mode"

BIN="$STATE/bin/axiom-split"
note "building axiom (working tree)…"
(cd "$REPO/axiom" && go build -o "$BIN" ./cmd/axiom)

# dev DSN/index derivation (the dev-up boundary: never prod)
dev_dsn() {
    set -a
    # shellcheck disable=SC1090
    . "$RAG_ENV"
    set +a
    printf '%s' "$AXIOM_DATABASE_URL" | sed -E 's#/axiom_db([?]|$)#/'"$DEV_DB"'\1#'
}

# start_proc <name> <port> — one split process: own session (killable by
# PGID), dev-isolated env, role + internal edge per the topology table.
start_proc() {
    local name="$1" port="$2" pid
    (
        set -a
        # shellcheck disable=SC1090
        . "$RAG_ENV"
        # shellcheck disable=SC1090
        . "$RAG_API_ENV"
        set +a
        AXIOM_DATABASE_URL="$(printf '%s' "$AXIOM_DATABASE_URL" | sed -E 's#/axiom_db([?]|$)#/'"$DEV_DB"'\1#')"
        # #358: the sourced operator env may carry the PRODUCTION library
        # DSN — never let the dev split reach it. Unset = single-database
        # topology (both planes on the dev store DB, the supported shape).
        unset AXIOM_LIBRARY_DATABASE_URL
        AXIOM_API_PORT="$port"
        AXIOM_BIND_ADDR=127.0.0.1
        AXIOM_OS_INDEX="$DEV_INDEX"
        AXIOM_PROCESSOR_URLS="http://127.0.0.1:$RUNNER_PORT"
        AXIOM_PROCESSOR_URL="http://127.0.0.1:$RUNNER_PORT"
        AXIOM_QUERY_RUNNER_URL="http://127.0.0.1:$RUNNER_PORT"
        AXIOM_PROCESSOR_RUNNER_NAME=axiom-dev-local
        AXIOM_ARTIFACT_ROOT="$STATE/artifacts"
        AXIOM_QUARANTINE_ROOT="$STATE/quarantine"
        AXIOM_RUNNER_DIR="$REPO/axiom-compute-worker"
        AXIOM_FIXER_INVOKER_ENABLED=0
        AXIOM_ZOTERO_WRITE_KEY_FILE="$STATE/no-write-key"
        AXIOM_DISPATCHER_ENABLED=0
        AXIOM_LIBRARY_IMPORT_PROVIDERS=fake
        case "$name" in
        library)
            AXIOM_INTERNAL_LIBRARY_ADDR="127.0.0.1:$LIB_EDGE"
            ;;
        store)
            AXIOM_INTERNAL_STORE_ADDR="127.0.0.1:$STORE_EDGE"
            AXIOM_DISPATCHER_ENABLED=1
            AXIOM_DISPATCHER_WORKER_ID=axiom-split-store
            # the dispatcher signs processor-source URLs; the runner must
            # fetch them from THIS process (it owns the claim loop)
            AXIOM_PROCESSOR_SOURCE_BASE_URL="http://127.0.0.1:$STORE_PORT"
            ;;
        api)
            AXIOM_LIBRARY_URL="http://127.0.0.1:$LIB_EDGE"
            AXIOM_STORE_URL="http://127.0.0.1:$STORE_EDGE"
            ;;
        esac
        export AXIOM_DATABASE_URL AXIOM_API_PORT AXIOM_BIND_ADDR AXIOM_OS_INDEX \
            AXIOM_PROCESSOR_URLS AXIOM_PROCESSOR_URL AXIOM_QUERY_RUNNER_URL \
            AXIOM_PROCESSOR_RUNNER_NAME AXIOM_ARTIFACT_ROOT AXIOM_QUARANTINE_ROOT \
            AXIOM_RUNNER_DIR AXIOM_FIXER_INVOKER_ENABLED AXIOM_ZOTERO_WRITE_KEY_FILE \
            AXIOM_DISPATCHER_ENABLED AXIOM_LIBRARY_IMPORT_PROVIDERS \
            AXIOM_INTERNAL_LIBRARY_ADDR AXIOM_INTERNAL_STORE_ADDR \
            AXIOM_DISPATCHER_WORKER_ID AXIOM_PROCESSOR_SOURCE_BASE_URL \
            AXIOM_LIBRARY_URL AXIOM_STORE_URL
        cd "$STATE"
        exec /usr/bin/python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' \
            "$BIN" serve "$name" >>"$STATE/logs/split-$name.log" 2>&1
    ) &
    pid=$!
    echo "$name $pid"
}

# --- runner (shared with the dev env when already up) ----------------------

RUNNER_PID=""
if lsof -i ":$RUNNER_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    # Identity probe: adopting means trusting the listener — a foreign or
    # wedged process on the port must fail loudly here, not as a generic
    # search-unready symptom in the smoke.
    curl -fsS -m 5 "http://127.0.0.1:$RUNNER_PORT/v1/health" >/dev/null ||
        die "listener on :$RUNNER_PORT does not answer /v1/health — not a runner; refusing to adopt"
    note "runner already listening on :$RUNNER_PORT — adopting it"
else
    note "starting dev runner on :$RUNNER_PORT …"
    (
        set -a
        # shellcheck disable=SC1090
        . "$RUNNER_ENV"
        set +a
        unset AXIOM_DATABASE_URL
        AXIOM_PROCESSOR_PORT="$RUNNER_PORT"
        AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
        AXIOM_PROCESSOR_WORK_ROOT="$STATE/runner"
        AXIOM_CAPTION_CACHE_DIR="$STATE/cache/captions"
        PYTHONPATH="$REPO/axiom-compute-worker"
        export AXIOM_PROCESSOR_PORT AXIOM_PROCESSOR_BIND_ADDR AXIOM_PROCESSOR_WORK_ROOT \
            AXIOM_CAPTION_CACHE_DIR PYTHONPATH
        cd "$STATE"
        exec /usr/bin/python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' \
            "$RUNNER_VENV" -m axiom_compute_worker \
            >>"$STATE/logs/runner.log" 2>&1
    ) &
    RUNNER_PID=$!
    note "waiting for runner warmup (up to 4 min)…"
    for i in $(seq 1 240); do
        curl -fsS -o /dev/null "http://127.0.0.1:$RUNNER_PORT/v1/health" 2>/dev/null && break
        kill -0 "$RUNNER_PID" 2>/dev/null || { tail -5 "$STATE/logs/runner.log" >&2; die "runner died during warmup"; }
        sleep 1
        [ "$i" = 240 ] && { tail -5 "$STATE/logs/runner.log" >&2; die "runner warmup timeout"; }
    done
fi

# --- the three processes (library first: the api binds to its edge) ---------

PIDS=""
trap '[ -n "$PIDS" ] && kill -TERM -- $PIDS 2>/dev/null || true' EXIT
for spec in "library $LIB_PORT" "store $STORE_PORT" "api $API_PORT"; do
    read -r name port <<<"$spec"
    note "starting $name on :$port …"
    line="$(start_proc "$name" "$port")"
    set -- $line
    PIDS="-$2 $PIDS"
    # wait for the process's own health before the next binds to it
    for i in $(seq 1 60); do
        curl -fsS -o /dev/null "http://127.0.0.1:$port/api/health" 2>/dev/null && break
        kill -0 "$2" 2>/dev/null || { tail -5 "$STATE/logs/split-$name.log" >&2; die "$name died during startup"; }
        sleep 1
        [ "$i" = 60 ] && { tail -5 "$STATE/logs/split-$name.log" >&2; die "$name health timeout (Zotero running?)"; }
    done
done

# pid file (split-down owns the processes from here)
: >"$STATE/split.pid"
for spec in "library $LIB_PORT" "store $STORE_PORT" "api $API_PORT"; do
    read -r name port <<<"$spec"
    pgid="$(lsof -ti tcp:"$port" -sTCP:LISTEN | head -1)"
    [ -n "$pgid" ] || die "no listener on :$port after start"
    echo "$name $pgid" >>"$STATE/split.pid"
done
[ -n "$RUNNER_PID" ] && echo "runner $RUNNER_PID" >>"$STATE/split.pid"
trap - EXIT

note "split topology up (working tree):"
note "  api     :$API_PORT   (public base URL — golden-suite target)"
note "  library :$LIB_PORT   internal edge :$LIB_EDGE"
note "  store   :$STORE_PORT internal edge :$STORE_EDGE (dispatcher here)"
note "  logs      $STATE/logs/split-{api,library,store}.log"
note "  stop      scripts/dev/split-down.sh"
note "acceptance: scripts/dev/split-smoke.sh (see the header of this script)"
