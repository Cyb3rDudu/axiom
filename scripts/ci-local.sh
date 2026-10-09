#!/bin/bash
# ci-local.sh — the whole local test pipeline in ONE command.
#
# Background (#354 CI cost discipline, decision 2026-10-06): GitHub CI
# runs only as the fidelity check on a PR against main; branch pushes
# bill nothing, and strand verification happens locally. This script is
# that local verification: it chains ONLY existing, proven parts in CI
# order — it invents no checks and changes no workflow file.
#
#   (0) drift preflight    host clock vs the local podman VM clock
#   (1) fix-convention     scripts/test_fix_convention.sh
#   (2) go vet + go unit   DB-gated suites skip by design (like go-unit)
#   (3) DB legs, natively  scratch setup → go-db-it · golden-baseline ·
#                          library-engine-postgres · library-engine-sqlite
#                          → teardown (also on abort: trap)
#   (4) runner/fixer pytest (venv suites, same invocations as `make test`)
#   (5) docs gate          naming gate + mkdocs build --strict
#   (6) --with-topology    split-up → split-smoke (smoke runs split-down)
#       --with-act         act replay of the service-free CI jobs
#
# The scratch-DB conventions: the suites' `_test` guard refuses any DSN
# whose database does not end in _test, and each suite creates/migrates/
# drops its OWN ephemeral <base>_..._test databases. Every run derives
# its OWN pid-suffixed base (ci_local_base_<pid>_test) — no shared
# object exists between runs, so parallel runs of different agents
# coexist cleanly on the cluster (that is what the *_test isolation is
# built for). Foreign bases (e.g. a scratch_base_test of another agent)
# are NEVER used and NEVER dropped. axiom_ci_test is the baseline leg's
# admin channel: that exact name is on the baseline suite's frozen
# scratchableDSNs allowlist (name-based by design — the other allowlisted
# names lack the _test suffix the suites' guard requires, so renaming
# would take test changes); the suite writes nothing into it.
# Suite-internal FIXED names cannot be per-run (test-code consts):
# go-db-it refreshes axiom_mirror_it_test, axiom_repair_test,
# axiom_repo_test and axiom_server_test every run; teardown additionally
# idle-drops axiom_baseline_scratch (the baseline suite's scratch).
# ci-local serializes the legs touching them across its own concurrent
# runs (mkdir lock) and refreshes/drops them only when no other session
# is connected — in-use databases are never touched. The DSN is derived read-only from the environment's database
# URL with the name REWRITTEN; the source variables are never exported.
# The role drill (AXIOM_REQUIRE_DRILL) stays CI-exclusive: a cluster
# with standing production roles must skip it, never fail it.
# Fail-fast: fast legs first, abort on first red leg.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

# --- knobs (overridable defaults mirror the sibling dev scripts) -----------
DB_CONTAINER="${AXIOM_CI_DB_CONTAINER:-postgres}"
RAG_ENV="${AXIOM_DEV_RAG_ENV:-/run/agenix/axiom-rag.env}"
DRIFT_TOLERANCE="${AXIOM_CI_DRIFT_TOLERANCE:-120}"
RUN_BASE="ci_local_base_$$_test" # OWN per-run base: parallel runs never share an object
BASELINE_DB="axiom_ci_test" # frozen scratchableDSNs allowlist name
ACT_JOBS="go-lint go-unit library-engine-sqlite runner-pytest fixer-pytest"

WITH_TOPOLOGY=0
WITH_ACT=0
for arg in "$@"; do
    case "$arg" in
    --with-topology) WITH_TOPOLOGY=1 ;;
    --with-act) WITH_ACT=1 ;;
    *)
        echo "usage: ci-local.sh [--with-topology] [--with-act]" >&2
        exit 2
        ;;
    esac
done

# --- plumbing --------------------------------------------------------------
LEG_SKIP=77 # exit code a leg function returns to soft-skip (tool missing)
LEG_NAMES=()
LEG_STATUS=()
LEG_SECS=()

