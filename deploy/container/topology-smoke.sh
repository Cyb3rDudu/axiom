#!/bin/bash
# topology-smoke.sh — the F14 container-split acceptance (#308): the same
# contract-class checks as the OS-process split smoke, driven against the
# container topology from deploy/container/compose.topology.yml:
#
#   1. public-edge health: ok + the four dependency checks
#   2. intake through the public edge → remote-class worker (own
#      namespace) → searchable with the typed hit shape (no field leak)
#   3. passage: attachment_id + neighbors, no rendition_id leak
#   4. kill probe: the library CONTAINER dies → typed library/unavailable
#      envelope, leak-free
#   5. teardown: compose down leaves nothing behind
#
# Usage: deploy/container/topology-smoke.sh
#   COMPOSE via AXIOM_TOPOLOGY_COMPOSE (default "docker compose"); podman
#   hosts export e.g. "podman-compose".
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE_DIR="$REPO/deploy/container"
COMPOSE_FILE="$COMPOSE_DIR/compose.topology.yml"
COMPOSE="${AXIOM_TOPOLOGY_COMPOSE:-docker compose}"
API="http://127.0.0.1:18111"
FIXTURE="$REPO/axiom/internal/backfill/testdata/book.epub"
SEED_PATH_IN_CONTAINER="/srv/seed/book.epub"

die() { echo "topology-smoke: $*" >&2; exit 1; }
ok() { echo "topology-smoke: ok — $*"; }
cc() { $COMPOSE -f "$COMPOSE_FILE" "$@"; }

command -v jq >/dev/null || die "jq required"
command -v curl >/dev/null || die "curl required"
[ -r "$FIXTURE" ] || die "fixture missing: $FIXTURE"

# --- 1. health (component-edge checks over the public edge) --------------
# DM07 #316: the api process holds no DB credentials — its dependency
# visibility proxies the library/store component health (each component's
# own /api/health folds its postgres/zotero/runner checks).

health="$(curl -fsS -m 60 "$API/api/health")"
echo "$health" | jq -e '.ok == true' >/dev/null || die "health not ok: $health"
for check in library store; do
    echo "$health" | jq -e --arg c "$check" '.checks[$c] == "ok"' >/dev/null ||
        die "health check $check: $(echo "$health" | jq -r --arg c "$check" '.checks[$c]')"
done
ok "health: ok=true, both component-edge checks green"

# --- 2. seed + intake through the public edge -------------------------------

hash="$( (shasum -a 256 "$FIXTURE" 2>/dev/null || sha256sum "$FIXTURE") | awk '{print $1}')"
[ -n "$hash" ] || die "fixture hash"

seed() {
    cc exec -T pg psql -U axiom -d axiom -v ON_ERROR_STOP=1 >/dev/null <<SQL
BEGIN;
DELETE FROM zotero_attachments WHERE zotero_key='ATTF14C1';
DELETE FROM zotero_documents WHERE zotero_key='DOCF14C1';
DELETE FROM zotero_items WHERE zotero_key='DOCF14C1';
DELETE FROM zotero_sources WHERE base_url='https://topology.local';
INSERT INTO zotero_sources (base_url, library_id, server_id)
VALUES ('https://topology.local','users/0','topology-standin');
INSERT INTO zotero_documents (source_id, zotero_key, zotero_version, item_type, title)
SELECT id, 'DOCF14C1', 1, 'book', 'Buchkapitel (F14 Container)'
FROM zotero_sources WHERE base_url='https://topology.local';
INSERT INTO zotero_items (source_id, zotero_key, zotero_version, item_type, parent_key, raw_envelope, raw_data)
SELECT id, 'DOCF14C1', 1, 'journalArticle', NULL, '{}', '{}'
FROM zotero_sources WHERE base_url='https://topology.local';
UPDATE zotero_documents d SET canonical_item_id = i.id
FROM zotero_items i, zotero_sources s
WHERE d.source_id = s.id AND i.source_id = s.id
  AND d.zotero_key = 'DOCF14C1' AND i.zotero_key = 'DOCF14C1'
  AND s.base_url='https://topology.local';
INSERT INTO zotero_attachments (source_id, document_id, zotero_key, zotero_version,
   parent_zotero_key, link_mode, content_type, filename, local_path, content_hash, preferred, deleted)
SELECT s.id, d.id, 'ATTF14C1', 1, 'DOCF14C1', 'imported_file', 'application/epub+zip',
       'f14.epub', '$SEED_PATH_IN_CONTAINER', '$hash', true, false
FROM zotero_sources s, zotero_documents d
WHERE s.base_url='https://topology.local' AND d.zotero_key='DOCF14C1' AND d.source_id=s.id;
-- #358: the Store-side projection row the claim resolves (identities
-- instead of joins — the mirror read died with the legacy lane).
INSERT INTO store_documents (document_id, attachment_id, source_id, server_id,
    record_key, rendition_key, content_hash, title, citation_class, content_type, filename, local_path, preferred)
SELECT a.document_id, a.id, a.source_id, 'topology-standin',
       'DOCF14C1', 'ATTF14C1', '$hash', 'Buchkapitel (F14 Container)', 'citable',
       'application/epub+zip', 'f14.epub', '$SEED_PATH_IN_CONTAINER', true
FROM zotero_attachments a JOIN zotero_sources s ON s.id = a.source_id
WHERE s.base_url='https://topology.local' AND a.zotero_key='ATTF14C1'
ON CONFLICT (source_id, rendition_key) DO UPDATE SET
    document_id=EXCLUDED.document_id, content_hash=EXCLUDED.content_hash,
    local_path=EXCLUDED.local_path, preferred=true, deleted=false;
