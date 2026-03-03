#!/usr/bin/env python3
"""
POC: Test BAAI/bge-m3 and BAAI/bge-reranker-v2-m3 on Apple Silicon MPS.

Validates that FlagEmbedding models can run on Metal Performance Shaders,
compares output quality against CPU baseline, and benchmarks performance.

Usage:
    PYTORCH_ENABLE_MPS_FALLBACK=1 python scripts/poc_mps_flagembedding.py
"""

import os
import sys
import time
import json
import traceback
import numpy as np

# Enable MPS fallback for unsupported ops
os.environ.setdefault("PYTORCH_ENABLE_MPS_FALLBACK", "1")

import torch

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def cosine_similarity(a, b):
    """Cosine similarity between two vectors."""
    a, b = np.array(a, dtype=np.float32), np.array(b, dtype=np.float32)
    return float(np.dot(a, b) / (np.linalg.norm(a) * np.linalg.norm(b)))


def section(title):
    print(f"\n{'='*70}")
    print(f"  {title}")
    print(f"{'='*70}\n")


def status(ok, msg):
    icon = "PASS" if ok else "FAIL"
    print(f"  [{icon}] {msg}")


# ---------------------------------------------------------------------------
# System info
# ---------------------------------------------------------------------------

def print_system_info():
    section("System Info")
    print(f"  Python:          {sys.version.split()[0]}")
    print(f"  PyTorch:         {torch.__version__}")
    print(f"  MPS available:   {torch.backends.mps.is_available()}")
    print(f"  MPS built:       {torch.backends.mps.is_built()}")
    print(f"  MPS fallback:    {os.environ.get('PYTORCH_ENABLE_MPS_FALLBACK', 'not set')}")

    if torch.backends.mps.is_available():
        # Quick MPS smoke test
        try:
            t = torch.tensor([1.0, 2.0, 3.0], device="mps")
            r = t * 2
            assert r.tolist() == [2.0, 4.0, 6.0]
            status(True, "MPS basic tensor ops work")
        except Exception as e:
            status(False, f"MPS basic tensor ops failed: {e}")
            return False
    else:
        status(False, "MPS not available — cannot run POC")
        return False
    return True


# ---------------------------------------------------------------------------
# Test data
# ---------------------------------------------------------------------------

TEST_SENTENCES = [
    "Artificial intelligence is transforming scientific research methodology.",
    "The weather forecast predicts rain for tomorrow afternoon.",
    "Quantum computing may revolutionize cryptography and drug discovery.",
    "The cat sat on the mat and watched the birds outside.",
    "Neural networks can approximate any continuous function given sufficient width.",
]

RERANK_QUERY = "How does AI impact scientific research?"
RERANK_PASSAGES = [
    "AI is transforming how scientists analyze data and form hypotheses.",
    "The stock market closed higher today driven by tech earnings.",
    "Machine learning models help researchers identify patterns in genomic data.",
    "A new restaurant opened downtown serving Mediterranean cuisine.",
    "Deep learning accelerates drug discovery by predicting molecular interactions.",
]

# Expected relevance order: [0, 2, 4] should rank higher than [1, 3]
RELEVANT_INDICES = {0, 2, 4}


# ---------------------------------------------------------------------------
# BGE-M3 Embedding Test
# ---------------------------------------------------------------------------

