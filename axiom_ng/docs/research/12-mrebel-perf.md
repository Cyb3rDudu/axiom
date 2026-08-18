# 12 — mREBEL Decoder Performance (G2 #179 pre-research, #171 continuation)

The target hierarchy: **p50 ≤ 0.2 s per chunk = "no regression" (end-goal threshold)**;
0.5 s was only a milestone. Sidecar = the very last data-backed reserve. Regression gate
after EVERY step: triple-set parity ≥ 95% against `mrebel_ref_50.json` (n=50) + 2×
determinism (byte-equal). Measurement environment: carrier 192.168.1.2, RTX 3090 GPU 0,
container `study-mrebel`, everything on GPU.

## Step 0 — GPU hygiene & exclusivity (BEFORE the measurements)

- `study-minirunner` (8 h up, 6.5 GB on GPU 0, continuously **0% util** — R7 verified,
  obsolete) and `runner-carrier-gpu1` (Python, 4 GB on GPU 1; 3× 0% util proven before the
  stop) **stopped** (`podman stop`, not removed — `podman start` revives them). After that:
  all GPUs at 1 MiB, no compute processes → **mrebelgo exclusive on GPU 0**.
- Control finding: the baseline with the (idle) minirunner resident (p50 4.45 s) is
  **identical** after exclusivity (4.459 s) — the old numbers were not contaminated; the
  bottleneck was CPU-side anyway (see step 1).

## Step 1 — Diagnosis (measure before building)

Instrumentation (`MRBEL_TRACE=1` → `/tmp/mrebel_trace.jsonl`, one JSONL per ORT call;
`MRBEL_DUMP_STEPS=1` → beam-step dump; `analyze_trace.py` / `cmp_steps.py`).

**Repeat-chunk test** (chunk 0 ×10): 2.81 s → 2.64 s (warm-up ~6%). **The shape-replan
hypothesis is thereby definitively refuted** — no first-occurrence effect per shape
(trace: 0/118 shapes "first >> rest", only the encoder 41→8 ms).

**The actual finding (trace, measured exclusively):**

| Quantity | Value |
|---|---|
| ORT calls per chunk | ~44 (repeat-chunk) / 68 (50-set, median) |
| **Time per ORT run** | **median 6.7–7.0 ms** — ORT/CUDA is fast, dynamic shapes cost nothing |
| ORT total per chunk | ~456 ms |
| Wall per chunk (at the time) | 2,650 ms → **~2.2 s sat in GO CODE between the calls** |
| GPU util during the run | 6–13% (GPU nearly idle → CPU-bound) |

**The bottleneck was `topKIndices`: full sort of 250,071 indices per beam call**
(~50–100 ms × 44–68 calls) plus the `logSoftmax` double pass (250k float64). My earlier
"90 ms fixed ORT per-call overhead" interpretation was an artifact (calls miscounted).

**Python study ("study the blueprint", the directive):** transformers 4.57.6 default
`generate()` for MBART = **DynamicCache with `torch.cat(key, dim=-2)` per step**
(`DynamicLayer.update`), eager — the pre-allocated StaticCache is opt-in
(`cache_implementation`, default `None`). Conclusion for Go: static max buffers are NOT
the key (replan refuted), but rather **one batched call per decoding step for all 3
beams** — exactly Python's structure (batch = 3 × 1 token/call). → Opt-3.

## Step 2 — Opt-1: logits-only graph + tensor reuse (223b4f8)

`onnx` trim to `decoder_logits.onnx` (only the `logits` output; the 48 unused `present.*`
dropped) + reuse of `enc_mask`/`enc_hidden` tensors per chunk.

**Result: p50 4.153 s (−7%), parity 96% ✓, deterministic ✓.**
Lesson: allocation/bandwidth overhead was NOT the bottleneck — more valuable than the
gain was the falsification finding for the next hypothesis.

## Step 3 — Opt-2: fused O(n) topK-log-softmax (e3b6218)

`topKIndices` full sort + `logSoftmax` double pass replaced by **one scan** (top-6
selection by raw logit — monotone in logprob, no sort, no 250k float64 slice) plus one
LSE pass; `decodeStep` returns raw logits. Tie behavior identical to the previous stable
sort (strict `>`, lower index wins).

