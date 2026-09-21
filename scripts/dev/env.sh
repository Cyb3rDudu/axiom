# shellcheck shell=bash
# env.sh — dev-environment identity for MANUAL tool runs. SOURCE it, never execute:
#
#   . scripts/dev/env.sh
#   python3 axiom_ng_runner/scripts/locator_rescan.py …
#
# Why this exists: the Python-side OpenSearch writers (locator_rescan.py,
# test_query_endpoints_it.py) honor AXIOM_OS_INDEX but nothing forces a fresh
# shell to set it. Run against the dev DB with only AXIOM_DATABASE_URL
# exported and those tools _bulk-write into the PRODUCTION index — the last
# open prod-write path in the dev workflow. Sourcing this file closes it.
#
# Keep DEV_INDEX/DEV_DB in sync with dev-up.sh (same isolation boundary;
# dev-up asserts them at start, this file exports them for operators).
# CI (dev scripts lint step) enforces the sync: it fails when the two
# definitions drift apart.
#
# NO `set -euo pipefail` here — this file is SOURCED, and errexit/nounset
# would stick to (or kill) the operator's interactive shell after a mid-file
# failure. Every failure site below is an explicit guard instead. The
# set -a/+a pair stays: the sourced rag.env itself contains `set -a`, so the
# caller's option state is clobbered by it regardless — nothing to preserve.

DEV_INDEX="axiom-dev-chunks-v1"
DEV_DB="axiom_dev"
RAG_ENV="${AXIOM_DEV_RAG_ENV:-/run/agenix/axiom-rag.env}"

[ -r "$RAG_ENV" ] || {
    echo "env.sh: cannot read $RAG_ENV" >&2
    return 1 2>/dev/null || exit 1
}

set -a
# shellcheck disable=SC1090
. "$RAG_ENV"
set +a

AXIOM_DATABASE_URL="$(printf '%s' "$AXIOM_DATABASE_URL" | sed -E "s#/axiom_db([?]|$)#/$DEV_DB\1#")"
export AXIOM_DATABASE_URL
export AXIOM_OS_INDEX="$DEV_INDEX"

# same hard assert as dev-up: refuse to point tools at prod
case "$AXIOM_DATABASE_URL" in
*"/$DEV_DB" | *"/$DEV_DB"[?]*) ;;
*)
    echo "env.sh: DATABASE_URL derivation failed (got: ${AXIOM_DATABASE_URL##*@})" >&2
    return 1 2>/dev/null || exit 1
    ;;
esac

echo "dev env active: db=$DEV_DB index=$DEV_INDEX os=${AXIOM_OPENSEARCH_URL:-http://127.0.0.1:9200}"