def test_bge_m3():
    section("Test 1: BAAI/bge-m3 (Embedder)")
    from FlagEmbedding import BGEM3FlagModel

    results = {"model": "BAAI/bge-m3", "tests": {}}

    # --- Load on CPU (baseline) ---
    print("  Loading model on CPU...")
    t0 = time.time()
    try:
        cpu_model = BGEM3FlagModel("BAAI/bge-m3", use_fp16=False, device="cpu")
        cpu_load_time = time.time() - t0
        status(True, f"CPU load: {cpu_load_time:.1f}s")
    except Exception as e:
        status(False, f"CPU load failed: {e}")
        traceback.print_exc()
        results["tests"]["cpu_load"] = {"pass": False, "error": str(e)}
        return results

    # CPU encode
    print("  Encoding on CPU...")
    t0 = time.time()
    cpu_out = cpu_model.encode(
        TEST_SENTENCES, return_dense=True, return_sparse=True, return_colbert_vecs=False
    )
    cpu_encode_time = time.time() - t0
    cpu_dense = cpu_out["dense_vecs"]
    cpu_sparse = cpu_out["lexical_weights"]
    status(True, f"CPU encode ({len(TEST_SENTENCES)} sentences): {cpu_encode_time:.2f}s")
    print(f"  Dense shape: {cpu_dense.shape}, dtype: {cpu_dense.dtype}")
    results["tests"]["cpu_encode"] = {
        "pass": True,
        "time_s": round(cpu_encode_time, 3),
        "dense_shape": list(cpu_dense.shape),
    }

    # Free CPU model
    del cpu_model
    torch.mps.empty_cache() if torch.backends.mps.is_available() else None
    import gc; gc.collect()

    # --- Load on MPS ---
    print("\n  Loading model on MPS...")
    t0 = time.time()
    try:
        mps_model = BGEM3FlagModel("BAAI/bge-m3", use_fp16=False, device="mps")
        mps_load_time = time.time() - t0
        status(True, f"MPS load: {mps_load_time:.1f}s")
        results["tests"]["mps_load"] = {"pass": True, "time_s": round(mps_load_time, 1)}
    except Exception as e:
        status(False, f"MPS load failed: {e}")
        traceback.print_exc()
        results["tests"]["mps_load"] = {"pass": False, "error": str(e)}
        return results

    # MPS encode
    print("  Encoding on MPS...")
    t0 = time.time()
    try:
        mps_out = mps_model.encode(
            TEST_SENTENCES, return_dense=True, return_sparse=True, return_colbert_vecs=False
        )
        mps_encode_time = time.time() - t0
        mps_dense = mps_out["dense_vecs"]
        mps_sparse = mps_out["lexical_weights"]
        status(True, f"MPS encode ({len(TEST_SENTENCES)} sentences): {mps_encode_time:.2f}s")
        print(f"  Dense shape: {mps_dense.shape}, dtype: {mps_dense.dtype}")

        speedup = cpu_encode_time / mps_encode_time if mps_encode_time > 0 else 0
        print(f"  Speedup vs CPU: {speedup:.2f}x")

        results["tests"]["mps_encode"] = {
            "pass": True,
            "time_s": round(mps_encode_time, 3),
            "speedup": round(speedup, 2),
        }
    except Exception as e:
        status(False, f"MPS encode failed: {e}")
        traceback.print_exc()
        results["tests"]["mps_encode"] = {"pass": False, "error": str(e)}
        return results

    # --- Compare CPU vs MPS output quality ---
    print("\n  Comparing CPU vs MPS dense embeddings...")
    sims = []
    for i in range(len(TEST_SENTENCES)):
        sim = cosine_similarity(cpu_dense[i], mps_dense[i])
        sims.append(sim)

    avg_sim = np.mean(sims)
    min_sim = np.min(sims)
    print(f"  Cosine similarity (CPU vs MPS per sentence):")
    for i, sim in enumerate(sims):
        print(f"    [{i}] {sim:.6f}")
    print(f"  Average: {avg_sim:.6f}, Min: {min_sim:.6f}")

    quality_ok = min_sim > 0.99
    status(quality_ok, f"Quality check (min cosine > 0.99): min={min_sim:.6f}")
    results["tests"]["quality"] = {
        "pass": quality_ok,
        "avg_cosine": round(float(avg_sim), 6),
        "min_cosine": round(float(min_sim), 6),
    }

    # --- Check sparse embeddings ---
    sparse_ok = all(len(s) > 0 for s in mps_sparse)
    status(sparse_ok, f"Sparse embeddings non-empty: {[len(s) for s in mps_sparse]}")
    results["tests"]["sparse"] = {"pass": sparse_ok}

    # --- Batch benchmark ---
    print("\n  Batch benchmark (3 runs of 5 sentences)...")
    times_cpu = []
    times_mps = []

    # Reload CPU model for fair benchmark
    cpu_model2 = BGEM3FlagModel("BAAI/bge-m3", use_fp16=False, device="cpu")

    for _ in range(3):
        t0 = time.time()
        cpu_model2.encode(TEST_SENTENCES, return_dense=True, return_sparse=True, return_colbert_vecs=False)
        times_cpu.append(time.time() - t0)

        t0 = time.time()
        mps_model.encode(TEST_SENTENCES, return_dense=True, return_sparse=True, return_colbert_vecs=False)
        times_mps.append(time.time() - t0)

    avg_cpu = np.mean(times_cpu)
    avg_mps = np.mean(times_mps)
    speedup = avg_cpu / avg_mps if avg_mps > 0 else 0
    print(f"  CPU avg: {avg_cpu:.3f}s | MPS avg: {avg_mps:.3f}s | Speedup: {speedup:.2f}x")

    results["tests"]["benchmark"] = {
        "pass": True,
        "cpu_avg_s": round(float(avg_cpu), 3),
        "mps_avg_s": round(float(avg_mps), 3),
        "speedup": round(float(speedup), 2),
    }

    del cpu_model2, mps_model
    import gc; gc.collect()

    return results


