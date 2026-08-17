#!/usr/bin/env python3
"""Trace analysis for the mREBEL perf campaign (#171).

Answers two questions from /tmp/mrebel_trace.jsonl:
 1. Per-call cost: ms per (kind, shape), split FIRST occurrence (plan+alloc) vs
    SUBSEQUENT occurrences (plan-cache hit) -> replan hypothesis verdict.
 2. Per-chunk: ORT calls, ORT ms vs wall ms (overhead outside ORT Run).

Usage: analyze_trace.py [trace.jsonl]
"""
import json
import statistics
import sys
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/mrebel_trace.jsonl"
events = [json.loads(l) for l in open(path) if l.strip()]

# 1. first vs subsequent per (kind, shape)
groups = defaultdict(list)
for e in events:
    groups[(e["kind"], e["shape"])].append(e["ms"])

print(f"{'kind':7} {'shape':10} {'n':>3} {'first ms':>9} {'med rest':>9} {'max rest':>9}")
slow_first = 0
for (kind, shape), ms in sorted(groups.items()):
    rest = ms[1:]
    med = statistics.median(rest) if rest else float("nan")
    mx = max(rest) if rest else float("nan")
    flag = ""
    if rest and ms[0] > 3 * med:
        flag = "  <-- first >> rest (plan?)"
        slow_first += 1
    print(f"{kind:7} {shape:10} {len(ms):>3} {ms[0]:>9.2f} {med:>9.2f} {mx:>9.2f}{flag}")

total_first = sum(ms[0] for ms in groups.values())
total_rest = sum(sum(ms[1:]) for ms in groups.values())
print(f"\nshape-classes: {len(groups)}, first-occurrence total: {total_first:.0f} ms, "
      f"subsequent total: {total_rest:.0f} ms")
print(f"VERDICT replan-hypothesis: {slow_first}/{len(groups)} shapes have first >> rest"
      f" -> {'CONFIRMED (per-shape warmup dominates first occurrence)' if slow_first > len(groups)*0.5 else 'NOT confirmed by first-vs-rest alone'}")

# 2. per-chunk calls + ort ms
per = defaultdict(lambda: [0, 0.0])
for e in events:
    per[e["chunk"]][0] += 1
    per[e["chunk"]][1] += e["ms"]
calls = [v[0] for v in per.values()]
ortms = [v[1] for v in per.values()]
if calls:
    print(f"\nper-chunk ORT: calls med={statistics.median(calls):.0f} "
          f"ort-ms med={statistics.median(ortms):.0f} "
          f"(= {statistics.median(ortms)/statistics.median(calls):.1f} ms/call median)")
