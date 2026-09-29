#!/bin/bash
# split-smoke.sh — the F11 split-topology acceptance smoke (#305; DoD
# re-decision 2026-09-28, option b): CONTRACT-CLASS checks over the
# public edge of the running three-process split topology. No frozen
# fixtures are compared here — the freeze witness stays all-in-one +
# release-only (`make golden-baseline` is untouched by this).
#
# What this proves:
#   1. /api/health answers healthy with the four dependency checks
#   2. /api/v1/search answers with the typed hit shape (chunk_id,
#      doc_id, locator.kind — field names, not frozen values)
#   3. /api/v1/passage/{id} answers with attachment_id + neighbors
#      (the ADR-0001 translation across the HTTP binding)
#   4. kill probe: the library process killed → the public import
#      status route answers the TYPED unavailable envelope, and the
#      body leaks no component host/port/dial detail
#   5. after split-down.sh: no split port is left listening
#
# Usage: scripts/dev/split-smoke.sh   (assumes split-up.sh already ran)
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
STATE="$HOME/.local/state/axiom-dev"
API_PORT=8111
API="http://127.0.0.1:$API_PORT"
LIB_PORT=8113
STORE_PORT=8114
LIB_EDGE=8211
STORE_EDGE=8212

die() { echo "split-smoke: $*" >&2; exit 1; }
ok() { echo "split-smoke: ok — $*"; }

command -v jq >/dev/null || die "jq required"
command -v curl >/dev/null || die "curl required"
command -v lsof >/dev/null || die "lsof required (teardown completeness check)"
[ -f "$STATE/split.pid" ] || die "no $STATE/split.pid — start the split topology first (scripts/dev/split-up.sh)"

# --- 1. health ---------------------------------------------------------------

health="$(curl -fsS -m 60 "$API/api/health")"
echo "$health" | jq -e '.ok == true' >/dev/null || die "health not ok: $health"
for check in postgres zotero query-runner ingest-runner; do
    echo "$health" | jq -e --arg c "$check" '.checks[$c] == "ok"' >/dev/null ||
        die "health check $check: $(echo "$health" | jq -r --arg c "$check" '.checks[$c]')"
done
ok "health: ok=true, all four dependency checks green"

# --- 2. search (typed hit shape, no fixtures) --------------------------------

query="${AXIOM_SPLIT_SMOKE_QUERY:-management}"

# Settling gate (the golden suite's quiescence pattern): the FIRST search
# after a split boot can outrun the api's default 30 s component budget
# while the store process's retrieval pipeline warms (runner model swap,
# OpenSearch client init). Retry until it answers — the timed assertions
# below then run against a warm edge. (3 min of retry sleeps, plus
# per-attempt latency.)
sres=""
qbody="$(jq -nc --arg q "$query" '{query:$q}')"
for i in $(seq 1 36); do
    if sres="$(curl -fsS -m 60 -X POST "$API/api/v1/search" -H 'Content-Type: application/json' \
        -d "$qbody" 2>/dev/null)"; then
        break
    fi
    echo "split-smoke: waiting for search readiness (attempt $i)…"
    sleep 5
done
[ -n "$sres" ] || die "search never became ready within 3 min (see $STATE/logs/split-{api,store}.log)"
echo "$sres" | jq -e '(.hits | type) == "array"' >/dev/null || die "search: hits not an array"
chunk_id="$(echo "$sres" | jq -r '.hits[0].chunk_id // empty')"
[ -n "$chunk_id" ] || die "search: no hit for query '$query' (pick another AXIOM_SPLIT_SMOKE_QUERY)"
echo "$sres" | jq -e '.hits[0].source.doc_id != null and .hits[0].locator.kind != null' >/dev/null ||
    die "search: hit misses the typed shape (source.doc_id / locator.kind)"
if echo "$sres" | jq -e '.hits[0].source.record_id != null' >/dev/null; then
    die "search: component-internal field record_id leaked into the public answer"
