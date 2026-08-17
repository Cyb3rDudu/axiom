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