**Result (GPU-exclusive, 50-set):**

| Metric | Baseline (c174716, exclusive) | Opt-2 |
|---|---|---|
| **p50** | 4.459 s | **0.543 s** (**8.2×**, −88%) |
| p95 | 9.531 s | 1.340 s |
| mean | 5.102 s | 0.648 s |
| Parity (gate) | 96% | **96.0% ✓** (outliers 23/48 = known tokenizer edges) |
| Determinism (gate) | ✓ | **byte-equal ✓** |
| ORT share of wall | — | 456/543 ms = 84% (Go overhead only ~90 ms now) |

0.5 s milestone reached; intermediate target p50 ≤ 0.2 s still open.

## Step 4 — Cached-path parity CLARIFIED (an additional review point; two logic bugs, not FP)

Re-measurement after Opt-2 confirmed 86% with LARGE deviations (chunk 2: 10 extra
triples) — not near-tie noise. First-divergence diff (`cmp_steps.py`): the cached path
ran **255 rounds** and expanded **`[tp_XX, EOS]` as a live beam** (generation past EOS →
hallucination chains). Two fixes:
1. **Eviction not ported** in the cached-loop rewrite (plain `append` instead of `addHyp`)
   → 43→47/50.
2. **The loop init fed the step-1 top-6 unfiltered into `beams`** (the EOS candidate was
   expanded) → init mirrors loop semantics (EOS→done+eviction, numBeams cap) → **96.0%**.

**Cached now: GATE PASS (96.0%, byte-deterministic, p50 0.583 s)** — no speed advantage
unbatched (per-call dominated), but correct. The reviewer's suspicion of a "real cache
ordering bug" was right (in Go semantics: EOS/beam lifecycle, not KV ordering — that was
correct).

## Step 5 — Opt-3: beam batching [B, L]

One decoder call per round for all 3 beams (B∈{1,3}; encoder tensors batch-materialized)
— exactly Python's generate() structure.

| Metric | Opt-2 | Opt-3 |
|---|---|---|
| p50 | 0.543 s | **0.364 s** (−33%) |
| p95 | 1.340 s | 1.037 s |
| ORT calls/chunk | 68 | **24** |
| ORT ms/chunk | 456 | 269 (11.2 ms/call) |
| Gate | PASS | **PASS** (96%, byte-equal) |

## Step 6 — Opt-4: `logits_last` graph trim

ONNX surgery on `decoder_logits.onnx`: Slice node (axis 1, starts=−1, ends=INT64_MAX)
→ output `logits_last` **[B, 1, 250071]**; validated (max|d| 0.0 vs `logits[:, -1, :]`).
Kills the L-scaled logit download (~48 MB → 3 MB per call at L=16).

| Metric | Opt-3 | Opt-4 |
|---|---|---|
| p50 | 0.364 s | **0.249 s** (−32%) |
| p95 | 1.037 s | 0.582 s |
| ORT ms/chunk | 269 | 171 (**7.1 ms/call**) |
| Gate | PASS | **PASS** (96%, byte-equal) |

**Campaign status:** baseline 4.459 s → 0.249 s = **17.9×**; target ≤ 0.2 s still open
(remaining: ~78 ms Go overhead [72 topK/LSE scans of 250k each] + 171 ms ORT across 24
calls).

## Step 7 — Opt-5a/5b/6: the last levers, measured

- **Opt-5a (parallel beam scans):** the 3 topK/LSE scans per call in parallel (goroutines,
  indexed writers — arithmetic identical). **p50 0.209 s**, gate PASS.
- **Opt-5b (cached+batched):** with_past beam search batched ([B,1] step + batch cache
  [B,16,L,64] incl. row re-parenting on selection). Correct (gate PASS 96%), but
  **p50 0.375 s — WORSE** than no-cache: the with_past graph's 49-tensor interface (48
  individual KV inputs) costs more than the saved prefix computation brings in. Lesson:
  the interface, not the mathematics, is the with_past bottleneck in onnxruntime_go.
