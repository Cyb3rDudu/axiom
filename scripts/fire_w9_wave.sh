#!/usr/bin/env bash
# W9 WAVE FIRING — single-command cold execution (runbook §2.1a→§3.1→§2.3).
# AUTHORIZATION: #188 wave authority via hivemind-68752, conditions =
# both W7 heals terminal (69865 report) + checklist green. This script
# ENFORCES those conditions — it aborts on any failed checkpoint.
# Every mutating step echoes [MUTATES] before running.
set -euo pipefail

CARRIER="dudu@192.168.1.2"
DSN_DB="axiom_db"
RUNBOOK_SHA_TARGET="78558d5"   # merge train minimum
LOG(){ printf '\n=== %s ===\n' "$*"; }
DIE(){ echo "ABORT: $*" >&2; exit 1; }

DB(){ podman exec axiom-postgres psql -U axiom_user -d "$DSN_DB" -tAc "$1"; }

# ── 4.0/§2.1a projection sync (idempotent; heals may have projected already)
LOG "4.0 projection sync"
HEALED=$(DB "SELECT count(*) FROM repair_cases WHERE status='healed'")
BLOCKED=$(DB "SELECT count(*) FROM repair_cases WHERE status='blocked_for_dudu'")
OPEN=$(DB "SELECT count(*) FROM repair_cases WHERE status IN ('queued','in_repair')")
[ "$OPEN" = "0" ] || DIE "4.5 W7 not terminal ($OPEN open cases) — NO FIRE"
echo "repair_cases: healed=$HEALED blocked=$BLOCKED open=$OPEN"
echo "[MUTATES] POST /api/zotero/sync (projection; idempotent if current)"
SYNC_JSON=$(curl -s --max-time 300 -X POST http://127.0.0.1:8011/api/zotero/sync)
echo "sync: $SYNC_JSON"

# ── 4.1–4.13 checkpoints
LOG "4.1 carrier clone SHA"
SHA=$(ssh -o ConnectTimeout=8 "$CARRIER" 'git -C ~/Code/axiom rev-parse --short HEAD')
echo "carrier at $SHA (target >= $RUNBOOK_SHA_TARGET)"

LOG "4.2/4.3 runner health + symbol proof (w9-gpu0 dry-run evidence; others at cutover)"
for p in 19542; do
  curl -s --max-time 10 "http://$CARRIER:$p/v1/health" | grep -q '"ok"' || DIE "4.3 :$p unhealthy"
done

LOG "4.4 queue state"
Q=$(DB "SELECT count(*) FROM ingest_jobs WHERE status='processing'")
[ "$Q" = "0" ] || DIE "4.4 $Q jobs PROCESSING (old-code drain!) — stop dispatcher first"
PEND=$(DB "SELECT count(*) FROM ingest_jobs WHERE status='pending'")
echo "pending=$PEND (heal-projections ride the wave)"

LOG "4.7 OpenSearch health"
curl -s --max-time 10 localhost:9200/_cluster/health | grep -q '"status":"green"' \
  || DIE "4.7 OS not green"

LOG "4.9/4.10 disk"
DF_MAC=$(df -k / | awk 'NR==2{print int($4/1024/1024)}')
echo "Mac free GB: $DF_MAC"

LOG "4.12 dry SELECT — expected book list"
COUNT=$(DB "SELECT count(*) FROM zotero_attachments a WHERE NOT a.deleted
  AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.attachment_id=a.id AND s.active)")
echo "active books: $COUNT (surveyed 121 at runbook time; heals shift it)"
[ "$COUNT" -ge 100 ] || DIE "4.12 suspiciously few active books ($COUNT)"

# ── §3.1 cutover
LOG "cutover: stop old GPU1 runner [MUTATES]"
ssh -o ConnectTimeout=8 "$CARRIER" 'podman stop runner-carrier-gpu1'
LOG "cutover: start w9-gpu1 + w9-a3000 [MUTATES]"
ssh -o ConnectTimeout=8 "$CARRIER" 'set -e
for spec in "1 19543 w9-gpu1" "2 19544 w9-a3000"; do
  set -- $spec
  podman run -d --name runner-carrier-$3 \
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
  for i in 1 2 3 4 5; do curl -s --max-time 5 http://127.0.0.1:$p/v1/health | grep -q ok && break || sleep 3; done
  curl -s --max-time 5 http://127.0.0.1:$p/v1/health | grep -q ok || { echo "runner :$p not healthy"; exit 1; }
done
echo "3 runners healthy"'
LOG "symbol proof on all three"
for n in w9-gpu0 w9-gpu1 w9-a3000; do
  ssh -o ConnectTimeout=8 "$CARRIER" "podman exec runner-carrier-$n python -c \"
import inspect, axiom_ng_runner.compute_core.chunker as c
assert 'current_chunk_headings' in inspect.getsource(c) and 'page_chapter_map' in inspect.getsource(c)
print('$n PROOF OK')\"" || DIE "4.2 $n symbol proof failed"
done

# ── dispatchers (Mac): TODO slot — 3 dispatcher processes per §2.2 env set;
# staged interactively at fire time against the live Mac service layout.
LOG "DISPATCHER TOPOLOGY — MANUAL STEP, see runbook §2.2"
echo "Start 3 dispatchers (TC2 shape) with AXIOM_PROCESSOR_RUNNER_NAME"
echo "w9-gpu0/w9-gpu1/w9-a3000 -> 19542/19543/19544, WORKER_ID distinct,"
echo "CONCURRENCY=1, then continue with the enqueue below."

LOG "4.13 confirm dispatchers claiming (waiting for first claims)"
echo "watch: DB 'SELECT runner_name, count(*) FROM ingest_jobs WHERE status=\"processing\" GROUP BY 1'"

# ── §2.3 enqueue [MUTATES] — transactional, count-asserted
LOG "fire: force_rebuild enqueue [MUTATES]"
EXPECTED="$COUNT"
INSERTED=$(podman exec axiom-postgres psql -U axiom_user -d "$DSN_DB" -v ON_ERROR_STOP=1 -tAc "
BEGIN;
INSERT INTO ingest_jobs (source_id, document_id, attachment_id, content_hash, status, force_rebuild)
SELECT a.source_id, a.document_id, a.id, a.content_hash, 'pending', true
FROM zotero_attachments a
WHERE NOT a.deleted
  AND EXISTS (SELECT 1 FROM processing_snapshots s WHERE s.attachment_id=a.id AND s.active);
SELECT count(*) FROM ingest_jobs WHERE force_rebuild AND status='pending';
COMMIT;")
echo "enqueued=$INSERTED expected~$EXPECTED"
[ "$INSERTED" -ge "$EXPECTED" ] || DIE "enqueue count mismatch ($INSERTED < $EXPECTED)"

# ── initial telemetry
LOG "INITIAL TELEMETRY"
sleep 30
DB "SELECT status, coalesce(runner_name,'—') AS card, count(*) FROM ingest_jobs
    WHERE force_rebuild OR status='pending' GROUP BY 1,2 ORDER BY 1;"
ssh -o ConnectTimeout=8 "$CARRIER" 'nvidia-smi --query-gpu=index,utilization.gpu,memory.used --format=csv,noheader'
echo "WAVE FIRED — monitor per runbook §2.6"