fi
ok "search: typed hits (chunk_id=$chunk_id, doc_id + locator.kind present, no record_id leak)"

# --- 3. passage (ADR-0001 translation over the binding) ----------------------

pres="$(curl -fsS -m 60 "$API/api/v1/passage/$chunk_id")"
echo "$pres" | jq -e '.attachment_id != null and (.neighbors | type) == "array"' >/dev/null ||
    die "passage: misses attachment_id / neighbors"
if echo "$pres" | jq -e '.rendition_id != null' >/dev/null; then
    die "passage: component-internal field rendition_id leaked into the public answer"
fi
ok "passage: attachment_id + neighbors present, no rendition_id leak"

# --- 4. kill probe (typed unavailable, no topology leak) ----------------------

libpid="$(awk '$1=="library"{print $2}' "$STATE/split.pid")"
[ -n "$libpid" ] || die "no library pid in split.pid"
# The probe's causal witness: the library edge must answer BEFORE the kill
# (a library that died after boot would pass the envelope check without
# the kill ever being exercised).
# Any HTTP answer proves the edge alive (the probe id is unknown → 404 is
# the expected alive answer; 000/empty means nothing is listening).
lcode="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$LIB_EDGE/internal/v1/library/sources/probe" || true)"
[ "$lcode" != "000" ] && [ -n "$lcode" ] ||
    die "library edge :$LIB_EDGE does not answer before the kill — library already dead?"
kill -TERM -- "-$libpid" 2>/dev/null || kill -TERM "$libpid" 2>/dev/null || true
ok "kill probe: TERM sent to the library process (pgid $libpid)"

# A slow-draining library keeps answering during shutdown; poll until the
# typed failure settles (the api's GET-retry rides out one blip by itself).
kbody=""
kcode=""
for i in $(seq 1 20); do # up to 10 s
    ktmp="$(mktemp)"
    kcode="$(curl -s -m 60 -o "$ktmp" -w '%{http_code}' \
        "$API/api/v1/library/imports/split-smoke-probe" || true)"
    kbody="$(cat "$ktmp" 2>/dev/null || true)"
    rm -f "$ktmp"
    [ "$kcode" = "503" ] && break
    sleep 0.5
done
[ "$kcode" = "503" ] || die "kill probe: status $kcode (want 503), body: $kbody"
echo "$kbody" | jq -e '.error.component == "library" and .error.class == "unavailable"' >/dev/null ||
    die "kill probe: not the typed library/unavailable envelope: $kbody"
for leak in "127.0.0.1" "localhost" "dial" "http://"; do
    case "$kbody" in *"$leak"*) die "kill probe: body leaks topology detail '$leak': $kbody" ;; esac
done
ok "kill probe: 503 typed library/unavailable envelope, leak-free"

# --- 5. teardown completeness (split-down leaves nothing) ---------------------

"$REPO/scripts/dev/split-down.sh" >/dev/null
# Graceful shutdown is asynchronous (split-down TERMs and returns; the
# composition's stop budget can be seconds) — poll before declaring the
# teardown incomplete, never false-green on a slow drain.
busy=1
for i in $(seq 1 20); do # up to 10 s
    busy=0
    for p in "$API_PORT" "$LIB_PORT" "$STORE_PORT" "$LIB_EDGE" "$STORE_EDGE"; do
        lsof -i ":$p" -sTCP:LISTEN >/dev/null 2>&1 && busy=1
    done
    [ "$busy" = 0 ] && break
    sleep 0.5
done
[ "$busy" = 0 ] || die "split ports still listening after split-down (10 s) — teardown incomplete"
[ -f "$STATE/split.pid" ] && die "$STATE/split.pid still present after split-down"
ok "teardown: all split ports free, no pid file left"

echo
echo "split-smoke: ALL GREEN — public contract classes, ADR translation,"
echo "typed kill envelope, leak-freedom, and clean teardown verified in split."