now() { date +%s; } # integer seconds — sub-second legs honestly show 0s

run_leg() { # run_leg <name> <func> [lock]: subshell → no cd/env leaks
    # The optional lock serializes the leg across CONCURRENT ci-local runs
    # on this host: the go legs share the Go build cache (concurrent
    # full-tree test builds race the cache trim) and two of them touch
    # suite-internal FIXED database names. Parallel runs stay DB-isolated
    # per run — this only takes turns, never shares state.
    local name="$1" func="$2" lock="${3:-}" t0 t1 rc status
    echo "ci-local: ── leg $(( ${#LEG_STATUS[@]} + 1 )): $name"
    t0="$(now)"
    rc=0
    if [ -n "$lock" ]; then
        ( with_runlock "$lock" "$func" ) || rc=$?
    else
        ( "$func" ) || rc=$?
    fi
    t1="$(now)"
    case "$rc" in
    0) status="PASS" ;;
    "$LEG_SKIP") status="SKIP" ;;
    *) status="FAIL" ;;
    esac
    LEG_NAMES+=("$name")
    LEG_STATUS+=("$status")
    LEG_SECS+=($((t1 - t0)))
    echo "ci-local: [$status] $name (${LEG_SECS[${#LEG_SECS[@]} - 1]}s)"
    if [ "$status" = FAIL ]; then
        echo "ci-local: FAIL-FAST on '$name' — teardown runs, later legs skipped" >&2
        exit 1
    fi
}

masked_dsn() { # never print credentials: strip userinfo
    printf '%s' "$1" | sed -E 's#(//)[^@/]+@#\1***@#'
}

go_env() { # go_env [VAR=val ...] <cmd...>: CI-fidelity env scrub
    # Every go leg runs through this: the operator's shell may export
    # AXIOM_TEST_DATABASE_URL / AXIOM_BASELINE_DSN / AXIOM_REQUIRE_DRILL
    # — unset all three; callers re-add exactly the one they mean (env
    # applies -u flags and assignments left to right). The role drill
    # stays CI-exclusive: never set locally, not even by inheritance.
    env -u AXIOM_TEST_DATABASE_URL -u AXIOM_BASELINE_DSN -u AXIOM_REQUIRE_DRILL "$@"
}

running_machine() { # name of the running podman machine, "" if none
    podman machine list --format '{{.Name}}|{{.LastUp}}' 2>/dev/null |
        awk -F'|' '/Currently running/{print $1; exit}' || true
}

dsn_dbname() { # database path component of a URL DSN ("" if none)
    printf '%s' "$1" | sed -nE 's#^[a-zA-Z0-9+]+://[^/]+/([^/?]+).*$#\1#p'
}

dsn_user() { # userinfo user; "" when the DSN carries no userinfo part
    # (without the @ anchor a userinfo-less DSN would yield its HOST as
    # the user — the caller's "cannot parse user" abort must fire)
    printf '%s' "$1" | sed -nE 's#^[a-zA-Z0-9+]+://([^:@/?]+)[^@]*@.*$#\1#p'
}

dsn_with_db() { # dsn_with_db <dsn> <dbname>: rewrite the database component
    local cur
    cur="$(dsn_dbname "$1")"
    [ -n "$cur" ] || return 1
    printf '%s' "$1" | sed -E "s#/${cur}([?]|$)#/$2\1#"
}

psql_admin() { # psql on the scratch cluster (container-local unix socket)
    podman exec "$DB_CONTAINER" psql -U "$PGUSER" -d postgres -tAq "$@"
}