- **Opt-6 (IOBinding, the directive "exhaust it first"):** constant inputs bound across
  calls (`CreateIoBinding`/`BindInput`/`RunWithBinding`). **p50 0.208 s ≈ no gain** —
  onnxruntime_go tensors live in HOST memory; ORT still copies host→device per run.
  Measured, not asserted: IOBinding is exhausted in the host-tensor model.

## Step 8 — Opt-7: in-graph LogSoftmax+TopK fusion (END GOAL)

Graph surgery on `decoder_logits_last.onnx`: `LogSoftmax(axis=-1)` + `TopK(K=6)`
appended, outputs now only `ftk_topk_ids/ftk_topk_logps` **[B,1,6]** → per call only
**6×2 values** leave the GPU instead of 3 MB of logits; the 250k host scans disappear
entirely (decodeStepB returns candidates directly). TopK IDs validated exactly against
numpy.

| Metric | Opt-5a | Opt-7 |
|---|---|---|
| p50 | 0.209 s | **0.195 s** ✓ ≤ 0.2 s |
| p95 | 0.496 s | 0.473 s |
| mean | 0.252 s | 0.233 s |
| Gate | PASS | **PASS** (96.0%, byte-equal; outliers 23/48 unchanged) |

## Final state & classification (all GPU-exclusive, n=50)

| | Baseline (c174716) | Opt-2 scan | Opt-3 batch | Opt-4 last | **Opt-7 fused** | Python (full-chunk) |
|---|---|---|---|---|---|---|
| **p50** | 4.459 s | 0.543 s | 0.364 s | 0.249 s | **0.195 s** | 0.141 s |
| p95 | 9.531 s | 1.340 s | 1.037 s | 0.582 s | 0.473 s | 0.314 s |
| Factor | 1× | 8.2× | 12.3× | 17.9× | **22.9×** | (reference) |

- **End-goal threshold p50 ≤ 0.2 s ACHIEVED** (0.195 s) with the gate holding throughout
  (96% triple parity, 2× determinism). Go sits at **1.38× Python** (0.195 vs 0.141 s).
- Remaining structural costs (data-backed): ~24 calls × ~7 ms — per-call fixed costs of
  the host-tensor model (encoder hidden [3,317,1024] ≈ 3.9 MB upload per call; ORT run
  overhead across 3 inputs/2 outputs) + remaining Go (~20 ms: tokenizer/decode/parse).
  Python's lead = end-to-end device residency (KV + logits + topk stay on GPU).
  onnxruntime_go offers no device-tensor API; IOBinding measured without gain. The last
  third (0.195→0.14) would be cgo/C-API device-buffer work — a named epic item, no
  longer pre-research.
- **Epic recommendation (G2 #179):** Go-native with the Opt-7 stack (logits-only trim +
  batch + in-graph topK) is pre-qualified for production: 96% parity, deterministic,
  0.195 s/chunk on a 3090. The sidecar remains the reserve, not the plan.

## Artifacts & commands

- `mrebelgo/analyze_trace.py`, `cmp_steps.py`, `parity_gate.py`, `repeat0.json`;
  `carrier_results/opt2_parity{,_r2}.json`; traces/logs `/tmp/mrebel_trace*.jsonl`.
- Run (carrier): `podman run --rm --device nvidia.com/gpu=all -e CUDA_VISIBLE_DEVICES=0
  -e ORT_CUDA=1 -e ORT_CUDA_DEVICE=0 [-e MRBEL_TRACE=1] … study-mrebel:latest bash -c
  "cd /study/cmd/feasibility/mrebelgo && go build -o /tmp/mg . && /tmp/mg
  /opt/onnxruntime/lib/libonnxruntime.so.1.29.0 /models/mrebel_onnx /models/sample_chunks.json
  /study/cmd/feasibility/mrebelgo/parity_idxs.json /tmp/out.json"`

## Next steps

1. **Cached-path parity (86%) as its own item** (an additional review point): first-divergence
   diff `nocache` vs `cached` steps dump — note: the old 86% measurement still contained
   the sort tie-breaks; re-measure after Opt-2, then diff.
2. **Opt-3: beam batching [3, L]** — one ORT call per decoding step for all 3 beams
   (68 → ~23 calls/chunk; estimate p50 ≈ 0.2 s).
3. If still > 0.2 s after that: check provider options/IOBinding; otherwise an honest
   delimitation.
