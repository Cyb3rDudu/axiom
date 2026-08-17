#!/usr/bin/env python3
"""
mREBEL parity + determinism gate (Restpunkt 6 optimizations, #171).

Regression gate after EVERY optimization:
  1. triple-set parity >= 95% on the same 50-chunk set (vs mrebel_ref_50.json)
  2. 2x byte-determinism of the Go output

Usage: parity_gate.py <go_output.json> <go_output_r2.json> <python_ref.json>
Exit 0 if gate passes, 1 otherwise. Prints per-chunk mismatch detail.
"""
import json
import sys


def norm_triples(ts):
    return set((t["head"].lower(), t["relation"], t["tail"].lower()) for t in ts)


def main():
    go_path, go_r2_path, ref_path = sys.argv[1], sys.argv[2], sys.argv[3]
    go = json.load(open(go_path))
    go_r2 = json.load(open(go_r2_path))
    ref = json.load(open(ref_path))
    refmap = {x["idx"]: x for x in ref if "raw_sequences" in x}
    gomap = {x["idx"]: x for x in go}

    # determinism
    b1 = open(go_path, "rb").read()
    b2 = open(go_r2_path, "rb").read()
    det = b1 == b2
    print(f"determinism (byte-equal run1 vs run2): {det}")

    # triple-set parity
    match = 0
    n = 0
    mismatches = []
    for idx, g in sorted(gomap.items()):
        r = refmap.get(idx)
        if not r:
            continue
        n += 1
        gs = norm_triples(g["triples"])
        rs = norm_triples(r["triples"])
        if gs == rs:
            match += 1
        else:
            mismatches.append((idx, sorted(gs - rs), sorted(rs - gs)))
    pct = match / n * 100 if n else 0
    print(f"triple-set parity: {match}/{n} = {pct:.1f}% (threshold >= 95%)")
    for idx, go_only, py_only in mismatches:
        print(f"  mismatch chunk {idx}: go-only={go_only} py-only={py_only}")

    ok = det and pct >= 95.0
    print(f"GATE: {'PASS' if ok else 'FAIL'}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
