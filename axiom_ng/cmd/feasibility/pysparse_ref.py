#!/usr/bin/env python
"""Block-3 sparse parity reference, CUDA (carrier Nachzug).

Both Go and Python run the SAME BGE-M3 sparse head (`sparse_head.onnx`) through
onnxruntime, then apply the same max-scatter by token id (specials 0-3 zeroed),
at the SAME max_length=8192. This isolates model/execution parity from any
post-processing difference.

Input: sample_chunks.json (corpus), sparse_head.onnx, the tokenizer spiece model,
       Go's sparse output (gosparse out.json: [{"i","doc","sparse"}]) and the
       token ids Go used (from a json dump to align max-scatter).
Compares each chunk's sparse (token-overlap + shared-weight cosine) vs Go.
"""
import json, sys, math, struct
import numpy as np
import onnxruntime as ort
import os

chunks_path, sp_head, spm_path, go_out_path, outdir = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]

with open(chunks_path) as f:
    chunks = json.load(f)
with open(go_out_path) as f:
    go = json.load(f)
assert len(chunks) == len(go), f"{len(chunks)} vs {len(go)}"

from transformers import AutoTokenizer
tok = AutoTokenizer.from_pretrained(tok_dir)
print("tokenizer-class", type(tok).__name__)

sess = ort.InferenceSession(sp_head, providers=['CUDAExecutionProvider','CPUExecutionProvider'])
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
