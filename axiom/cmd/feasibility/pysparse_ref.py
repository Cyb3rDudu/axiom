#!/usr/bin/env python
"""Block-3 sparse parity reference (carrier Nachzug).

Both Go and Python run the SAME BGE-M3 sparse head (`sparse_head.onnx`) through
onnxruntime, then apply the same max-scatter by token id (specials 0-3 zeroed),
at the SAME max_length=8192. This isolates model/execution parity from any
post-processing difference.

Input: sample_chunks.json (corpus), sparse_head.onnx, the tokenizer spiece model,
       Go's sparse output (gosparse out.json: [{"i","doc","sparse"}]) and the
       token ids Go used (from a json dump to align max-scatter).
Compares each chunk's sparse (token-overlap + shared-weight cosine) vs Go.

Usage: pysparse_ref.py <chunks.json> <sparse_head.onnx> <tok_dir> <go_out.json> <outdir>
       [--device auto|cuda|cpu]
Device policy: auto = CUDA > CPU (onnxruntime has no MPS EP — MPS hosts
run the CPU EP). Historical runs were CUDA — reproduce with --device cuda.
"""
import argparse
import json
import math
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _device import add_device_args, ort_providers, pick_device

p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
p.add_argument("chunks_path")
p.add_argument("sp_head")
p.add_argument("tok_dir")
p.add_argument("go_out_path")
p.add_argument("outdir")
add_device_args(p)
args = p.parse_args()
chunks_path, sp_head, tok_dir, go_out_path, outdir = (
    args.chunks_path, args.sp_head, args.tok_dir, args.go_out_path, args.outdir)

with open(chunks_path) as f:
    chunks = json.load(f)
with open(go_out_path) as f:
    go = json.load(f)
assert len(chunks) == len(go), f"{len(chunks)} vs {len(go)}"

import numpy as np
import onnxruntime as ort
from transformers import AutoTokenizer

tok = AutoTokenizer.from_pretrained(tok_dir)
print("tokenizer-class", type(tok).__name__)

dev, _fp16 = pick_device(args.device, no_fp16=True, label="sparse")  # fp16 n/a for ONNX EPs
sess = ort.InferenceSession(sp_head, providers=ort_providers(dev))
print("providers", sess.get_providers())

def max_scatter(ids, weights):
    sp = {}
    for pos, vid in enumerate(ids):
        if vid in (0,1,2,3):
            continue
        w = float(weights[pos])
        if w > sp.get(vid, 0.0):
            sp[vid] = w
    return sp

rows = []
for i, c in enumerate(chunks):
    enc = tok.encode(c["text"], add_special_tokens=True)[:8192]
    ids64 = np.array([enc], dtype=np.int64)
    mask = np.ones((1, len(enc)), dtype=np.int64)
    tw = sess.run(['token_weights'], {'input_ids': ids64, 'attention_mask': mask})[0][0]
    sp = max_scatter(enc, tw)
    g = {int(k): float(v) for k, v in go[i]['sparse'].items()}
    common = set(sp) & set(g)
    overlap = len(common)/max(len(sp), 1)
    da = math.sqrt(sum(v*v for v in sp.values())); db = math.sqrt(sum(v*v for v in g.values()))
    cos = sum(sp[k]*g[k] for k in common)/((da*db)+1e-12)
    rows.append((len(sp), len(g), overlap, cos, go[i]['i']))

with open(f"{outdir}/sparse_py_ref.json", "w") as f:
    json.dump([{"i": r[4], "n_py": r[0], "n_go": r[1], "overlap": r[2], "cos": r[3]} for r in rows],
              f, indent=1)

ov = sum(r[2] for r in rows)/len(rows)
cs = sum(r[3] for r in rows)/len(rows)
print(f"N={len(rows)} sparse token-overlap avg={ov:.5f} shared-cos avg={cs:.5f}")
print("worst overlaps:", sorted(rows, key=lambda r: r[2])[:3])
print("worst cosines:", sorted(rows, key=lambda r: r[3])[:3])
print("overlap>=0.98 count:", sum(1 for r in rows if r[2] >= 0.98), "/", len(rows))
print("cos>=0.999 count:", sum(1 for r in rows if r[3] >= 0.999), "/", len(rows))
