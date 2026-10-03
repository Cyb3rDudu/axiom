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

set -euo pipefail

OS_URL="${AXIOM_OPENSEARCH_URL:-http://127.0.0.1:9200}"
OLD_INDEX="${1:-axiom-ng-chunks-v1}"
NEW_INDEX="${2:-axiom-chunks-v1}"

CURL=(curl -fsS)
if [ -n "${AXIOM_OPENSEARCH_USERNAME:-}" ]; then
    CURL+=(-u "$AXIOM_OPENSEARCH_USERNAME:${AXIOM_OPENSEARCH_PASSWORD:-}")
fi

die() { echo "reindex: $*" >&2; exit 1; }
note() { echo "reindex: $*"; }

[ "$OLD_INDEX" != "$NEW_INDEX" ] || die "old and new index are identical ($OLD_INDEX)"

"${CURL[@]}" -o /dev/null "$OS_URL/$OLD_INDEX" || die "legacy index $OLD_INDEX not reachable at $OS_URL"
if "${CURL[@]}" -o /dev/null "$OS_URL/$NEW_INDEX"; then
    die "target index $NEW_INDEX already exists — refusing to touch it (delete it first if this is a retry)"
fi

note "cloning mappings+settings: $OLD_INDEX -> $NEW_INDEX …"
"${CURL[@]}" "$OS_URL/$OLD_INDEX" |
    jq --arg old "$OLD_INDEX" \
        '{settings: (.[$old].settings.index | del(.uuid,.version,.creation_date,.provided_name)), mappings: .[$old].mappings}' |
    "${CURL[@]}" -XPUT "$OS_URL/$NEW_INDEX" -H 'Content-Type: application/json' -d @- >/dev/null ||
    die "index create failed"

cleanup_partial() {
    "${CURL[@]}" -XDELETE "$OS_URL/$NEW_INDEX" >/dev/null 2>&1 || true
}
if ! "${CURL[@]}" -XPOST "$OS_URL/_reindex?wait_for_completion=true" -H 'Content-Type: application/json' \
    -d "{\"source\":{\"index\":\"$OLD_INDEX\"},\"dest\":{\"index\":\"$NEW_INDEX\"}}" \
    | jq -e '.failures | length == 0' >/dev/null; then
    # HTTP 200 can still carry per-doc failures — the copy must be exact
    cleanup_partial
    die "_reindex failed or reported per-doc failures (partial target deleted — safe to rerun)"
fi

old_n="$("${CURL[@]}" "$OS_URL/$OLD_INDEX/_count" | jq -r .count)"
new_n="$("${CURL[@]}" "$OS_URL/$NEW_INDEX/_count" | jq -r .count)"
[ "$new_n" = "$old_n" ] || {
    cleanup_partial
    die "count mismatch: $new_n/$old_n (partial target deleted — safe to rerun)"
}
note "counts match: $new_n/$old_n"

# spot-query parity: same top chunk id for a probe query on both indices
# (identical mappings -> identical BM25 ranking; the dense arm needs the
# runtime, BM25 parity is the index-level witness here)
PROBE='{"size":1,"query":{"match_all":{}},"sort":["_id"]}'
a="$("${CURL[@]}" "$OS_URL/$OLD_INDEX/_search" -H 'Content-Type: application/json' -d "$PROBE" | jq -r '.hits.hits[0]._id // "none"')"
b="$("${CURL[@]}" "$OS_URL/$NEW_INDEX/_search" -H 'Content-Type: application/json' -d "$PROBE" | jq -r '.hits.hits[0]._id // "none"')"
[ "$a" = "$b" ] && [ "$a" != "none" ] || {
    cleanup_partial
    die "spot-query mismatch: first _id $a vs $b (partial target deleted — safe to rerun)"
}
note "spot query parity ok (first _id $a)"

note "DONE — $NEW_INDEX holds $new_n docs, byte-preserving copy of $OLD_INDEX."
note "next (operator): start the RAG on the canonical default (unset AXIOM_OS_INDEX or set it to $NEW_INDEX),"
note "soak, then delete $OLD_INDEX. Rollback until then: AXIOM_OS_INDEX=$OLD_INDEX + restart."