# --- (0) drift preflight ----------------------------------------------------
# Known relapse after host sleep: the VM clock drifts while chrony still
# reports synchronized — timestamp-based tests then fail spuriously and
# apt refuses packages. Compare epochs BEFORE any leg burns time.
leg_drift() {
    local host_now vm_now diff machine hint
    host_now="$(date +%s)"
    if ! vm_now="$(podman exec "$DB_CONTAINER" date +%s 2>/dev/null)"; then
        echo "ci-local: cannot exec '$DB_CONTAINER' — is the local podman machine running and the database container up?" >&2
        return 1
    fi
    diff=$((host_now - vm_now))
    [ "$diff" -lt 0 ] && diff=$((-diff))
    if [ "$diff" -gt "$DRIFT_TOLERANCE" ]; then
        machine="$(running_machine)"
        hint="sudo chronyc -a makestep"
        [ -n "$machine" ] && hint="podman machine ssh $machine \"$hint\""
        echo "ci-local: VM clock drift ${diff}s > ${DRIFT_TOLERANCE}s — timestamp-based tests would fail spuriously (known relapse after host sleep)." >&2
        echo "ci-local: repair in the VM, then re-run:  $hint" >&2
        return 1
    fi
    echo "ci-local: drift ${diff}s (tolerance ${DRIFT_TOLERANCE}s) — ok"
}

# --- (1) fix convention -----------------------------------------------------
leg_fix_convention() {
    "$REPO/scripts/test_fix_convention.sh"
}

# --- (2) Go unit legs (DB suites skip by design, like CI go-unit) -----------
leg_go_fmt() { # the CI go-lint leg's core: gofmt-clean tree (cheap, mandatory)
    cd "$REPO/axiom" || return 1
    local bad
    bad="$(gofmt -l .)" || return 1
    if [ -n "$bad" ]; then
        echo "ci-local: [FAIL] go-fmt — not gofmt-clean:" >&2
        printf '%s\n' "$bad" >&2
        return 1
    fi
    echo "ci-local: go-fmt clean"
}

leg_go_vet() {
    cd "$REPO/axiom" && go vet ./...
}

leg_go_unit() {
    cd "$REPO/axiom" &&
        go_env go test -count=1 ./...
}

# --- (3) DB legs, natively --------------------------------------------------
# CI-fidelity invocations (ci.yml) with the live-validation findings
# applied: every DB-touching run carries -p 1 -count=1 (parallel package
# binaries contend on the shared DSN database — FK violations, lost rows,
# load flakes), and AXIOM_REQUIRE_DRILL is NEVER set locally: the role
# drill belongs to the disposable CI clusters and skips here by design.
leg_go_db_it() { # runs under the dbshared lock (call site)
    # The persistent FIXED-name IT databases (test-harness constants —
    # the suites migrate them idempotently, so a reused database keeps a
    # stale schema shape and drifted data across runs: 42P10/23503
    # failures). Refreshed here every run, idle-guarded, under the lock;
    # extend the list when a new fixed-name suite lands.
    for db in axiom_mirror_it_test axiom_repair_test axiom_repo_test axiom_server_test; do
        drop_db_if_idle "$db" "fixed-name IT refresh"
    done
    local rc=0
    cd "$REPO/axiom" &&
        go_env AXIOM_TEST_DATABASE_URL="$IT_DSN" \
            go test -p 1 -count=1 ./... || rc=$?
    return "$rc"
}

leg_golden_baseline() { # runs under the dbshared lock (call site)
    cd "$REPO/axiom" &&
        go_env AXIOM_BASELINE_DSN="$BASE_DSN" \
            go test -p 1 -count=1 -v ./internal/baseline
}

leg_library_engine_postgres() {
    # errexit is OFF inside leg subshells (run_leg's `||` idiom): chain
    # the invocations explicitly so an early failure is never masked by
    # a later one passing
    local rc=0
    cd "$REPO/axiom" || return 1
    go_env AXIOM_TEST_DATABASE_URL="$IT_DSN" \
        go test -p 1 -count=1 -v ./internal/library/reposuite ./internal/library/sqlite ./internal/library/pglib \
        -run 'TestRepositoryContractSuite|TestSkipGuardProbes' || rc=$?
    if [ "$rc" -eq 0 ]; then
        go_env AXIOM_TEST_DATABASE_URL="$IT_DSN" \
            go test -p 1 -count=1 -v ./internal/databundle || rc=$?
    fi
    return "$rc"
}