COMMIT;
SQL
}
seed || die "mirror seed failed (is the store container healthy?)"

doc_uuid="$(cc exec -T pg psql -U axiom -d axiom -tAc \
    "SELECT d.id FROM zotero_documents d JOIN zotero_sources s ON s.id=d.source_id WHERE s.base_url='https://topology.local'" | tr -d '[:space:]')"
[ -n "$doc_uuid" ] || die "seeded document uuid not resolvable"

rev="$(jq -nc --arg h "$hash" '{source_id:"", revision_id:"1", rendition_id:"ATTF14C1",
      content_hash:$h, media_type:"application/epub+zip",
      content_ticket:"probe", bibliography:{record_id:"DOCF14C1", title:"Buchkapitel (F14 Container)", citation_class:"citable"}}')"
srcid="$(cc exec -T pg psql -U axiom -d axiom -tAc \
    "SELECT id FROM zotero_sources WHERE base_url='https://topology.local'" | tr -d '[:space:]')"
rev="$(echo "$rev" | jq -c --arg s "$srcid" '.source_id=$s')"
code="$(curl -fsS -m 60 -o /dev/null -w '%{http_code}' -X POST "$API/api/v1/store/ingest" \
    -H 'Content-Type: application/json' -d "$(jq -nc --arg r "$rev" '{idempotency_key:"f14-container-1", revision:($r|fromjson)}')" 2>/dev/null || true)"
[ "$code" = "202" ] || die "intake status ${code:-none} (want 202), body: $(curl -s -m 60 -X POST "$API/api/v1/store/ingest" -H 'Content-Type: application/json' -d "$(jq -nc --arg r "$rev" '{idempotency_key:"f14-container-diag", revision:($r|fromjson)}')")"
ok "intake accepted through the public edge (202)"

# --- 3. remote-class ride to searchability + typed shapes -------------------

chunk_id=""
sres=""
for _ in $(seq 1 120); do
    sres="$(curl -fsS -m 60 -X POST "$API/api/v1/search" -H 'Content-Type: application/json' \
        -d "{\"query\":\"chapter inhalt\",\"top_n\":5,\"filters\":{\"document_ids\":[\"$doc_uuid\"]}}" 2>/dev/null || true)"
    if [ -n "$sres" ] && echo "$sres" | jq -e '(.hits | length) > 0' >/dev/null 2>&1; then
        break
    fi
    sres=""
    sleep 5
done
if [ -z "$sres" ]; then
    echo "topology-smoke: search never became ready — store/worker tails:" >&2
    cc logs --tail 30 store worker >&2 || true
    die "no searchable hit through the container split"
fi
echo "$sres" | jq -e '(.hits | type) == "array"' >/dev/null || die "hits not an array"
chunk_id="$(echo "$sres" | jq -r '.hits[0].chunk_id // empty')"
[ -n "$chunk_id" ] || die "no hit — the container job did not become searchable (see: $COMPOSE logs store worker)"
echo "$sres" | jq -e '.hits[0].source.doc_id != null and .hits[0].locator.kind != null' >/dev/null ||
    die "hit misses the typed shape (source.doc_id / locator.kind)"
if echo "$sres" | jq -e '.hits[0].source.record_id != null' >/dev/null; then
    die "search: component-internal field record_id leaked into the public answer"
fi
ok "search: typed hits over the container split (chunk_id=$chunk_id, no record_id leak)"

pres="$(curl -fsS -m 60 "$API/api/v1/passage/$chunk_id")"
echo "$pres" | jq -e '.attachment_id != null and (.neighbors | type) == "array"' >/dev/null ||
    die "passage: misses attachment_id / neighbors"
if echo "$pres" | jq -e '.rendition_id != null' >/dev/null; then
    die "passage: component-internal field rendition_id leaked"
fi
ok "passage: attachment_id + neighbors present, no rendition_id leak"

# --- 4. kill probe (container library TERM → typed envelope) -----------------

cc exec -T library sh -c 'kill -TERM 1' ||
    echo "topology-smoke: note — library container already stopped (partial rerun); riding the envelope"
kbody=""
for _ in $(seq 1 40); do
    ktmp="$(mktemp)"
    kcode="$(curl -s -m 60 -o "$ktmp" -w '%{http_code}' "$API/api/v1/library/imports/topology-probe" || true)"
    kbody="$(cat "$ktmp" 2>/dev/null || true)"
    rm -f "$ktmp"
    [ "$kcode" = "503" ] && break
    sleep 0.5
done
[ "$kcode" = "503" ] || die "kill probe: status ${kcode:-none} (want 503), body: $kbody"
echo "$kbody" | jq -e '.error.component == "library" and .error.class == "unavailable"' >/dev/null ||
    die "kill probe: not the typed library/unavailable envelope: $kbody"
# the component NAME in the typed envelope is contract, not topology —
# only host/port/dial detail is a leak (same list as the dev split smoke)
for leak in "127.0.0.1" "localhost" "dial" "http://" "store:" "worker:" "pg:" "opensearch:"; do
    case "$kbody" in *"$leak"*) die "kill probe: body leaks topology detail '$leak': $kbody" ;; esac
done
ok "kill probe: 503 typed library/unavailable envelope, leak-free"

# --- 5. teardown --------------------------------------------------------------

cc down -v >/dev/null
for _ in $(seq 1 20); do
    if ! curl -s -m 2 -o /dev/null "$API/api/health" 2>/dev/null; then
        ok "teardown: compose down -v, public edge gone"
        echo
        echo "topology-smoke: ALL GREEN — container split proven (health, remote-class"
        echo "ride to searchability, typed shapes, kill envelope, clean teardown)."
        exit 0
    fi
    sleep 1
done
die "the public edge still answers after compose down"
