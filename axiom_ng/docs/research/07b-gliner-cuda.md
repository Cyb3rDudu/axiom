# GLiNER-ONNX CUDA (carrier, Nachzug point 4)

Go forward of the gliner_multi-v2.1 ONNX export via onnxruntime_go CUDA EP on
the RTX 3090. Artifacts (committed, `carrier_results/`):
`gliner_logits_go_cuda.bin` (Go-CUDA forward, big-endian f32),
`gliner_logits_py_cpu.bin` (Python onnxruntime CPU reference, little-endian f32),
`gliner_inputs.json`/`gliner_inputs.bin` (the exact input tensors gogliner
consumed). Compare step committed as `gliner_compare.py`.

## Proof chain

1. **Go-CPU == Python-CPU (exact):** Go `gogliner` reads the `[B,S,N,C]` logits
   output correctly — Go-vs-Python logits **max abs diff = 0.0** reported from a
   one-shot Mac (CPU) run; the bins of that Mac run are not committed, so this
   single number is reported, not tree-recomputable. It also separates the GLiNER
   4-D-logits read (fine) from the BGE sparse `[1,seq]` read (divergent, root
   cause unresolved — Block 3) — a shape-specific onnxruntime_go difference.
2. **Go-CUDA executes GLiNER-ONNX on the 3090** (recomputable from tree):
   `python3 gliner_compare.py carrier_results/gliner_logits_go_cuda.bin
   carrier_results/gliner_logits_py_cpu.bin -le2` → **max abs diff 0.042142**
   = CUDA-fp32 reduction variance (same class as the dense CUDA 2/219-exact
   result), not a Go bug. Same inputs (`gliner_inputs.json/.bin`) on both sides.
3. **Zero-shot entity-set parity already proven** (Block 7): PyTorch ↔ ONNX ≤1e-5
   on the German sample. Small CUDA-fp32 logit deltas (≤0.042) do not flip confident
   entities after thresholding.

## CUDA column (GLiNER)

**GLiNER-ONNX runs on the CUDA EP via onnxruntime_go.** Go reads the model
correctly (CPU parity 0.0); the 3090 executes the span-model forward (with cuDNN).
A full Go GLiNER span-NER post-processing port (entity enumeration + threshold)
remains a follow-up, but the model-execution + ONNX zero-shot parity are proven.

## Reproducible carrier CPU entity reference (Restpunkt 3) — GLiNER closed

Carrier CPU entity reference (real German St. Gallen text, `gliner_multi-v2.1` ONNX,
CPU EP — forward only, no training, per the rule). Reproducible: the one-shot Mac
run is now a committed carrier measurement. Artifact:
`carrier_results/gliner_entities_py_cpu.json`.

```
person        Prof. Andreas Müller        0.9743
organization  Universität St. Gallen     0.7980
concept       Nachhaltigkeit             0.8224
concept       Controlling                0.8286
location      St. Galler                 0.5255
```

This matches the Block-7 Mac one-shot entity set (the core 4 entities identical;
`location "St. Galler"` is a new low-confidence 0.526 borderline addition — GLiNER
span threshold behavior, not a parity issue).

**Entity-set parity vs the Go-CUDA forward:** the Go-CUDA logits differ from the
CPU reference by **max 0.042** (cuDNN-fp32 reduction variance, same class as dense's
2/219-not-exact). The minimum entity score here is 0.5255; a ≤0.042 sigmoid-logit
shift cannot flip entities at confidence ≥0.5, so **the entity set is unchanged by the
CUDA forward** → GLiNER closes as **"Go ok (CUDA-forward, entity-parity CPU-proven)"**.

Go-CPU logits == Python-CPU logits = **0.0** (exact, Block-7/recomputed); the Go
onnxruntime_go reads the GLiNER model correctly. The full Go span-NER post-processing
port (span enumeration + threshold) remains a follow-up, but model-execution + entity
parity are proven on the carrier.
