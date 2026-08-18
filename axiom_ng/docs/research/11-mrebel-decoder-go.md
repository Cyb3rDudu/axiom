# Restpunkt 6 — mREBEL decoder natively in Go: decoder ONNX, beam-search loop, parity

**Conclusion up front:** the mREBEL line in the decision document switches from
"sidecar (option 1)" to **"Go-native possible"** — the complete seq2seq decoding stack
(encoder + autoregressive beam-search decoder + triple parser) runs natively in Go on the
carrier (3090), with **96% chunk-level triple-set parity** against the Python oracle
(n=50, threshold ≥95%). Pitfalls, exact numbers, and the honest performance discussion
below.

Branch `research/go-runner-feasibility`, worktree `axiom-research`. All artifacts
committed. Model binaries NOT committed (rule). Test DB untouched (no access needed —
pure model study on `sample_chunks.json`).

---

## 1. Goal 1 — Decoder ONNX export (FIX of the optimum bug)

**Finding:** optimum 2.3.0 fails on the BART decoder at `os.remove(decoder_model.onnx.data)`
→ `FileNotFoundError` (external-data cleanup bug) — the blocker documented in 07c.
Additionally the `onnx` subcommand is not registered in optimum 2.3.0; and `torch.onnx`
(delegates to the Dynamo exporter in torch 2.13) fails on transformers 4.57
`EncoderDecoderCache` (not pytree-registered).

**Solution (committed reproducibly):**
- Dedicated export image `Containerfile.mrebel-export` with a **pinned legacy stack**:
  `optimum==1.21.0` + `transformers==4.42.4` (optimum 1.21 needs `<4.43`) + `torch 2.5.1`
  (pytorch/pytorch base) + `onnx` + `onnxruntime`.
- `optimum-cli export onnx --model Babelscape/mrebel-large --task text2text-generation --no-post-process`
  → `encoder_model.onnx` + `decoder_model.onnx` (without present outputs, correct).
- `optimum-cli export onnx --model Babelscape/mrebel-large --task text2text-generation-with-past --no-post-process`
  → `decoder_with_past_model.onnx` (KV-cache variant) — **validation: present KV matches
  the reference (2,16,17,64), logits max-diff 4.6e-5**.
- `--no-post-process` only skips the decoder_model_merged step (fails at serialize) and
  keeps the individual graphs.

**Artifacts on the carrier (`~/models/mrebel_onnx/`):**
| File | Size |
|---|---|
| `encoder_model.onnx` (+data) | 1.63 GB |
| `decoder_model.onnx` (+data) | 679 KB / 2.86 GB |
| `decoder_with_past_model.onnx` (+data) | 579 KB / 2.76 GB |

**Correctness proof of the graphs (ORT vs torch, 3090):**
- `decoder_model.onnx`: argmax identical at lengths 1–4 (`<triplet>`,`Z`,`Z`,`part`),
  max|d| 1.6e-3 (L=1) to 2.4e-1 (L=4, FP drift from the missing KV cache in the reference
  comparison).
- `decoder_with_past_model.onnx`: 2-step loop (step1 + with_past) logits max|d| 2.8e-3 vs
  torch full forward — **the KV-cache loop is numerically correct**.

**Export scripts committed:** `mrebel_export/export_decoder_onnx.sh` (recipe),
`Containerfile.mrebel-export`, `mrebel_export/export_mrebel_decoder.py` (discarded
manual trace — documents why: Dynamo exporter failure + corrupt logits).

## 2. Goal 2 — Go decoding loop (`cmd/feasibility/mrebelgo/`)

Structure (own go.mod, like the other PoCs): `yalue/onnxruntime_go` +
`tggo/goSentencePiece`.

- **Encoder:** `encoder_model.onnx` (input_ids, attention_mask → last_hidden_state).
  Go encoder output == torch encoder output: **cosine 1.0, max-abs-diff 0.0**
  (byte-identical).