# ---------------------------------------------------------------------------
# BGE-Reranker Test
# ---------------------------------------------------------------------------

def test_bge_reranker():
    section("Test 2: BAAI/bge-reranker-v2-m3 (Reranker)")
    from FlagEmbedding import FlagReranker

    results = {"model": "BAAI/bge-reranker-v2-m3", "tests": {}}
    pairs = [[RERANK_QUERY, p] for p in RERANK_PASSAGES]

    # --- CPU baseline ---
    print("  Loading reranker on CPU...")
    t0 = time.time()
    try:
        cpu_reranker = FlagReranker("BAAI/bge-reranker-v2-m3", use_fp16=False, device="cpu")
        cpu_load_time = time.time() - t0
        status(True, f"CPU load: {cpu_load_time:.1f}s")
    except Exception as e:
        status(False, f"CPU load failed: {e}")
        traceback.print_exc()
        results["tests"]["cpu_load"] = {"pass": False, "error": str(e)}
        return results

    print("  Scoring on CPU...")
    t0 = time.time()
    cpu_scores = cpu_reranker.compute_score(pairs, normalize=True)
    cpu_score_time = time.time() - t0
    if not isinstance(cpu_scores, list):
        cpu_scores = [cpu_scores]
    print(f"  CPU scores: {[round(s, 4) for s in cpu_scores]}")
    status(True, f"CPU scoring: {cpu_score_time:.3f}s")
    results["tests"]["cpu_score"] = {"pass": True, "time_s": round(cpu_score_time, 3)}

    del cpu_reranker
    import gc; gc.collect()

    # --- MPS ---
    print("\n  Loading reranker on MPS (FP16)...")
    t0 = time.time()
    try:
        mps_reranker = FlagReranker("BAAI/bge-reranker-v2-m3", use_fp16=True, device="mps")
        mps_load_time = time.time() - t0
        status(True, f"MPS load: {mps_load_time:.1f}s")
        results["tests"]["mps_load"] = {"pass": True, "time_s": round(mps_load_time, 1)}
    except Exception as e:
        status(False, f"MPS load failed: {e}")
        traceback.print_exc()
        results["tests"]["mps_load"] = {"pass": False, "error": str(e)}
        return results

    print("  Scoring on MPS...")
    t0 = time.time()
    try:
        mps_scores = mps_reranker.compute_score(pairs, normalize=True)
        mps_score_time = time.time() - t0
        if not isinstance(mps_scores, list):
            mps_scores = [mps_scores]
        print(f"  MPS scores: {[round(s, 4) for s in mps_scores]}")

        speedup = cpu_score_time / mps_score_time if mps_score_time > 0 else 0
        print(f"  Speedup vs CPU: {speedup:.2f}x")
        status(True, f"MPS scoring: {mps_score_time:.3f}s")

        results["tests"]["mps_score"] = {
            "pass": True,
            "time_s": round(mps_score_time, 3),
            "speedup": round(float(speedup), 2),
        }
    except Exception as e:
        status(False, f"MPS scoring failed: {e}")
        traceback.print_exc()
        results["tests"]["mps_score"] = {"pass": False, "error": str(e)}
        return results

    # --- Score correlation ---
    print("\n  Comparing CPU vs MPS ranking...")
    cpu_ranking = sorted(range(len(cpu_scores)), key=lambda i: cpu_scores[i], reverse=True)
    mps_ranking = sorted(range(len(mps_scores)), key=lambda i: mps_scores[i], reverse=True)
    print(f"  CPU ranking: {cpu_ranking}")
    print(f"  MPS ranking: {mps_ranking}")

    ranking_match = cpu_ranking == mps_ranking
    status(ranking_match, f"Ranking order match: {ranking_match}")

    # Check that relevant passages rank in top 3
    mps_top3 = set(mps_ranking[:3])
    relevance_ok = mps_top3 == RELEVANT_INDICES
    status(relevance_ok, f"Top-3 are relevant indices {RELEVANT_INDICES}: got {mps_top3}")

    # Score correlation
    score_diffs = [abs(c - m) for c, m in zip(cpu_scores, mps_scores)]
    max_diff = max(score_diffs)
    avg_diff = np.mean(score_diffs)
    print(f"  Score diffs (CPU vs MPS): max={max_diff:.4f}, avg={avg_diff:.4f}")

    results["tests"]["ranking"] = {
        "pass": ranking_match,
        "cpu_ranking": cpu_ranking,
        "mps_ranking": mps_ranking,
        "relevance_ok": relevance_ok,
        "max_score_diff": round(float(max_diff), 4),
    }

    # --- Benchmark ---
    print("\n  Batch benchmark (3 runs)...")
    cpu_reranker2 = FlagReranker("BAAI/bge-reranker-v2-m3", use_fp16=False, device="cpu")
    times_cpu, times_mps = [], []
    for _ in range(3):
        t0 = time.time()
        cpu_reranker2.compute_score(pairs, normalize=True)
        times_cpu.append(time.time() - t0)

        t0 = time.time()
        mps_reranker.compute_score(pairs, normalize=True)
        times_mps.append(time.time() - t0)

    avg_cpu = np.mean(times_cpu)
    avg_mps = np.mean(times_mps)
    speedup = avg_cpu / avg_mps if avg_mps > 0 else 0
    print(f"  CPU avg: {avg_cpu:.3f}s | MPS avg: {avg_mps:.3f}s | Speedup: {speedup:.2f}x")

    results["tests"]["benchmark"] = {
        "pass": True,
        "cpu_avg_s": round(float(avg_cpu), 3),
        "mps_avg_s": round(float(avg_mps), 3),
        "speedup": round(float(speedup), 2),
    }

    del cpu_reranker2, mps_reranker
    import gc; gc.collect()

    return results


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    section("POC: FlagEmbedding Models on Apple Silicon MPS")

    if not print_system_info():
        sys.exit(1)

    all_results = {}
    all_pass = True

    # Test 1: BGE-M3
    try:
        r = test_bge_m3()
        all_results["bge_m3"] = r
        for name, t in r["tests"].items():
            if not t.get("pass", False):
                all_pass = False
    except Exception as e:
        print(f"\n  [FAIL] BGE-M3 test crashed: {e}")
        traceback.print_exc()
        all_results["bge_m3"] = {"error": str(e)}
        all_pass = False

    # Test 2: Reranker
    try:
        r = test_bge_reranker()
        all_results["bge_reranker"] = r
        for name, t in r["tests"].items():
            if not t.get("pass", False):
                all_pass = False
    except Exception as e:
        print(f"\n  [FAIL] Reranker test crashed: {e}")
        traceback.print_exc()
        all_results["bge_reranker"] = {"error": str(e)}
        all_pass = False

    # --- Summary ---
    section("Summary")
    for model_key, data in all_results.items():
        if "error" in data:
            print(f"  {model_key}: CRASHED — {data['error']}")
            continue
        print(f"  {data['model']}:")
        for test_name, test_data in data["tests"].items():
            icon = "PASS" if test_data.get("pass") else "FAIL"
            extras = {k: v for k, v in test_data.items() if k != "pass"}
            print(f"    [{icon}] {test_name}: {extras}")

    print(f"\n  Overall: {'ALL PASSED' if all_pass else 'SOME FAILURES'}")

    # Save results JSON
    results_path = os.path.join(os.path.dirname(__file__), "poc_mps_results.json")

    # Python 3.14 bool serialization fix
    class SafeEncoder(json.JSONEncoder):
        def default(self, obj):
            if isinstance(obj, bool):
                return True if obj else False
            return super().default(obj)

    def make_serializable(obj):
        if isinstance(obj, dict):
            return {k: make_serializable(v) for k, v in obj.items()}
        if isinstance(obj, list):
            return [make_serializable(v) for v in obj]
        if isinstance(obj, bool):
            return 1 if obj else 0
        if isinstance(obj, (np.bool_,)):
            return 1 if obj else 0
        if isinstance(obj, (np.integer,)):
            return int(obj)
        if isinstance(obj, (np.floating,)):
            return float(obj)
        return obj

    with open(results_path, "w") as f:
        json.dump(make_serializable(all_results), f, indent=2)
    print(f"  Results saved to: {results_path}")

    sys.exit(0 if all_pass else 1)


if __name__ == "__main__":
    main()