leg_library_engine_sqlite() { # PG-free is the point: no DSN anywhere
    # same explicit chaining as the postgres leg (see its comment)
    local rc=0
    cd "$REPO/axiom" || return 1
    go_env go test -p 1 -count=1 -v ./internal/library/reposuite ./internal/library/sqlite \
        -run 'TestRepositoryContractSuite|TestSkipGuardProbes' || rc=$?
    if [ "$rc" -eq 0 ]; then
        go_env go test -p 1 -count=1 ./internal/databundle || rc=$?
    fi
    return "$rc"
}

# --- (4) Python suites (same invocations as `make test`) --------------------
# The venvs are CHECKED PRECONDITIONS — a missing venv SKIPs the leg with
# a bootstrap hint; building venvs is not this pipeline's business.
leg_runner_pytest() {
    [ -x "$REPO/axiom-compute-worker/.venv/bin/python" ] || {
        echo "ci-local: compute-worker venv missing — SKIP (bootstrap axiom-compute-worker/.venv first, then re-run)" >&2
        return "$LEG_SKIP"
    }
    cd "$REPO/axiom-compute-worker" && .venv/bin/python -m pytest -q
}

leg_fixer_pytest() {
    [ -x "$REPO/axiom-fixer/.venv/bin/python" ] || {
        echo "ci-local: fixer venv missing — SKIP (run axiom-fixer/bootstrap.sh, then re-run)" >&2
        return "$LEG_SKIP"
    }
    cd "$REPO/axiom-fixer" && .venv/bin/python -m pytest -q
}

# --- (5) docs gate, natively (docs.yml shapes) ------------------------------
leg_docs() {
    cd "$REPO" || return 1
    # naming gate — verbatim from the docs workflow (product name 'axiom')
    ADRROWS='^docs/adr/0001[^:]*\.md:\|'
    MIG='^docs/operations/migration-g[^:]*\.md:\|'
    OPSROWS='^docs/operations/deprecations\.md:\|'
    if {
        grep -riH 'axiom-ng' docs/ --include='*.md'
        grep -iH 'axiom-ng' mkdocs.yml
    } |
        grep -v '_inventory.md' |
        grep -vF 'was known as *axiom-ng*' |
        grep -vE '(go run|go build).*cmd/axiom-ng' |
        grep -vE '(\./)?axiom-ng +-' |
        grep -vF 'axiom-ng-<version>' |
        grep -vF 'axiom-ng, axiom-compute-worker' |
        grep -vE "$ADRROWS" |
        grep -vE "$MIG" |
        grep -vE "$OPSROWS"; then
        echo "ci-local: legacy product name 'axiom-ng' found in site content" >&2
        return 1
    fi
    python3 -m mkdocs build --clean --strict
}

# --- (6) optional flag legs -------------------------------------------------
leg_topology() { # boots the split topology; split-smoke tears it down
    local t
    for t in jq curl lsof; do
        command -v "$t" >/dev/null || {
            echo "ci-local: --with-topology needs '$t' — SKIP (install it to run this leg)" >&2
            return "$LEG_SKIP"
        }
    done
    # chained with &&: a failed split-up must abort before split-smoke
    # (errexit is off in leg subshells — see the library legs' comment)
    "$REPO/scripts/dev/split-up.sh" &&
        "$REPO/scripts/dev/split-smoke.sh" # runs split-down itself + verifies teardown
}

leg_act() { # service-free CI jobs through act against the local machine
    local machine sock job
    PATH="/opt/homebrew/bin:$PATH"
    if ! command -v act >/dev/null; then
        echo "ci-local: act not installed — SKIP (bonus leg, not required; the DB legs run natively anyway)" >&2
        return "$LEG_SKIP"
    fi
    if [ -z "$(podman images -q axiom-act-runner:latest 2>/dev/null)" ]; then
        echo "ci-local: runner image axiom-act-runner:latest not found — SKIP (build it first)" >&2
        return "$LEG_SKIP"
    fi
    machine="$(running_machine)"
    if [ -z "$machine" ]; then
        echo "ci-local: no running podman machine — SKIP the act replay" >&2
        return "$LEG_SKIP"
    fi
    sock="unix://$(podman machine inspect "$machine" --format '{{.ConnectionInfo.PodmanSocket.Path}}')"
    for job in $ACT_JOBS; do
        echo "ci-local: act — job '$job'"
        DOCKER_HOST="$sock" act -j "$job" -W "$REPO/.github/workflows/ci.yml" \
            --container-daemon-socket "$sock" --pull=false \
            -P ubuntu-latest=axiom-act-runner:latest || return 1
    done
}