- **Decoder (standard path):** `decoder_model.onnx` per step with full re-encoding
  (the task's "full re-encoding per step" fallback, §4.2) — every hypothesis has its own
  growing token sequence, logits at the last token.
- **Beam search:** width 3, return 3, max_length 256, length_penalty 0, do_sample false,
  decoder_start `tp_XX` (250058). **BeamHypotheses semantics replicated** (keep the best
  numBeams finished, evict worse ones; stop via `worst_finished >= best_open`).
- **KV-cache path (experimental, `MRBEL_CACHE=1`):** `decoder_with_past_model.onnx` with
  cache threading (step1 present.encoder = constant encoder KV; stepN re-emits only the
  decoder KV). Runs, but **no speed gain** in the harness (see §4) and more FP-sensitive.
- **2× determinism:** byte-equal (sha256 `1934fcde…`), both runs identical.

## 3. Goals 3+4 — Parser port + parity (n=50)

**Parser** (`decode.go`): `_parse_mrebel_output` regex ported 1:1 to Go, type mapping
(per→PERSON, org→ORGANIZATION, loc→LOCATION, media→WORK, else CONCEPT), dedup first-seen
across the 3 beams. **Unit test green against 6 real Python fixtures**
(`parser_fixtures.json`).

**Parity measurement (50 chunks, sample_chunks.json, carrier 3090, same-device):**
| Metric | Result |
|---|---|
| **Chunk-level triple-set equality** (union over 3 beams, deduplicated) | **48/50 = 96.0%** (threshold ≥95% ✓) |
| String equality (raw) | 0/50 — Python appends `</s><pad>` padding; Go returns bare sequences |
| String equality (normalized: `</s>`/`<pad>` stripped, whitespace collapsed) | **47/50 = 94%** |
| Total triples | Go 137 vs Python 136 |

**Divergence characterization (identical to the sparse-outlier methodology):**
- Chunk 23: Go generates one extra triple ("constant mix subclass of musterportfolio") —
  Python does not have it (beam FP ordering at nearly equal scores).
- Chunk 48: token-boundary divergence on a rare compound (Python splits
  "datenquel"/"daten" instead of "datenquelle") — **the known rare-non-Latin/compound
  boundary-token case** that also drove the sparse outliers (chunks 48/195).
- 1 further string-only diff without triple impact.

**Critical pitfalls (documented in the report + code):**
1. **Byte- vs. rune truncation:** Python `text[:1500]` cuts characters, Go `text[:1500]`
   cut bytes → with umlauts 21 characters lost → completely different encoder input →
   garbage generation. Fix: `truncateRunes`.
2. **HF vocab offset:** decoder output ids are HF-space = raw-sentencepiece + 1; Go decode
   must compute `id-1` (otherwise id 130629 decodes as Oriya script instead of
   "Verantwortung").
3. **SPM run leading space:** `tggo` decode strips a run's leading `▁` space; after
   special tokens it must be re-added (HF spacing: `Köpfen <concept>`).
4. **Beam eviction:** a naive "top-3 finished" selection keeps `[tp_XX, eos]`
   (score −8.78); transformers' `BeamHypotheses` evicts it as soon as 3 real beams are
   finished (−3.67, −5.64, −6.13). The stop condition
   `worst_finished >= best_open` is mandatory.

## 4. Goal 5 — Performance on the 3090 (honest)

| Path | p50 per chunk | p95 | mean |
|---|---|---|---|
| **Go no-cache beam** (standard) | **4.45 s** | 9.53 s | 5.10 s |
| Go cached (with_past, `MRBEL_CACHE=1`) | 4.38 s | 8.76 s | 5.63 s |
| **Python oracle** (torch generate, with KV cache) | **0.14 s** | 0.31 s | 0.16 s |

Findings:
- The **no-cache path** is ~30× slower than Python (O(L²) re-encoding: ~136 decoder
  forwards per beam vs. ~16 KV-cache steps).
- The **KV-cache path in Go does NOT get faster** — per-call costs dominate:
  (a) onnxruntime_go allocates ~50 input/25 output tensors via cgo per step;
  (b) the with_past graphs have dynamic shapes → ORT re-plans per step.
  Together both eat the algorithmic gain. A production runner would have to reuse
  tensors + use shape-cached sessions to approach Python.
- The KV-cache path is also more FP-sensitive (86% vs 96% — score accumulation over
  cache concatenation vs. recompute; same kind of cuDNN scatter as with GLiNER).

**Measurement for the epic:** Go mREBEL is *functionally* at parity (96% triple set),
but latency is the price of the no-cache path; the cache-path optimization (tensor
reuse, shape caching) is the named epic cost item. For the query paths (dense/rerank)
this is irrelevant — they are already CUDA-parity and fast.

## 5. Definition of done — status

- [x] Decoder ONNX files on the carrier (sizes above; export script committed,
  reproducible)
- [x] `mrebelgo` builds (own go.mod), runs 2× byte-equal
- [x] Parser unit test green against 6 committed Python fixtures
- [x] Parity measurement n=50: **triple set 96.0% ≥ 95%** (number + artifact committed),
  string measured and reported
- [x] Latencies p50/p95 vs Python on the same 3090 in the report
- [x] `11-mrebel-decoder-go.md` exists; decision-document line updated
- [x] No model binaries committed; test DB unchanged; no merge

**Artifacts:** `carrier_results/go_mrebel_parity.json` (+r2), `go_mrebel_cache.json`,
`mrebel_ref_50.json` (Python oracle), `cache_timings2.txt`, `go_timings_r2.txt`,
`parser_fixtures.json`.

**Reproducible commands (carrier):**
```
# Export (one-time, image is committed):
podman run --rm -e HF_HOME=/hf -v ~/.cache/huggingface:/hf:rw -v ~/models:/models:rw \
  -v $PWD/axiom_ng/cmd/feasibility/mrebel_export/export_decoder_onnx.sh:/run.sh:ro \
  localhost/study-mrebel-export:latest bash /run.sh

# Go parity (50 chunks):
podman run --rm --device nvidia.com/gpu=all -e CUDA_VISIBLE_DEVICES=0 -e ORT_CUDA=1 -e ORT_CUDA_DEVICE=0 \
  -v ~/models:/models:ro localhost/study-mrebel:latest bash -c \
  "cd /study/cmd/feasibility/mrebelgo && go build -o /tmp/mg . && /tmp/mg \
   /opt/onnxruntime/lib/libonnxruntime.so.1.29.0 /models/mrebel_onnx /models/sample_chunks.json \
   /study/cmd/feasibility/mrebelgo/parity_idxs.json /tmp/go_mrebel_parity.json"
```

**Decision-document update:** `go-runner-feasibility.md` — mREBEL line from
"sidecar (option 1)" to "**Go-native possible** (decoder ONNX exported + validated;
Go beam 96% triple-set parity; parser 1:1; latency 4.45 s vs 0.14 s Python — cache
optimization = epic item)"; conclusion line adjusted accordingly.
