#!/usr/bin/env python
"""Block 3 rerank-parity reference: Python FlagReranker (bge-reranker-v2-m3,
MPS fp32) scores for the same rerank_pairs.json, compared against the Go
gorerank output (out.json: [{"query","doc","score"}, ...]) via
Spearman/Kendall correlation (scipy).

NOTE: the Block-3 measured Spearman 0.978 was produced with gorerank's
pre-auto-review pair form (<s> q </s> p </s> — single </s>); gorerank now
emits the HF XLM-R pair form (<s> q </s> </s> p </s>). Re-run both sides
before citing final parity. (see docs/research/03-dense-parity.md)

Usage: pyrerank_ref.py <pairs.json> <gorerank_out.json> <outdir>
Writes <outdir>/py_rerank.json = [{"query","doc","score"}, ...].
"""
import json, sys, os

pairs_path, go_out_path, outdir = sys.argv[1], sys.argv[2], sys.argv[3]
os.makedirs(outdir, exist_ok=True)

with open(pairs_path) as f:
    pairs = json.load(f)
with open(go_out_path) as f:
    go = json.load(f)
assert len(pairs) == len(go), f"pair count mismatch: {len(pairs)} vs {len(go)}"

from FlagEmbedding import FlagReranker
dev = sys.argv[4] if len(sys.argv) > 4 else "cuda"
rrk = FlagReranker("BAAI/bge-reranker-v2-m3", use_fp16=False, devices=dev.split(","))

texts = [[p["query"], p["passage"]] for p in pairs]
scores = rrk.compute_score(texts, normalize=True)  # sigmoid, matches Go

out = [{"query": p["query"], "doc": p["doc"], "score": float(s)}
       for p, s in zip(pairs, scores)]
with open(f"{outdir}/py_rerank.json", "w") as f:
    json.dump(out, f, ensure_ascii=False, indent=1)

go_scores = [g["score"] for g in go]
py_scores = [o["score"] for o in out]
n = len(go_scores)
mad = sum(abs(a - b) for a, b in zip(go_scores, py_scores)) / n
mx = max(abs(a - b) for a, b in zip(go_scores, py_scores))
print(f"n={n} |score| avg={mad:.6f} max={mx:.6f}")

from scipy.stats import spearmanr, kendalltau
print(f"Spearman={spearmanr(go_scores, py_scores).statistic:.4f} "
      f"Kendall={kendalltau(go_scores, py_scores).statistic:.4f}")
