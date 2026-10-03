#!/bin/bash
# reindex_index_rename.sh — #352 OS index rename window (Goal 6).
#
# Byte-preserving index-to-index move of the chunks index:
#   legacy `axiom-ng-chunks-v1` -> canonical `axiom-chunks-v1`
# via OpenSearch `_reindex` ONLY — no document is re-processed, re-chunked
# or re-embedded; chunks and vectors are copied as stored bytes (the path
# the v0.2.0 release validation already proved at 57,724/57,724).
#
# What this script does (run with the RAG stopped — write delta exactly 0):
#   1. clone mappings+settings from the legacy index (cluster metadata
#      stripped), creating the canonical index
#   2. `_reindex?wait_for_completion=true` legacy -> canonical
#   3. verify: equal doc counts + spot-query parity (same top hit for one
#      probe query on both indices)
#
# What it deliberately does NOT do:
#   - no delete of the legacy index — that happens after the soak period,
#     by the operator, once the canonical index holds in production
#   - no switch — the RAG starts on the canonical default by itself once
#     this branch's code default is live (AXIOM_OS_INDEX overrides if set)
#
# Rollback: point AXIOM_OS_INDEX at the legacy index and restart — both
# indices stay side by side until the post-soak delete.
#
# Env: AXIOM_OPENSEARCH_URL (default http://127.0.0.1:9200),
#      AXIOM_OPENSEARCH_USERNAME / AXIOM_OPENSEARCH_PASSWORD (optional).
#      Source a credential env file first if the cluster requires auth.
#      Passwords containing whitespace are not representable in the netrc
#      form (they fail loudly at the reachability check); the URL must
#      carry a scheme (http:// or https://) and a hostname or IPv4 host —
#      IPv6 literals are not matched by the netrc host form.
#
# Preconditions beyond the stopped RAG: no backfill or rescan tool may run
# either (caption/figcap/sparse backfills and locator_rescan bulk-write
# against search.IndexName and would auto-create the canonical index,
# refusing the rerun). If rows drained during a premature boot reached the
# terminal state, the documented requeue (repo/outbox.go) replays them
# after the window.

set -euo pipefail

OS_URL="${AXIOM_OPENSEARCH_URL:-http://127.0.0.1:9200}"
OLD_INDEX="${1:-axiom-ng-chunks-v1}"
NEW_INDEX="${2:-axiom-chunks-v1}"

CURL=(curl -fsS)
NETRC=""
cleanup() {
    [ -n "$NETRC" ] && rm -f "$NETRC"
    return 0
}
trap cleanup EXIT
if [ -n "${AXIOM_OPENSEARCH_USERNAME:-}" ]; then
    # credentials via a 0600 netrc file, never in argv (ps-visible)
    NETRC="$(mktemp)"
    chmod 600 "$NETRC"
    OS_HOST="$(printf '%s' "$OS_URL" | sed -E 's#^[a-zA-Z]+://([^/@]+@)?([^/:]+).*#\2#')"
    printf 'machine %s login %s password %s\n' "$OS_HOST" \
        "$AXIOM_OPENSEARCH_USERNAME" "${AXIOM_OPENSEARCH_PASSWORD:-}" >"$NETRC"
    CURL+=(--netrc-file "$NETRC")
fi

die() { echo "reindex: $*" >&2; exit 1; }
note() { echo "reindex: $*"; }

[ "$OLD_INDEX" != "$NEW_INDEX" ] || die "old and new index are identical ($OLD_INDEX)"

# every call bounded: short ops get -m 30, the (large) _reindex gets -m 600
"${CURL[@]}" -m 30 -o /dev/null "$OS_URL/$OLD_INDEX" || die "legacy index $OLD_INDEX not reachable at $OS_URL"
if "${CURL[@]}" -m 30 -o /dev/null "$OS_URL/$NEW_INDEX"; then
    die "target index $NEW_INDEX already exists — refusing to touch it (delete it first if this is a retry)"
fi

note "cloning mappings+settings: $OLD_INDEX -> $NEW_INDEX …"
"${CURL[@]}" -m 30 "$OS_URL/$OLD_INDEX" |
    jq --arg old "$OLD_INDEX" \
        '{settings: (.[$old].settings.index | del(.uuid,.version,.creation_date,.provided_name)), mappings: .[$old].mappings}' |
    "${CURL[@]}" -m 30 -XPUT "$OS_URL/$NEW_INDEX" -H 'Content-Type: application/json' -d @- >/dev/null ||
    die "index create failed"

cleanup_partial() {
    "${CURL[@]}" -m 30 -XDELETE "$OS_URL/$NEW_INDEX" >/dev/null 2>&1 || true
}
if ! "${CURL[@]}" -m 600 -XPOST "$OS_URL/_reindex?wait_for_completion=true" -H 'Content-Type: application/json' \
    -d "{\"source\":{\"index\":\"$OLD_INDEX\"},\"dest\":{\"index\":\"$NEW_INDEX\"}}" \
    | jq -e '(.failures // [] | length == 0) and (.timed_out // false | not)' >/dev/null; then
    # HTTP 200 can still carry per-doc failures or a timeout flag —
    # the copy must be exact
    cleanup_partial
    die "_reindex failed or reported per-doc failures (target cleanup attempted — safe to rerun; an existing target is refused loudly)"
fi

# make the copied docs visible to count/_search before parity checks
"${CURL[@]}" -m 30 -XPOST "$OS_URL/$NEW_INDEX/_refresh" >/dev/null

old_n="$("${CURL[@]}" -m 30 "$OS_URL/$OLD_INDEX/_count" | jq -r .count)"
new_n="$("${CURL[@]}" -m 30 "$OS_URL/$NEW_INDEX/_count" | jq -r .count)"
[ "$new_n" = "$old_n" ] || {
    cleanup_partial
    die "count mismatch: $new_n/$old_n (target cleanup attempted — safe to rerun; an existing target is refused loudly)"
}
note "counts match: $new_n/$old_n"

# spot-doc parity (no _id sort — that needs non-default fielddata): take
# any one doc id from the OLD index and require the SAME id in the NEW
# index with a semantically identical _source — the byte-preserving copy
# witness at document level
probe_id="$("${CURL[@]}" -m 30 "$OS_URL/$OLD_INDEX/_search" -H 'Content-Type: application/json' \
    -d '{"size":1,"query":{"match_all":{}}}' | jq -r '.hits.hits[0]._id // empty')"
[ -n "$probe_id" ] || {
    cleanup_partial
    die "could not read a probe doc id from $OLD_INDEX — is the legacy index empty? (target cleanup attempted — safe to rerun; an existing target is refused loudly)"
}
old_src="$("${CURL[@]}" -m 30 "$OS_URL/$OLD_INDEX/_doc/$probe_id" | jq -S '._source')"
new_src="$("${CURL[@]}" -m 30 "$OS_URL/$NEW_INDEX/_doc/$probe_id" | jq -S '._source // empty')"
[ -n "$old_src" ] && [ "$old_src" = "$new_src" ] || {
    cleanup_partial
    die "spot-doc mismatch for _id $probe_id — copy not byte-preserving (target cleanup attempted — safe to rerun; an existing target is refused loudly)"
}
note "spot doc parity ok (_id $probe_id, _source equal)"

note "DONE — $NEW_INDEX holds $new_n docs, byte-preserving copy of $OLD_INDEX."
note "next (operator): start the RAG on the canonical default (unset AXIOM_OS_INDEX or set it to $NEW_INDEX),"
note "soak, then delete $OLD_INDEX. Rollback until then: AXIOM_OS_INDEX=$OLD_INDEX + restart."