# --- scratch setup / teardown -----------------------------------------------
SCRATCH_PHASE=0
PGUSER=""
OWN_PREFIX=""
OWN_BASE_MANAGED=0

db_idle() { # no other session connected to the database?
    [ -z "$(psql_admin -c "SELECT 1 FROM pg_stat_activity WHERE datname='$1' AND pid<>pg_backend_pid() LIMIT 1" 2>/dev/null)" ]
}

drop_db_if_idle() { # drop_db_if_idle <name> [why]: NEVER touches in-use databases
    local exists
    [ -n "$PGUSER" ] || return 0
    # command substitution, no pipe: grep -q would SIGPIPE psql under
    # pipefail and randomly swallow the drop
    exists="$(psql_admin -c "SELECT 1 FROM pg_database WHERE datname='$1'" 2>/dev/null || true)"
    case "$exists" in *1*) ;; *) return 0 ;; esac
    db_idle "$1" || {
        echo "ci-local: '$1' is in use — left untouched"
        return 0
    }
    psql_admin -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$1' AND pid<>pg_backend_pid()" >/dev/null 2>&1 || true
    if psql_admin -c "DROP DATABASE IF EXISTS $1" >/dev/null 2>&1; then
        echo "ci-local: dropped $1${2:+ ($2)}"
    else
        echo "ci-local: could not drop $1 (in use again?) — left in place" >&2
    fi
}

drop_pattern_if_idle() { # drop_pattern_if_idle <prefix> : own ephemeral <prefix>_..._test
    local pat dbs db
    pat="$(printf '%s' "$1" | sed 's/_/\_/g')\_%\_test"
    dbs="$(psql_admin -c "SELECT datname FROM pg_database WHERE datname LIKE '$pat'" 2>/dev/null || true)"
    for db in $dbs; do
        drop_db_if_idle "$db" "own ephemeral"
    done
}

with_runlock() { # with_runlock <name> <cmd...>: mkdir lock, self-healing
    # Serializes a leg across concurrent ci-local runs on this host (the
    # go legs share the Go build cache; two legs also touch suite-internal
    # FIXED database names; mkdocs cleans a shared site dir).
    #
    # Liveness: the lock dir records the holder's root pid — $$ is stable
    # across the leg subshells, so it names the holding RUN however deep
    # the call sits. A waiter steals only once that pid is gone (a live
    # holder, however slow, is never stolen from); the 30-minute mtime
    # rule stays as the fallback for pid-less lock dirs. Release is
    # pid-guarded: a holder that was stolen from cannot delete the new
    # holder's lock. No EXIT-trap cleanup on purpose: with_runlock also
    # runs in the main shell (scratch_setup), where replacing the on_exit
    # trap would lose abort-time teardown — an interrupted run dies with
    # its pid, and the next run steals immediately.
    # ponytail: host-local mkdir lock — a Postgres advisory lock would
    # cover remote agents, add one if ci-local ever runs cross-host.
    local name="$1" lock="${TMPDIR:-/tmp}/ci-local-$1.lock" holder rc=0
    shift
    while ! mkdir "$lock" 2>/dev/null; do
        holder="$(cat "$lock/pid" 2>/dev/null || true)"
        if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then
            echo "ci-local: stealing run lock '$name' — holder $holder is gone" >&2
            rm -rf "$lock"
        elif [ -z "$holder" ] && [ -n "$(find "$lock" -maxdepth 0 -mmin +30 2>/dev/null)" ]; then
            echo "ci-local: stealing pid-less run lock '$name' (older than 30 min)" >&2
            rm -rf "$lock"
        else
            sleep 5
        fi
    done
    echo $$ >"$lock/pid"
    "$@" || rc=$?
    if [ "$(cat "$lock/pid" 2>/dev/null || true)" = "$$" ]; then
        rm -rf "$lock"
    fi
    return "$rc"
}

