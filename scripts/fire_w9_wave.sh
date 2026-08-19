#!/usr/bin/env bash
# W9 WAVE FIRING — single-command cold execution (runbook §2.1a→§3.1→§2.3).
# AUTHORIZATION: #188 wave authority via hivemind-68752, gated on:
# both W7 heals terminal + checklist green + Hivemind re-check of THIS
# script. Enforces its own conditions — aborts on any failed checkpoint.
# Abort-state guarantee: before the enqueue, every abort leaves the system
# untouched (at worst: old runner stopped + new runners up + no dispatchers
# = inert; runners are passive HTTP servers, nothing drains).
set -euo pipefail

SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"
CARRIER="dudu@192.168.1.2"     # ssh target
CARRIER_HOST="192.168.1.2"     # URL host (never user@ in URLs — review W2)
TRAIN_SHA="78558d5"            # merge-train minimum (ancestry-checked)
LOG(){ printf '\n=== %s ===\n' "$*"; }
DIE(){ echo "ABORT: $*" >&2
  echo "Abort state: pre-enqueue = untouched or inert (old runner may be stopped," >&2
  echo "new runners up, no dispatchers, queue unchanged). Resume via runbook §1.3/§3.1." >&2
  exit 1; }
DB(){ podman exec axiom-postgres psql -U axiom_user -d axiom_db -tAq -v ON_ERROR_STOP=1 -c "$1"; }

