#!/bin/sh
# entrypoint.sh — the F14 topology image's role router: ONE image, roles
# per argument (#308). The RAG roles exec the axiom binary; the compute
# worker runs the reference-mode Python service. Everything else falls
# through to the caller's command (inspect/shell).
set -e

ROLE="${1:-}"

case "$ROLE" in
serve*)
    exec /usr/local/bin/axiom "$@"
    ;;
worker)
    shift
    exec env AXIOM_PROCESSOR_COMPUTE="${AXIOM_PROCESSOR_COMPUTE:-reference}" \
        PYTHONPATH=/opt/axiom/worker \
        python3 -m axiom_compute_worker "$@"
    ;;
"")
    echo "usage: <image> serve all|api|library|store | worker | <cmd>" >&2
    exit 2
    ;;
*)
    exec "$@"
    ;;
esac
