# Restpunkt 5 — Mac Metal/MPS proof (backend decision + same-device measurement)

Backend decision (posted to #171, before measuring): **onnxruntime CoreML-EP via
`yalue/onnxruntime_go`** — the preferred single-backend path (same codepath as the
proven CUDA-EP, just a different ExecutionProvider), vs GoMLX (MLX-native) as the
alternative requiring a second backend. the Mac GPU window was opened. Measured
on the Mac (Apple Silicon, MPS), same-device: Go CoreML-EP vs Python-MPS.

## Correctness — at PARITY (proven)

Test instruments: `gocoreml` (Go onnxruntime_go CoreML-EP dense), `gorerankm`
(Go CoreML-EP rerank). Both deterministic (2× byte-equal). Note: an early 0.939
dense cosine was traced to a **stale/truncated `py_dense.bin` reference** — with a
fresh same-window Python-MPS reference the true cosine is 1.0 (the truncation-mismatch
lesson recurring).

| Metric | Go CoreML (Mac) vs Python-MPS | Result |
|---|---|---|
| Dense sentence_embedding (chunks 0-2) | cosine | **1.0** (all tested) |
| Rerank 75 pairs | Spearman | **0.99998**, avg \|d\| 2.1e-5, max 3.3e-4 |

The Go CoreML embeddings/rerank scores are numerically identical to the current
Python-MPS runner. Correctness is not the blocker.

## Performance — REGRESSION vs the status quo (the decisive finding)

The bar: the Mac Go backend must hold **at least the current MPS-Python level**
(rerank p95 ~5.3 s MPS). Measured:

| Endpoint | Go CoreML (Mac GPU) | Python-MPS (current runner) | CoreML ratio |
|---|---|---|---|
| `/v1/embed` (dense query) | **79.4 ms/query** | 40.5 ms/query | **~2× slower** |
| `/v1/rerank` (per pair) | **695 ms/pair** | 126 ms/pair | **~5.5× slower** |

**onnxruntime CoreML-EP is functional and correct but ~2-5× SLOWER than the
MPS-Python runner on the Mac** — it does NOT meet the "no regression" bar for the
always-on query-runner. (Tuning attempts: the 1.29 CoreML options I tried
(`coreml_precision`, `enable_coreml_ep`) are not valid in this build; default
options measured.)

## Backend decision outcome

- **(a) onnxruntime CoreML-EP: correct (1.0 / 0.99998) but ~2-5× slower than MPS.** 
  A single-backend path — but it would be a **regression** on the Mac query-runner.
  Viable only with further CoreML batching/config work (an Epic cost point).
- **(b) GoMLX (MLX-native on Metal): NOT yet measured** here; MLX is designed for
  Metal and may hit the MPS latency more closely, but requires a **second backend**
  (CUDA farm + MLX Mac) = the explicit Epic cost point.

**Verdict for the Epic:** the Mac Go backend is *correct* via CoreML-EP but would
regress performance 2-5× vs the status quo unless the CoreML path is batched/tuned
or GoMLX/MLX is adopted as a second backend. This is the remaining decision-input for
a follow-up epic, not a blockers for the CUDA-farm feasibility (the three pillars +
R7 parity stand on the carrier).
