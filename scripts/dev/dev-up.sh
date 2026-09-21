#!/bin/bash
# dev-up.sh — start the 0.2.x dev environment next to frozen production.
#
#   RAG    : go-built from the working tree,   127.0.0.1:8111
#   Runner : source venv axiom_ng_runner/.venv, 127.0.0.1:8112
#
# Isolation boundaries against production (v0.1.18, :8011/:8012, axiom_db,
# axiom-ng-chunks-v1): own DB (axiom_dev), own OpenSearch index
# (AXIOM_OS_INDEX=axiom-dev-chunks-v1), own state dirs, own logs, no Zotero
# write credentials, fixer invoker off. Shared: OpenSearch instance :9200,
# Postgres instance, Zotero local API (read-only).
#
# Env source: production agenix files are sourced for parity values
# (OpenSearch URL, source secret, dispatcher profile, contextual rules,
# caption model) and then explicitly overridden for every isolation
# boundary. Asserts below fail the start if an override did not stick.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
STATE="$HOME/.local/state/axiom-dev"
RAG_ENV="${AXIOM_DEV_RAG_ENV:-/run/agenix/axiom-rag.env}"
RAG_API_ENV="${AXIOM_DEV_RAG_API_ENV:-/run/agenix/axiom-rag-api.env}"
RUNNER_ENV="${AXIOM_DEV_RUNNER_ENV:-/run/agenix/axiom-runner.env}"

RAG_PORT=8111
RUNNER_PORT=8112
DEV_DB=axiom_dev
DEV_INDEX="axiom-dev-chunks-v1"
PROD_INDEX="axiom-ng-chunks-v1"

die() {
    echo "dev-up: $*" >&2
    exit 1
}
note() { echo "dev-up: $*"; }

# --- preflight -------------------------------------------------------------

for f in "$RAG_ENV" "$RAG_API_ENV" "$RUNNER_ENV"; do
    [ -r "$f" ] || die "env file not readable: $f"
done
[ -x "$REPO/axiom_ng_runner/.venv/bin/python" ] || die "runner venv missing: $REPO/axiom_ng_runner/.venv"
command -v jq >/dev/null || die "jq required"

if [ -f "$STATE/dev.pid" ] && kill -0 "$(awk '$1=="rag"{print $2}' "$STATE/dev.pid")" 2>/dev/null; then
    die "dev environment already running ($STATE/dev.pid) — run dev-down.sh first"
fi
for p in "$RAG_PORT" "$RUNNER_PORT"; do
    lsof -i ":$p" -sTCP:LISTEN >/dev/null 2>&1 && die "port $p already in use"
done

mkdir -p "$STATE"/{logs,artifacts,runner,quarantine,bin,cache/captions}

# --- build RAG from the working tree ---------------------------------------

note "building axiom-ng (debug build, working tree)…"
(cd "$REPO/axiom_ng" && go build -o "$STATE/bin/axiom-ng-dev" ./cmd/axiom-ng)

# --- bootstrap the dev OpenSearch index -------------------------------------
# Same instance as prod; own namespace. Created from the prod index's
# mappings+settings (cluster metadata stripped) and filled via _reindex, so
# dev search is immediately fully functional. Prod stays writable throughout.
# A MISSING index is bootstrapped with an exact count check; an EXISTING one
# is adopted as-is (it lives its own life — counts drift legitimately once
# dev ingests, so no re-verification on skip). A failed/partial bootstrap
# deletes the index again instead of leaving a silent stub for the next run.

set -a
# shellcheck disable=SC1090
. "$RAG_ENV"
set +a
OS_URL="${AXIOM_OPENSEARCH_URL:-http://127.0.0.1:9200}"

if curl -fsS -o /dev/null "$OS_URL/$DEV_INDEX"; then
    note "dev index $DEV_INDEX present ($(curl -fsS "$OS_URL/$DEV_INDEX/_count" | jq -r .count) docs) — adopted as-is"