ensure_db() { # ensure_db <name>: create if missing (fresh and empty)
    if [ "$(psql_admin -c "SELECT 1 FROM pg_database WHERE datname='$1'")" = "1" ]; then
        return 0
    fi
    podman exec "$DB_CONTAINER" createdb -U "$PGUSER" "$1"
}

scratch_ensure_baseline_db() { # under dbshared: the shared baseline fixture
    ensure_db "$BASELINE_DB"
}

scratch_setup() { # derive DSNs, clean scratch, create the bases fresh
    local base_dsn base_name
    if [ -n "${AXIOM_TEST_DATABASE_URL:-}" ]; then
        IT_DSN="$AXIOM_TEST_DATABASE_URL"
        IT_DSN_SRC="AXIOM_TEST_DATABASE_URL (operator-supplied)"
        OWN_BASE_MANAGED=0 # operator owns the base: never dropped here
    else
        [ -r "$RAG_ENV" ] || {
            echo "ci-local: no AXIOM_TEST_DATABASE_URL and env file unreadable: $RAG_ENV" >&2
            echo "ci-local: export AXIOM_TEST_DATABASE_URL (database must end in _test) or point AXIOM_DEV_RAG_ENV at a readable env file" >&2
            return 1
        }
        # subshell: the source DSN (and its variables) must never leak
        # into the environment. Precedence inside the env file:
        # AXIOM_STORE_DATABASE_URL (component-split store plane — the
        # binary's canonical spelling), else the legacy pre-split
        # AXIOM_DATABASE_URL (same cluster).
        base_dsn="$(
            set -a
            # shellcheck disable=SC1090
            . "$RAG_ENV"
            set +a
            if [ -n "${AXIOM_STORE_DATABASE_URL:-}" ]; then
                printf '%s' "$AXIOM_STORE_DATABASE_URL"
            else
                printf '%s' "${AXIOM_DATABASE_URL:-}"
            fi
        )"
        [ -n "$base_dsn" ] || {
            echo "ci-local: $RAG_ENV defines neither AXIOM_STORE_DATABASE_URL nor AXIOM_DATABASE_URL" >&2
            return 1
        }
        IT_DSN="$(dsn_with_db "$base_dsn" "$RUN_BASE")" || {
            echo "ci-local: cannot parse database from the env file's database URL — need postgres://…/dbname form" >&2
            return 1
        }
        IT_DSN_SRC="derived from the env file's database URL"
        OWN_BASE_MANAGED=1 # this run owns its base and removes it at teardown
    fi
    base_name="$(dsn_dbname "$IT_DSN")"
    case "$base_name" in
    *_test) ;; # the suites' own guard, asserted early instead of 10 min in
    *)
        echo "ci-local: refusing DSN with database '$base_name' — the suites require a _test-suffixed scratch database" >&2
        return 1
        ;;
    esac
    BASE_DSN="$(dsn_with_db "$IT_DSN" "$BASELINE_DB")" # allowlist-guaranteed
    PGUSER="$(dsn_user "$BASE_DSN")"
    [ -n "$PGUSER" ] || {
        echo "ci-local: cannot parse user from DSN" >&2
        return 1
    }

    # shared-state work under the dbshared lock: ensure_db on the shared
    # baseline fixture races a concurrent run's baseline leg (both hold
    # the same lock). The own per-run base below is pid-unique and needs
    # no lock.
    # own per-run base, fresh: a same-pid leftover from a crashed earlier
    # run is dropped first (idle-guarded); reused IT databases drift (42P10
    # on a phantom unique index) — runs never inherit database state
    OWN_PREFIX="${base_name%_test}"
    if [ "$OWN_BASE_MANAGED" = 1 ]; then
        drop_db_if_idle "$base_name" "stale same-run base"
    fi
    ensure_db "$base_name"
    with_runlock dbshared scratch_ensure_baseline_db
    echo "ci-local: own scratch base fresh: '$base_name' (IT legs) + '$BASELINE_DB' ensured (baseline leg)"
    echo "ci-local: DB legs DSN: $(masked_dsn "$IT_DSN")  [$IT_DSN_SRC]"
}

