#!/usr/bin/env python
"""Block-3 dense parity reference, CUDA (torch) — carrier Nachzug.
Runs BGEM3FlagModel (torch cuda) on the study corpus, writes py_dense.bin
(big-endian f32, 1024/chunk), prints cosine stats vs Go's go_run1.bin.
Usage: pydense_ref_cuda.py <sample_chunks.json> <go_run.bin> <outdir> <device=cuda>
"""
import json, sys, struct
import numpy as np
chunks_path, go_bin, outdir, device = sys.argv[1], sys.argv[2], sys.argv[3], (sys.argv[4] if len(sys.argv)>4 else 'cuda')
with open(chunks_path) as f: chunks = json.load(f)
from FlagEmbedding import BGEM3FlagModel
md = BGEM3FlagModel("BAAI/bge-m3", use_fp16=False, device=device)
dense=[]
for c in chunks:
    out = md.encode([c["text"]], return_dense=True, return_sparse=False, max_length=8192)
    dense.append(np.asarray(out["dense_vecs"], dtype=np.float32)[0])
arr=np.stack(dense)
with open(f"{outdir}/py_dense.bin","wb") as f:
    for v in arr.ravel(): f.write(struct.pack(">f", float(v)))
with open(go_bin,"rb") as f: g=np.frombuffer(f.read(),dtype=">f4").reshape(len(chunks),1024).astype(np.float32)
def cos(a,b): return float(np.dot(a,b)/(np.linalg.norm(a)*np.linalg.norm(b)+1e-12))
coss=[cos(arr[i],g[i]) for i in range(len(chunks))]
print(f"device={device} N={len(chunks)}")
print(f"cosine avg={np.mean(coss):.6f} min={np.min(coss):.6f} max={np.max(coss):.6f}")
print(f"cos>=0.999 count: {sum(1 for c2 in coss if c2>=0.999)}/{len(coss)}")
print(f"cos==1.0 exact: {sum(1 for c2 in coss if c2>=0.9999999)}/{len(coss)}")
print(f"max abs diff: {float(np.abs(arr-g).max()):.2e}")
