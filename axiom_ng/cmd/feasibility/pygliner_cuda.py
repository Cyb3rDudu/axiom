#!/usr/bin/env python
"""GLiNER-ONNX CUDA model-execution proof (carrier Nachzug point 4).

Proves the gliner_multi-v2.1 ONNX export executes on the CUDA EP (via the GPU
onnxruntime shared lib), with logits identical to CPU (engine correctness).
Entity-SET parity was already proven PyTorch<->ONNX in Block 7 (<=1e-5); this
adds the CUDA model-execution column.

Builds valid ONNX inputs via gliner's own `_build_dummy_batch` + `_get_onnx_input_spec`,
runs `logits` on CPU and CUDA, prints max-abs-diff + providers.

Usage: pygliner_cuda.py <gliner_dir> <outdir>
"""
import os
import sys

import numpy as np

gliner_dir, outdir = sys.argv[1], sys.argv[2]
sys.path.insert(0, gliner_dir)

import onnxruntime as ort
from gliner import GLiNER

# gliner for input construction (torch model — CPU inference OK for building).
m = GLiNER.from_pretrained("urchade/gliner_multi-v2.1")
dummy = m._build_dummy_batch()
spec = m._get_onnx_input_spec()
input_names = spec["input_names"]
feed = {n: np.asarray(dummy[n].detach().cpu().numpy()) for n in input_names}
print("input names:", input_names, "shapes:", {n: feed[n].shape for n in input_names})

cpu = ort.InferenceSession(f"{gliner_dir}/model.onnx", providers=["CPUExecutionProvider"])
logits_cpu = cpu.run(["logits"], feed)[0].astype(np.float32)

gpu = ort.InferenceSession(f"{gliner_dir}/model.onnx", providers=["CUDAExecutionProvider","CPUExecutionProvider"])
logits_gpu = gpu.run(["logits"], feed)[0].astype(np.float32)
d = float(np.abs(logits_gpu - logits_cpu).max())
print("GPU providers:", gpu.get_providers())
print("logits shape:", logits_gpu.shape)
print(f"GLiNER-ONNX CUDA-vs-CPU logits max-abs-diff: {d:.3e}")
print(f"CUDA EP active: {'CUDAExecutionProvider' in gpu.get_providers()}")
os.makedirs(outdir, exist_ok=True)
with open(f"{outdir}/gliner_logits_cuda.txt", "w") as f:
    f.write(f"cuda_vs_cpu_max_abs_diff={d:.6e}\nproviders={gpu.get_providers()}\nshape={list(logits_gpu.shape)}\n")
print("wrote", f"{outdir}/gliner_logits_cuda.txt")
