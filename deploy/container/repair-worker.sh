#!/bin/sh
# axiom-repair-worker — canonical in-container wrapper (deploy/container).
#
# Contract (repair/executor.go workerArgs): the invoker execs
#   Command <key> --apply [ --lang L | --ocr-mode force ]
# The mothership shim (scripts/fix.sh) translated the positional key to
# repair_agent.py's --key; this wrapper does the same for the bare agent.
#
# EPUB-arm note: --format/--source have no counterpart in repair_agent.py
# (that arm needs the EPUB repair CLI, not wired in-container yet) —
# reject loudly instead of silently misrouting.
KEY="${1:-}"
[ -n "$KEY" ] && shift
case " $* " in
  *" --format "*|*" --source "*)
    echo "axiom-repair-worker: epub arm not wired in-container" >&2
    exit 64
    ;;
esac
exec python3 /opt/axiom/fixer/app/repair_agent.py --key "$KEY" "$@"