else
    note "bootstrapping $DEV_INDEX from $PROD_INDEX (mappings+settings+_reindex)…"
    curl -fsS "$OS_URL/$PROD_INDEX" |
        jq --arg prod "$PROD_INDEX" \
            '{settings: (.[$prod].settings.index | del(.uuid,.version,.creation_date,.provided_name)), mappings: .[$prod].mappings}' |
        curl -fsS -XPUT "$OS_URL/$DEV_INDEX" -H 'Content-Type: application/json' -d @- >/dev/null ||
        die "index create failed"
    if ! curl -fsS -XPOST "$OS_URL/_reindex?wait_for_completion=true" -H 'Content-Type: application/json' \
        -d "{\"source\":{\"index\":\"$PROD_INDEX\"},\"dest\":{\"index\":\"$DEV_INDEX\"}}" >/dev/null; then
        curl -fsS -XDELETE "$OS_URL/$DEV_INDEX" >/dev/null || true
        die "_reindex failed (partial index deleted — retry dev-up)"
    fi
    # HTTP 200 can still carry per-doc failures; at bootstrap time the copy
    # must be exact (prod is frozen — no concurrent ingest expected)
    prod_n="$(curl -fsS "$OS_URL/$PROD_INDEX/_count" | jq -r .count)"
    dev_n="$(curl -fsS "$OS_URL/$DEV_INDEX/_count" | jq -r .count)"
    if [ "$dev_n" != "$prod_n" ]; then
        curl -fsS -XDELETE "$OS_URL/$DEV_INDEX" >/dev/null || true
        die "bootstrap incomplete: $dev_n/$prod_n docs (partial index deleted — retry dev-up)"
    fi
    note "dev index ready: $dev_n docs (count-verified against $PROD_INDEX)"
fi

# --- start the runner (source venv, :8112) ----------------------------------
# Own session/process group (python setsid helper — macOS has no setsid(1)),
# so dev-down can kill the whole group by PGID.

note "starting dev runner on :$RUNNER_PORT …"
(
    set -a
    # shellcheck disable=SC1090
    . "$RUNNER_ENV"
    set +a
    # the bootstrap section above sourced the PROD rag env (set -a) into this
    # shell — the runner service never reads a DSN, but the inherited prod
    # value is a latent trap for anything ever run inside this environment
    unset AXIOM_DATABASE_URL
    AXIOM_PROCESSOR_PORT="$RUNNER_PORT"
    AXIOM_PROCESSOR_BIND_ADDR=127.0.0.1
    AXIOM_PROCESSOR_WORK_ROOT="$STATE/runner"
    AXIOM_CAPTION_CACHE_DIR="$STATE/cache/captions"
    PYTHONPATH="$REPO/axiom_ng_runner"
    export AXIOM_PROCESSOR_PORT AXIOM_PROCESSOR_BIND_ADDR AXIOM_PROCESSOR_WORK_ROOT \
        AXIOM_CAPTION_CACHE_DIR PYTHONPATH
    cd "$REPO/axiom_ng_runner"
    exec /usr/bin/python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' \
        "$REPO/axiom_ng_runner/.venv/bin/python" -m axiom_ng_runner \
        >>"$STATE/logs/runner.log" 2>&1
) &
RUNNER_PID=$!
# orphan guard: if anything below fails before dev.pid is written, dev-down
# refuses to run (no pid file) — so this script must not leave the runner
# behind. Re-armed below to cover the RAG group too; cleared on success.
trap 'kill -TERM -- "-$RUNNER_PID" 2>/dev/null || true' EXIT

note "waiting for runner warmup (MPS model load, up to 4 min)…"
for i in $(seq 1 240); do
    curl -fsS -o /dev/null "http://127.0.0.1:$RUNNER_PORT/v1/health" 2>/dev/null && break
    kill -0 "$RUNNER_PID" 2>/dev/null || {
        tail -5 "$STATE/logs/runner.log" >&2
        die "runner died during warmup"
    }
    sleep 1
    [ "$i" = 240 ] && {
        tail -5 "$STATE/logs/runner.log" >&2
        die "runner warmup timeout"
    }
done
note "runner warm (pid $RUNNER_PID)"

# --- start the RAG (:8111) ---------------------------------------------------