# ── 4.0/§2.1a: W7 terminal + projection sync (idempotent)
LOG "4.0 W7 terminal + projection sync"
OPEN=$(DB "SELECT count(*) FROM repair_cases WHERE status IN ('queued','in_repair','rejected')")
[ "$OPEN" = "0" ] || DIE "4.5 W7 not terminal ($OPEN open cases incl. rejected) — NO FIRE"
DB "SELECT status||'='||count(*) FROM repair_cases GROUP BY status ORDER BY 1"
echo "[MUTATES] POST /api/zotero/sync (idempotent projection; -f: HTTP 500 aborts)"
SYNC_JSON=$(curl -sf --max-time 300 -X POST http://127.0.0.1:8011/api/zotero/sync) \
  || DIE "4.0 projection sync FAILED — old attachments would be enqueued instead of healed bytes"
echo "sync: $SYNC_JSON" | head -c 300; echo

# ── 4.0b stale attachments retired/re-pointed (CURRENT heal set, preflight
#     order item 4: YLPUI8AM/RBBYMAF4/AFGC84BL ghosts → EGTTJ3AF/28RBZD3L/
#     LWY53EWV live; legacy DNC73IVL/PC9U5YEX stay valid. SUB-CASE
#     Datenbasierte: AFGC84BL carries preferred+active in the DB while
#     Zotero has LWY53EWV live — the projection's setPreferredWithStats +
#     clearSiblingPreferred flip preferred to the live attachment BY
#     CONSTRUCTION once the sync runs; these asserts make it a gate.)
LOG "4.0b stale-attachment re-point (current heal set)"
STALE=$(DB "SELECT count(*) FROM zotero_attachments a WHERE a.zotero_key IN ('YLPUI8AM','RBBYMAF4','AFGC84BL')
            AND (NOT a.deleted OR EXISTS(SELECT 1 FROM processing_snapshots s
                                         WHERE s.attachment_id=a.id AND s.active))")
[ "$STALE" = "0" ] || DIE "4.0b ghost rows still live (YLPUI8AM/RBBYMAF4/AFGC84BL undeleted or active-snapshotted) — sync did NOT retire; no fire"
NEWKEYS=$(DB "SELECT count(*) FROM zotero_attachments WHERE zotero_key IN ('EGTTJ3AF','28RBZD3L','LWY53EWV') AND NOT deleted")
[ "$NEWKEYS" = "3" ] || DIE "4.0b re-pointed keys missing (EGTTJ3AF/28RBZD3L/LWY53EWV present=$NEWKEYS of 3) — heals not projected; no fire"
PREFLW=$(DB "SELECT count(*) FROM zotero_attachments WHERE zotero_key='LWY53EWV' AND preferred AND NOT deleted")
PREFAF=$(DB "SELECT count(*) FROM zotero_attachments WHERE zotero_key='AFGC84BL' AND preferred")
[ "$PREFLW" = "1" ] && [ "$PREFAF" = "0" ] || DIE "4.0b Datenbasierte preferred flip failed (LWY53EWV preferred=$PREFLW want 1, AFGC84BL preferred=$PREFAF want 0) — audit-chain attachment must win; re-point explicitly per runbook before fire"
LEGACY=$(DB "SELECT count(*) FROM zotero_attachments WHERE zotero_key IN ('DNC73IVL','PC9U5YEX') AND NOT deleted
             AND EXISTS(SELECT 1 FROM processing_snapshots s WHERE s.attachment_id=zotero_attachments.id AND s.active)")
[ "$LEGACY" = "2" ] || DIE "4.0b legacy healed keys (DNC73IVL/PC9U5YEX) no longer valid — investigate before fire"

# ── 4.1 carrier clone: REAL ancestry check (SHAs do not order lexicographically)
LOG "4.1 carrier clone ancestry >= $TRAIN_SHA"
$SSH "$CARRIER" "git -C ~/Code/axiom merge-base --is-ancestor $TRAIN_SHA HEAD" \
  || DIE "4.1 carrier clone lacks the merge train ($TRAIN_SHA not an ancestor)"

# ── 4.4 queue: nothing PROCESSING (old-code drain); pendings = known heal set
LOG "4.4 queue state"
PROC=$(DB "SELECT count(*) FROM ingest_jobs WHERE status='processing'")
[ "$PROC" = "0" ] || DIE "4.4 $PROC jobs PROCESSING (old-code dispatcher drain!) — stop it first"
PEND=$(DB "SELECT count(*) FROM ingest_jobs WHERE status='pending'")
HEALNEW=$(DB "SELECT count(*) FROM ingest_jobs j JOIN zotero_attachments a ON a.id=j.attachment_id
              WHERE j.status='pending' AND j.content_hash = a.content_hash AND NOT j.force_rebuild")
echo "pending=$PEND (of which current-hash heal-projections: $HEALNEW)"
[ "$PEND" = "$HEALNEW" ] || DIE "4.4 $PEND pending but only $HEALNEW are heal-projections — unexpected queue content, eyeball first"

# ── 4.7 OS health (bounded)
curl -sf --max-time 10 localhost:9200/_cluster/health | grep -q '"status":"green"' \
  || DIE "4.7 OpenSearch not green"

# ── 4.9/4.10 disk thresholds
LOG "4.9/4.10 disk"
DF_MAC=$(df -k / | awk 'NR==2{print int($4/1024/1024)}')
[ "$DF_MAC" -ge 50 ] || DIE "4.9 Mac free ${DF_MAC}G < 50G (new generation transiently doubles chunk tables)"
DF_CARRIER=$($SSH "$CARRIER" "df -k / | awk 'NR==2{print int(\$4/1024/1024)}'") \
  || DIE "4.10 carrier df failed"
[ "$DF_CARRIER" -ge 100 ] || DIE "4.10 carrier free ${DF_CARRIER}G < 100G"
echo "Mac ${DF_MAC}G / carrier ${DF_CARRIER}G free"

# ── 4.12 dry SELECT with the BOOK LIST (eyeball before enqueue — runbook §2.3)
LOG "4.12 expected book list (eyeball: every healed book under its NEW key)"
DB "SELECT left(d.title,60), a.zotero_key, a.content_hash
    FROM zotero_attachments a JOIN zotero_documents d ON d.id=a.document_id
    WHERE NOT a.deleted
      AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.attachment_id=a.id AND s.active)
    ORDER BY d.title" | tee /tmp/w9_wave_books.txt
COUNT=$(wc -l < /tmp/w9_wave_books.txt | tr -d ' ')
[ "$COUNT" -ge 100 ] || DIE "4.12 suspiciously few active books ($COUNT)"
echo "active books: $COUNT (list in /tmp/w9_wave_books.txt — VERIFY healed books present)"

# ── 4.2/4.3 symbol proof on the UP gpu0 BEFORE any mutation (review W4:
#    full assert set incl. page_trust + runner, and before cutover)
LOG "4.2 pre-cutover symbol proof (w9-gpu0)"
$SSH "$CARRIER" "podman exec runner-carrier-w9-gpu0 python -c \"
import inspect, axiom_ng_runner.compute_core.chunker as c
import axiom_ng_runner.compute_core.page_trust as pt, axiom_ng_runner.runner as r
s=inspect.getsource
assert 'current_chunk_headings' in s(c) and 'page_chapter_map' in s(c)
assert 'page_chapter_map' in s(pt) and '_stamp_chapter' in s(r)
print('gpu0 PROOF OK')\"" || DIE "4.2 gpu0 symbol proof failed"

# ── §3.1 cutover
LOG "cutover: stop old GPU1 runner [MUTATES]"
$SSH "$CARRIER" 'podman stop runner-carrier-gpu1' || DIE "cutover: cannot stop old runner"
LOG "cutover: start w9-gpu1 + w9-a3000 [MUTATES]"
$SSH "$CARRIER" 'set -e
for spec in "1 19543 w9-gpu1" "2 19544 w9-a3000"; do
  set -- $spec
  podman run -d --replace --name runner-carrier-$3 \
    --network=host --shm-size=8g --device nvidia.com/gpu=all \
    -e CUDA_VISIBLE_DEVICES=$1 -e AXIOM_PROCESSOR_COMPUTE=real \
    -e AXIOM_PROCESSOR_BIND_ADDR=0.0.0.0 -e AXIOM_PROCESSOR_PORT=$2 \
    -e AXIOM_PROCESSOR_WORK_ROOT=/work \
    -e AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS=/data \
    -e DEVICE_GLINER=cuda runner-poc:78558d5 >/dev/null
  mkdir -p /tmp/axiom_runs
  podman inspect runner-carrier-$3 --format "{{.State.Pid}}" > /tmp/axiom_runs/$3.pid
done
for p in 19542 19543 19544; do
  ok=0; for i in 1 2 3 4 5 6 7 8 9 10; do
    curl -s --max-time 5 http://127.0.0.1:$p/v1/health | grep -q ok && ok=1 && break || sleep 3
  done
  [ $ok = 1 ] || { echo "runner :$p not healthy"; exit 1; }
done
echo "3 runners healthy"'
LOG "cutover: symbol proof gpu1 + a3000"
for n in w9-gpu1 w9-a3000; do
  $SSH "$CARRIER" "podman exec runner-carrier-$n python -c \"
import inspect, axiom_ng_runner.compute_core.chunker as c
assert 'current_chunk_headings' in inspect.getsource(c) and 'page_chapter_map' in inspect.getsource(c)
print('$n PROOF OK')\"" || DIE "4.2 $n symbol proof failed (post-cutover abort: see state note)"
done

# ── dispatchers: MANUAL step with the FULL env set (review W1) + operator gate
LOG "DISPATCHERS — manual step, full env per runbook §2.2 (incl. SOURCE_BASE_URL,"
echo "SOURCE_SECRET, DATABASE_URL, ARTIFACT_ROOT, PROFILE=full-rag-v1 — the Flutgate"
echo "empty-profile trap). Start 3 (runner w9-gpu0/w9-gpu1/w9-a3000 -> 19542/43/44)."
read -r -p "Confirm: 3 dispatcher processes running with the reviewed env (y/N) " ANS
[ "$ANS" = "y" ] || DIE "dispatchers not confirmed — nothing enqueued (safe abort)"

# ── §2.3 enqueue [MUTATES]: atomic insert, count asserted in a SECOND call
LOG "fire: force_rebuild enqueue [MUTATES]"
DB "INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
    SELECT a.source_id, a.document_id, a.id, a.content_hash, 'pending', true
    FROM zotero_attachments a
    WHERE NOT a.deleted
      AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.attachment_id=a.id AND s.active)" \
  || DIE "enqueue INSERT failed — transaction rolled back atomically, nothing fired"
INSERTED=$(DB "SELECT count(*) FROM ingest_jobs WHERE force_rebuild AND status='pending'")
echo "force jobs pending: $INSERTED (expected >= $COUNT from the eyeballed list)"
case "$INSERTED" in (*[!0-9]*|"") DIE "enqueue count capture corrupted: '$INSERTED'";; esac
[ "$INSERTED" -ge "$COUNT" ] || DIE "enqueue count mismatch ($INSERTED < $COUNT) — jobs ARE live; check state before any re-run (a re-run would double-enqueue: force rows bypass the idempotency index)"

# ── initial telemetry
LOG "INITIAL TELEMETRY (30s in)"
sleep 30
DB "SELECT status, coalesce(runner_name,'—') AS card, count(*)
    FROM ingest_jobs WHERE force_rebuild OR status='pending' GROUP BY 1,2 ORDER BY 1,2"
DB "SELECT count(*) AS first_claims FROM ingest_jobs
    WHERE status IN ('claimed','processing') AND runner_name IS NOT NULL"
$SSH "$CARRIER" 'nvidia-smi --query-gpu=index,utilization.gpu,memory.used --format=csv,noheader'
echo "WAVE FIRED — monitor per runbook §2.6; watch query:"
echo "  SELECT status, coalesce(runner_name,'—'), count(*) FROM ingest_jobs"
echo "  WHERE force_rebuild OR status='pending' GROUP BY 1,2;"   # single quotes = SQL literal (review W3)
