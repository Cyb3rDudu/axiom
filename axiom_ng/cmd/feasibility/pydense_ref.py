#!/usr/bin/env python
"""Block 3 dense-parity reference: Python (MPS fp32) BGE-M3 dense embeddings
of the same 219 chunks, compared against the Go onnxruntime_go output.

Writes py_dense.bin (big-endian f32, 1024 per chunk) and prints cosine stats
avg/max (and cosine==1.0 hit rate) Go vs Python.

Usage: pydense_ref.py <chunks.json> <go_run.bin> <outdir>
"""
import json, sys, struct, math
import numpy as np

chunks_path, go_bin, outdir = sys.argv[1], sys.argv[2], sys.argv[3]

with open(chunks_path) as f:
    chunks = json.load(f)

from FlagEmbedding import BGEM3FlagModel
md = BGEM3FlagModel("BAAI/bge-m3", use_fp16=False, device="mps")

dense = []
for i, c in enumerate(chunks):
    out = md.encode([c["text"]], return_dense=True, return_sparse=False, max_length=8192)
    dense.append(np.asarray(out["dense_vecs"], dtype=np.float32)[0])

arr = np.stack(dense)                       # (N,1024) f32
# write py_dense.bin big-endian
with open(f"{outdir}/py_dense.bin", "wb") as f:
    for v in arr.ravel():
        f.write(struct.pack(">f", float(v)))

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
