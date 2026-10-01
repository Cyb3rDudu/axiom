#!/bin/sh
# entrypoint.sh — the F14 topology image's role router: ONE image, roles
# per argument (#308). The RAG roles exec the axiom binary; the compute
# worker runs the reference-mode Python service. Everything else falls
# through to the caller's command (inspect/shell).
set -e

ROLE="${1:-}"

case "$ROLE" in
serve | serve\ all | serve\ api | serve\ library | serve\ store)
    # drop the leading role word ("serve X" -> "serve X"): the binary
    # takes the same argv
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