note "starting dev RAG on :$RAG_PORT …"
(
    set -a
    # shellcheck disable=SC1090
    . "$RAG_ENV"
    # shellcheck disable=SC1090
    . "$RAG_API_ENV"
    set +a
    # isolation overrides (every boundary prod shares with dev gets its own)
    AXIOM_DATABASE_URL="$(printf '%s' "$AXIOM_DATABASE_URL" | sed -E 's#/axiom_db([?]|$)#/'"$DEV_DB"'\1#')"
    AXIOM_API_PORT="$RAG_PORT"
    AXIOM_BIND_ADDR=127.0.0.1
    AXIOM_OS_INDEX="$DEV_INDEX"
    AXIOM_PROCESSOR_URLS="http://127.0.0.1:$RUNNER_PORT"
    AXIOM_PROCESSOR_URL="http://127.0.0.1:$RUNNER_PORT"
    AXIOM_QUERY_RUNNER_URL="http://127.0.0.1:$RUNNER_PORT"
    AXIOM_PROCESSOR_SOURCE_BASE_URL="http://127.0.0.1:$RAG_PORT"
    AXIOM_PROCESSOR_RUNNER_NAME=axiom-dev-local
    AXIOM_ARTIFACT_ROOT="$STATE/artifacts"
    AXIOM_QUARANTINE_ROOT="$STATE/quarantine"
    # runner-checkout discovery for the *-backfill cmd tools when run against dev
    AXIOM_RUNNER_DIR="$REPO/axiom_ng_runner"
    AXIOM_FIXER_INVOKER_ENABLED=0
    # never inherit prod's Zotero write credentials: point the key file at a
    # path that must not exist → repair API stays disabled in dev
    AXIOM_ZOTERO_WRITE_KEY_FILE="$STATE/no-write-key"
    AXIOM_DISPATCHER_ENABLED=1
    AXIOM_DISPATCHER_WORKER_ID=axiom-dev
    export AXIOM_DATABASE_URL AXIOM_API_PORT AXIOM_BIND_ADDR AXIOM_OS_INDEX \
        AXIOM_PROCESSOR_URLS AXIOM_PROCESSOR_URL AXIOM_QUERY_RUNNER_URL \
        AXIOM_PROCESSOR_SOURCE_BASE_URL AXIOM_PROCESSOR_RUNNER_NAME \
        AXIOM_ARTIFACT_ROOT AXIOM_QUARANTINE_ROOT AXIOM_RUNNER_DIR \
        AXIOM_FIXER_INVOKER_ENABLED AXIOM_ZOTERO_WRITE_KEY_FILE \
        AXIOM_DISPATCHER_ENABLED AXIOM_DISPATCHER_WORKER_ID

    # hard asserts: an override that silently did not stick would aim dev at prod
    case "$AXIOM_DATABASE_URL" in
    *"/$DEV_DB" | *"/$DEV_DB"[?]*) ;;
    *"/axiom_db"*)
        echo "dev-up: DATABASE_URL still points at prod (axiom_db) — sed rewrite failed" >&2
        exit 1
        ;;
    *)
        echo "dev-up: DATABASE_URL override failed" >&2
        exit 1
        ;;
    esac
    [ "$AXIOM_OS_INDEX" = "$DEV_INDEX" ] || {
        echo "dev-up: OS_INDEX override failed" >&2
        exit 1
    }
    [ "$AXIOM_API_PORT" = "$RAG_PORT" ] || {
        echo "dev-up: API_PORT override failed" >&2
        exit 1
    }
    [ ! -e "$AXIOM_ZOTERO_WRITE_KEY_FILE" ] || {
        echo "dev-up: write-key path exists" >&2
        exit 1
    }

    exec /usr/bin/python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' \
        "$STATE/bin/axiom-ng-dev" \
        >>"$STATE/logs/rag.log" 2>&1
) &
RAG_PID=$!
# orphan guard now covers both groups (the RAG-health-timeout die path leaves
# a LIVE RAG behind — the runner-only trap above would not catch it)
trap 'kill -TERM -- "-$RUNNER_PID" "-$RAG_PID" 2>/dev/null || true' EXIT

note "waiting for RAG health on :$RAG_PORT …"
for i in $(seq 1 90); do
    if curl -fsS "http://127.0.0.1:$RAG_PORT/api/health" 2>/dev/null | grep -q '"ok":true'; then break; fi
    kill -0 "$RAG_PID" 2>/dev/null || {
        tail -5 "$STATE/logs/rag.log" >&2
        die "RAG died during startup"
    }
    sleep 1
    [ "$i" = 90 ] && {
        tail -5 "$STATE/logs/rag.log" >&2
        die "RAG health timeout (Zotero running?)"
    }
done

printf 'rag %s\nrunner %s\n' "$RAG_PID" "$RUNNER_PID" >"$STATE/dev.pid"
trap - EXIT # success: both services stay up, dev-down.sh owns them from here

note "dev environment up:"
note "  RAG     :$RAG_PORT  (pid $RAG_PID,  log $STATE/logs/rag.log)"
note "  runner  :$RUNNER_PORT  (pid $RUNNER_PID,  log $STATE/logs/runner.log)"
note "  index   : $DEV_INDEX   db: axiom_dev"
note "  stop    : scripts/dev/dev-down.sh"
