#!/bin/bash
# dev-up.sh — start the 0.2.x dev environment next to frozen production.
#
#   RAG    : go-built from the working tree,   127.0.0.1:8111
#   Runner : source venv axiom_ng_runner/.venv, 127.0.0.1:8112
#
#   --release (#295): start RAG+Runner from the frozen v0.1.18 release
#   assets (GitHub release, the same bits production runs) instead of
#   working-tree builds. Required for the golden baseline suite — the
#   baseline must witness the freeze bits, not debug builds. Dev DB,
#   dev index, ports and all isolation boundaries stay exactly the same.
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

MODE=source
[ "${1:-}" = "--release" ] && MODE=release
[ $# -le 1 ] || { echo "usage: dev-up.sh [--release]" >&2; exit 2; }

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
STATE="$HOME/.local/state/axiom-dev"

# --- freeze-bit identity (v0.1.18 release, #295) ---------------------------
# The tag is v0.1.18 (commit 4704656); the DEPLOYED RAG binary inside that
# release is the bf77410-generation asset (verified byte-identical with
# /opt/axiom/bin/axiom-ng when present). bf77410..v0.1.18 touches no
# axiom_ng/axiom_ng_runner runtime code (4 commits: fixer/docs/ci only),
# so the bf77410-generation pair IS the freeze state in every observable
# behavior. Runner: same-generation tarball; note the runner generation is
# not hash-pinnable against prod (prod runs a local nix build — #295 debt).
RELEASE_TAG="v0.1.18"
RELEASE_REPO="${AXIOM_RELEASE_REPO:-Cyb3rDudu/axiom}"
RELEASE_GEN="v0.1.17-59-gbf77410"
RAG_ASSET="axiom-ng-$RELEASE_GEN-darwin-arm64"   # prod asset name (darwin, not uname)
RUNNER_ASSET="axiom-runner-$RELEASE_GEN-macos-arm64.tar.zst"

RAG_ENV="${AXIOM_DEV_RAG_ENV:-/run/agenix/axiom-rag.env}"
RAG_API_ENV="${AXIOM_DEV_RAG_API_ENV:-/run/agenix/axiom-rag-api.env}"
RUNNER_ENV="${AXIOM_DEV_RUNNER_ENV:-/run/agenix/axiom-runner.env}"

RAG_PORT=8111
RUNNER_PORT=8112
DEV_DB="axiom_dev"
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
command -v jq >/dev/null || die "jq required"
if [ "$MODE" = source ]; then
    [ -x "$REPO/axiom_ng_runner/.venv/bin/python" ] || die "runner venv missing: $REPO/axiom_ng_runner/.venv"
else
    command -v gh >/dev/null || die "gh required for --release"
    # same preflight as scripts/install_dist.sh (#211): the runner tarball is
    # unpacked via `tar --zstd`; macOS bsdtar resolves the zstd filter from
    # PATH (not from --help text, which does not advertise it)
    command -v zstd >/dev/null || die "zstd required for --release (see scripts/install_dist.sh)"
fi

if [ -f "$STATE/dev.pid" ] && kill -0 "$(awk '$1=="rag"{print $2}' "$STATE/dev.pid")" 2>/dev/null; then
    die "dev environment already running ($STATE/dev.pid) — run dev-down.sh first"
fi
for p in "$RAG_PORT" "$RUNNER_PORT"; do
    lsof -i ":$p" -sTCP:LISTEN >/dev/null 2>&1 && die "port $p already in use"
done

mkdir -p "$STATE"/{logs,artifacts,runner,quarantine,bin,cache/captions}
echo "$MODE" >"$STATE/mode"

# --- provide the RAG binary (and, in release mode, the runner env) ---------

RAG_BIN="$STATE/bin/axiom-ng-dev"   # source mode default: working-tree build
RUNNER_PY="$REPO/axiom_ng_runner/.venv/bin/python"
RUNNER_PYTHONPATH="$REPO/axiom_ng_runner" # source venv needs the package on sys.path

if [ "$MODE" = release ]; then
    REL="$STATE/release"
    mkdir -p "$REL"

    fetch_asset() { # $1 = asset filename (expects <name>.sha256 sidecar too)
        local name="$1" have=""
        [ -f "$REL/$name" ] && have="yes"
        if [ -z "$have" ] || ! (cd "$REL" && shasum -a 256 -c "$name.sha256" >/dev/null 2>&1); then
            note "release: fetching $name from $RELEASE_REPO ${RELEASE_TAG}…"
            gh release download "$RELEASE_TAG" --repo "$RELEASE_REPO" \
                --pattern "$name" --pattern "$name.sha256" --clobber --dir "$REL"
        fi
        (cd "$REL" && shasum -a 256 -c "$name.sha256") || die "release asset checksum FAILED: $name"
    }

    fetch_asset "$RAG_ASSET"
    RAG_BIN="$REL/$RAG_ASSET"
    chmod +x "$RAG_BIN"
    # freeze-bit proof: dev must run the SAME bytes as prod. /opt/axiom is
    # the prod install; absence (non-prod host) downgrades to a note.
    if [ -x /opt/axiom/bin/axiom-ng ]; then
        cmp -s "$RAG_BIN" /opt/axiom/bin/axiom-ng \
            || die "release RAG asset is NOT byte-identical with /opt/axiom/bin/axiom-ng — freeze bits diverged, refusing to start"
        note "release RAG verified byte-identical with /opt/axiom/bin/axiom-ng"
    else
        note "release RAG verified against checksum (no /opt/axiom to compare — non-prod host?)"
    fi

    fetch_asset "$RUNNER_ASSET"
    # unpack once per tarball content (marker = verified sha); conda-unpack
    # is part of the one-time relocation fixup
    RUNNER_SHA="$(awk '{print $1}' "$REL/$RUNNER_ASSET.sha256")"
    RUNNER_REL="$REL/runner"
    if [ "$(cat "$RUNNER_REL/.unpacked_sha" 2>/dev/null || true)" != "$RUNNER_SHA" ]; then
        note "release: unpacking runner artifact…"
        rm -rf "$RUNNER_REL"
        mkdir -p "$RUNNER_REL"
        tar --zstd -xf "$REL/$RUNNER_ASSET" -C "$RUNNER_REL" --strip-components 1
        "$RUNNER_REL/env/bin/python" "$RUNNER_REL/env/bin/conda-unpack" \
            || die "conda-unpack failed for the release runner env"
        echo "$RUNNER_SHA" >"$RUNNER_REL/.unpacked_sha"
    fi
    RUNNER_PY="$RUNNER_REL/env/bin/python"
    RUNNER_PYTHONPATH="" # release env is self-contained; a PYTHONPATH would
                         # let working-tree code shadow the freeze bits
else
    note "building axiom-ng (debug build, working tree)…"
    (cd "$REPO/axiom_ng" && go build -o "$RAG_BIN" ./cmd/axiom-ng)
fi

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
    PYTHONPATH="$RUNNER_PYTHONPATH" # empty in release mode: no working-tree leakage
    export AXIOM_PROCESSOR_PORT AXIOM_PROCESSOR_BIND_ADDR AXIOM_PROCESSOR_WORK_ROOT \
        AXIOM_CAPTION_CACHE_DIR PYTHONPATH
    cd "$STATE"
    exec /usr/bin/python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' \
        "$RUNNER_PY" -m axiom_ng_runner \
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
        "$RAG_BIN" \
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

# release mode must prove the freeze bits are what serves: the build banner
# is the observable identity of the binary (see health_version_test.go).
if [ "$MODE" = release ]; then
    health="$(curl -fsS "http://127.0.0.1:$RAG_PORT/api/health")"
    echo "$health" | jq -e '.build' >/dev/null \
        || { echo "$health" >&2; die "release mode: health has no build banner"; }
    echo "$health" | jq -r '.build' | grep -q 'commit bf77410, release build' \
        || die "release mode: health build is NOT the freeze banner (got: $(echo "$health" | jq -r .build))"
    note "release freeze bits confirmed: $(echo "$health" | jq -r .build)"
fi


note "dev environment up:"
note "  RAG     :$RAG_PORT  (pid $RAG_PID,  log $STATE/logs/rag.log)"
note "  runner  :$RUNNER_PORT  (pid $RUNNER_PID,  log $STATE/logs/runner.log)"
note "  index   : $DEV_INDEX   db: axiom_dev"
note "  stop    : scripts/dev/dev-down.sh"
