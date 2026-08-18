#!/usr/bin/env python
"""Block 3 dense-parity reference: Python (MPS fp32) BGE-M3 dense embeddings
of the same 219 chunks, compared against the Go onnxruntime_go output.

Writes py_dense.bin (big-endian f32, 1024 per chunk) and prints cosine stats
avg/max (and cosine==1.0 hit rate) Go vs Python.

Usage: pydense_ref.py <chunks.json> <go_run.bin> <outdir>
       [--device auto|cuda|mps|cpu] [--no-fp16]
Device policy: auto = CUDA (fp16) > MPS > CPU; the historical parity runs
were MPS fp32 — reproduce with --device mps --no-fp16.
"""
import argparse
import json
import os
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _device import add_device_args, pick_device

p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
p.add_argument("chunks_path")
p.add_argument("go_bin")
p.add_argument("outdir")
add_device_args(p)
args = p.parse_args()
chunks_path, go_bin, outdir = args.chunks_path, args.go_bin, args.outdir

import numpy as np

with open(chunks_path) as f:
    chunks = json.load(f)

from FlagEmbedding import BGEM3FlagModel

dev, fp16 = pick_device(args.device, no_fp16=args.no_fp16, label="dense")
md = BGEM3FlagModel("BAAI/bge-m3", use_fp16=fp16, device=dev)

dense = []
for i, c in enumerate(chunks):
    out = md.encode([c["text"]], return_dense=True, return_sparse=False, max_length=8192)
    dense.append(np.asarray(out["dense_vecs"], dtype=np.float32)[0])

arr = np.stack(dense)                       # (N,1024) f32
# write py_dense.bin big-endian
with open(f"{outdir}/py_dense.bin", "wb") as f:
    f.writelines(struct.pack(">f", float(v)) for v in arr.ravel())

# load Go
with open(go_bin, "rb") as f:
    g = np.frombuffer(f.read(), dtype=">f4").reshape(len(chunks), 1024).astype(np.float32)
print("py shape", arr.shape, "go shape", g.shape)

def cos(a, b):
    return float(np.dot(a, b) / (np.linalg.norm(a) * np.linalg.norm(b) + 1e-12))

coss = [cos(arr[i], g[i]) for i in range(len(chunks))]
print(f"N={len(chunks)}")
print(f"cosine avg={np.mean(coss):.6f} min={np.min(coss):.6f} max={np.max(coss):.6f}")
print(f"cos==1.0 exactly: {sum(1 for c in coss if c >= 0.9999999)}/{len(coss)}")
print(f"L2 py={np.linalg.norm(arr[0]):.6f} go={np.linalg.norm(g[0]):.6f}")
print(f"max abs diff (normalized): {np.abs(arr - g).max():.2e}")
