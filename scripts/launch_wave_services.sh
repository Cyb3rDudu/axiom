#!/usr/bin/env bash
# Wave services launcher (owner ruling L1: gesetzt≠gefüllt must never boot).
# Reads the HMAC secret from /tmp/axiom_runs/.secret, asserts EVERY env
# value is non-empty before starting, and verifies each process's runtime
# env afterwards. A silent empty SOURCE_BASE_URL/SECRET dies loudly here —
# boot-time, not 130 jobs in.
set -euo pipefail
BIN="${1:-/tmp/axiom-ng-wave}"
SECRET_FILE="${SECRET_FILE:-/tmp/axiom_runs/.secret}"
DSN='postgresql://axiom_user:axiom_password@127.0.0.1:5432/axiom_db?sslmode=disable'
PROFILE='{"profile":"full-rag-v1","extract_entities":true,"extract_relationships":true,"compute_dense_embeddings":true,"compute_sparse_embeddings":true,"extract_images":true}'
mkdir -p /tmp/axiom_runs

SECRET=$(cat "$SECRET_FILE")
[ -n "$SECRET" ] || { echo "ABORT: $SECRET_FILE empty"; exit 1; }
[ "${#SECRET}" -ge 32 ] || { echo "ABORT: secret len ${#SECRET} < 32 (truncated recovery? ps eww truncates!)"; exit 1; }

kill $(cat /tmp/axiom_runs/api.pid 2>/dev/null) 2>/dev/null || true
for f in /tmp/axiom_runs/w9-dispatcher-*.pid; do kill $(cat "$f" 2>/dev/null) 2>/dev/null || true; done
sleep 2

COMMON_ENV=(AXIOM_DATABASE_URL="$DSN" AXIOM_OPENSEARCH_URL='http://127.0.0.1:9200'
  AXIOM_ARTIFACT_ROOT='/tmp/axiom_artifacts' AXIOM_PROCESSOR_TIMEOUT=30m
  AXIOM_PROCESSOR_SOURCE_BASE_URL='http://192.168.1.47:8011'
  AXIOM_PROCESSOR_SOURCE_SECRET="$SECRET")

env "${COMMON_ENV[@]}" \
  AXIOM_API_PORT=8011 AXIOM_BIND_ADDR=0.0.0.0 AXIOM_DISPATCHER_ENABLED=0 \
  AXIOM_PROCESSOR_URL='http://192.168.1.2:19542' AXIOM_PROCESSOR_RUNNER_NAME=carrier-w9-gpu0 \
  nohup "$BIN" > /tmp/axiom_runs/api-main.log 2>&1 &
echo $! > /tmp/axiom_runs/api.pid

i=0
for spec in "19542 w9-gpu0 8013" "19543 w9-gpu1 8014" "19544 w9-a3000 8015"; do
  set -- $spec; i=$((i+1))
  env "${COMMON_ENV[@]}" \
    AXIOM_API_PORT=$3 AXIOM_BIND_ADDR=127.0.0.1 \
    AXIOM_DISPATCHER_ENABLED=1 AXIOM_DISPATCHER_WORKER_ID="axiom-ng-w9-$i" \
    AXIOM_DISPATCHER_CONCURRENCY=1 AXIOM_DISPATCHER_LEASE=5m AXIOM_DISPATCHER_PROFILE="$PROFILE" \
    AXIOM_PROCESSOR_URL="http://192.168.1.2:$1" AXIOM_PROCESSOR_RUNNER_NAME="$2" \
    nohup "$BIN" > /tmp/axiom_runs/w9-dispatcher-$2.log 2>&1 &
  echo $! > /tmp/axiom_runs/w9-dispatcher-$2.pid
done

sleep 8
FAIL=0
for pidfile in /tmp/axiom_runs/api.pid /tmp/axiom_runs/w9-dispatcher-*.pid; do
  P=$(cat "$pidfile")
  kill -0 "$P" 2>/dev/null || { echo "DEAD: $pidfile"; FAIL=1; continue; }
  B=$(ps eww "$P" | tr ' ' '\n' | grep '^AXIOM_PROCESSOR_SOURCE_BASE_URL=' | head -1 | cut -d= -f2)
  S=$(ps eww "$P" | tr ' ' '\n' | grep '^AXIOM_PROCESSOR_SOURCE_SECRET=' | head -1 | cut -d= -f2)
  D=$(ps eww "$P" | tr ' ' '\n' | grep '^AXIOM_DATABASE_URL=' | head -1 | cut -d= -f2)
  { [ -n "$B" ] && [ -n "$S" ] && [ "${#S}" -ge 32 ] && [ -n "$D" ]; } \
    || { echo "ENV-BROKEN: $pidfile (base=$B secretlen=${#S} db=$([ -n "$D" ] && echo set || echo EMPTY))"; FAIL=1; }
  echo "$(basename "$pidfile" .pid): alive, source-env verified (secretlen=${#S})"
done
[ "$FAIL" = 0 ] || { echo "ABORT: refusing to leave broken services up"; exit 1; }
echo "all services up with verified env"
