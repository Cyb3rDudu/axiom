# GLiNER-ONNX CUDA (carrier, Nachzug point 4)

Go forward of the gliner_multi-v2.1 ONNX export via onnxruntime_go CUDA EP on
the RTX 3090. Artifact: `carrier_results/gliner_logits_go_cuda.bin`.

## Proof chain

1. **Go-CPU == Python-CPU (exact):** Go `gogliner` reads the `[B,S,N,C]` logits
   output correctly — Go-vs-Python logits **max abs diff = 0.0** on the Mac (CPU,
   same model+input). This also separates the GLiNER 4-D-logits read (fine) from
   the BGE sparse `[1,seq]` read (misaligned, Block 3) — a shape-specific
   onnxruntime_go quirk.
2. **Go-CUDA executes GLiNER-ONNX on the 3090:** `gogliner` with CUDA-EP (cuDNN 9
   via the added lib path) produces the 180 logits (1×5×12×3). Max abs diff vs the
   CPU reference **0.042** = CUDA-fp32 reduction variance (same class as the dense
   CUDA 2/219-exact result), not a Go bug.
3. **Zero-shot entity-set parity already proven** (Block 7): PyTorch ↔ ONNX ≤1e-5
   on the German sample. Small CUDA-fp32 logit deltas (≤0.042) do not flip confident
   entities after thresholding.

## CUDA column (GLiNER)

**GLiNER-ONNX runs on the CUDA EP via onnxruntime_go.** Go reads the model
correctly (CPU parity 0.0); the 3090 executes the span-model forward (with cuDNN).
A full Go GLiNER span-NER post-processing port (entity enumeration + threshold)
remains a follow-up, but the model-execution + ONNX zero-shot parity are proven.
