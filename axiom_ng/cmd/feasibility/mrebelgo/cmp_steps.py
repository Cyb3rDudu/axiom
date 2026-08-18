#!/usr/bin/env python3
"""First-divergence diff: no-cache vs cached beam search (#171).

Reads /tmp/mrebel_steps.jsonl (MRBEL_DUMP_STEPS=1 runs, paths "nocache" + "cached"),
walks the steps per chunk in parallel and reports the FIRST step where the two
paths' beam hypotheses diverge — plus the top-6 candidate scores at that step, to
distinguish a near-tie FP flip (gap < ~1e-3) from a logic bug (large gap /
already-different parent states).

Usage: cmp_steps.py <chunk_ids_csv> [steps.jsonl]
"""
import json
import sys
from collections import defaultdict

chunks = [int(x) for x in sys.argv[1].split(",")]
path = sys.argv[2] if len(sys.argv) > 2 else "/tmp/mrebel_steps.jsonl"

steps = defaultdict(lambda: defaultdict(list))  # chunk -> path -> [step records]
for line in open(path):
    r = json.loads(line)
    if r["chunk"] in chunks:
        steps[r["chunk"]][r["path"]].append(r)

for ci in chunks:
    nc = steps[ci].get("nocache", [])
    ca = steps[ci].get("cached", [])
    if not nc or not ca:
        print(f"chunk {ci}: missing path data (nocache={len(nc)} cached={len(ca)})")
        continue
    print(f"\n=== chunk {ci}: nocache {len(nc)} steps, cached {len(ca)} steps ===")
    div = None
    for s_nc, s_ca in zip(nc, ca):
        ids_nc = sorted(tuple(b["ids"]) for b in s_nc["beams"])
        ids_ca = sorted(tuple(b["ids"]) for b in s_ca["beams"])
        if ids_nc != ids_ca:
            div = (s_nc, s_ca)
            break
    if div is None:
        print("  no divergence in aligned prefix (one path may be longer)")
        continue
    s_nc, s_ca = div
    print(f"  FIRST DIVERGENCE at step {s_nc['step']} (nocache beams vs cached beams):")
    for tag, s in (("nocache", s_nc), ("cached", s_ca)):
        for b in s["beams"]:
            print(f"    {tag:8} ids={b['ids'][-6:]} score={b['score']:.4f} top6={[(t, round(p,4)) for t,p in b['top6']]}")
    # near-tie check: same parent ids, compare top6 logp gaps
    by_ids_nc = {tuple(b["ids"]): b for b in s_nc["beams"]}
    by_ids_ca = {tuple(b["ids"]): b for b in s_ca["beams"]}
    shared = set(by_ids_nc) & set(by_ids_ca)
    for pid in shared:
        t_nc = by_ids_nc[pid]["top6"]
        t_ca = by_ids_ca[pid]["top6"]
        toks_nc = [t for t, _ in t_nc]
        toks_ca = [t for t, _ in t_ca]
        if toks_nc[:3] != toks_ca[:3]:
            print(f"    same parent {pid[-4:]}: top3 order differs -> "
                  f"nc={[(t, round(p,5)) for t,p in t_nc[:4]]} ca={[(t, round(p,5)) for t,p in t_ca[:4]]}")
            gap = abs(t_nc[0][1] - t_nc[1][1]) if len(t_nc) > 1 else float("nan")
            print(f"    nc top1-top2 logp gap: {gap:.6f} -> {'NEAR-TIE (FP)' if gap < 1e-3 else 'LARGE GAP (logic suspect)'}")
