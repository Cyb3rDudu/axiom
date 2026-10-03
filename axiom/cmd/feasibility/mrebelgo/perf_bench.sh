#!/bin/bash
# mREBEL perf bench (Restpunkt 6 optimizations): runs mrebelgo on the 50-chunk set
# twice (determinism), captures per-chunk timings, computes p50/p95.
#
# Usage (in study-mrebel container): perf_bench.sh <dylib> <modeldir> <chunks.json>
#   <parity_idxs.json> <out_prefix>
# Writes: <out_prefix>.json (run1), <out_prefix>_r2.json (run2), <out_prefix>.timings
set -u
DYLIB=$1; MDIR=$2; CHUNKS=$3; IDXS=$4; PREFIX=$5
cd /study/cmd/feasibility/mrebelgo
go build -o /tmp/mg .
echo "=== run 1 ==="
/tmp/mg "$DYLIB" "$MDIR" "$CHUNKS" "$IDXS" "${PREFIX}.json" 2>&1 \
  | grep -E "chunk [0-9]+: .* [0-9.]+s" | sed -E "s/.* ([0-9.]+)s/\1/" > "${PREFIX}.timings"
echo "=== run 2 (determinism) ==="
/tmp/mg "$DYLIB" "$MDIR" "$CHUNKS" "$IDXS" "${PREFIX}_r2.json" 2>&1 >/dev/null
echo "=== p50/p95 ==="
python3 - <<PYEOF
import statistics
ts = sorted(float(x.strip()) for x in open("${PREFIX}.timings") if x.strip())
n = len(ts)
if n:
    print(f"n={n} p50={ts[int(n*0.5)]:.3f}s p95={ts[int(n*0.95)]:.3f}s mean={statistics.mean(ts):.3f}s")
PYEOF