scratch_teardown() { # remove EVERYTHING this run created — even on abort
    [ "$SCRATCH_PHASE" = 1 ] || return 0
    [ -n "$PGUSER" ] || return 0
    # the baseline suite's fixed scratch name (no _test suffix; normally
    # dropped by its own defer — this catches aborts mid-run)
    drop_db_if_idle axiom_baseline_scratch "baseline suite scratch"
    # NOT dropped here, on purpose: $BASELINE_DB (a shared, never-drifting
    # fixture — nothing writes into it, and a concurrent run's baseline
    # leg still needs it) and the fixed-name IT databases (the go-db-it
    # refresh owns their lifecycle; next run drops+recreates them anyway)
    # own ephemerals + own base (operator-supplied bases are never
    # dropped; in operator mode even the derived prefix belongs to the
    # operator — another session's ephemerals under it stay theirs)
    if [ "${OWN_BASE_MANAGED:-0}" = 1 ]; then
        drop_pattern_if_idle "$OWN_PREFIX"
        drop_db_if_idle "${OWN_PREFIX}_test" "own base"
    fi
    echo "ci-local: teardown complete — own databases removed"
}

split_down_if_needed() {
    [ "$WITH_TOPOLOGY" = 1 ] || return 0
    [ -f "$HOME/.local/state/axiom-dev/split.pid" ] || return 0
    "$REPO/scripts/dev/split-down.sh" >/dev/null 2>&1 || true
}

print_summary() {
    local i pass skip total
    pass=0
    skip=0
    echo
    echo "== ci-local summary (one line per leg) =="
    i=0
    while [ "$i" -lt "${#LEG_NAMES[@]}" ]; do
        printf '  [%s] %-26s %7ss\n' "${LEG_STATUS[$i]}" "${LEG_NAMES[$i]}" "${LEG_SECS[$i]}"
        [ "${LEG_STATUS[$i]}" = PASS ] && pass=$((pass + 1))
        [ "${LEG_STATUS[$i]}" = SKIP ] && skip=$((skip + 1))
        i=$((i + 1))
    done
    total=${#LEG_NAMES[@]}
    echo "== ci-local: $pass/$total PASS, $skip SKIP, $((total - pass - skip)) FAIL =="
}

on_exit() {
    local rc=$?
    scratch_teardown
    split_down_if_needed
    print_summary
    exit "$rc"
}
trap on_exit EXIT
trap 'trap - INT TERM HUP; on_exit' INT TERM HUP

# --- the pipeline -----------------------------------------------------------
run_leg drift-preflight leg_drift
run_leg fix-convention leg_fix_convention
run_leg go-fmt leg_go_fmt
run_leg go-vet leg_go_vet dbshared
run_leg go-unit leg_go_unit dbshared

SCRATCH_PHASE=1
echo "ci-local: ── scratch setup"
scratch_setup
run_leg go-db-it leg_go_db_it dbshared
run_leg golden-baseline leg_golden_baseline dbshared
run_leg library-engine-postgres leg_library_engine_postgres dbshared
run_leg library-engine-sqlite leg_library_engine_sqlite dbshared

run_leg runner-pytest leg_runner_pytest
run_leg fixer-pytest leg_fixer_pytest
run_leg docs-gate leg_docs docs

[ "$WITH_TOPOLOGY" = 0 ] || run_leg split-topology leg_topology
[ "$WITH_ACT" = 0 ] || run_leg act-replay leg_act
