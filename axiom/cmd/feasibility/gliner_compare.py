#!/usr/bin/env python
"""GLiNER logits compare — the previously ad-hoc step, committed (#171).

Compares two logits dumps and reports max-abs-diff (Go-vs-Python model-execution
parity for the GLiNER-ONNX forward). Byte order per writer:
  - gogliner writes big-endian f32 (encoding/binary.BigEndian)
  - the Python reference (numpy tofile) writes little-endian f32
Pass -le for either file whose writer used numpy-native byte order.

Recomputes the 07b-gliner-cuda.md number from the committed carrier artifacts:
  gliner_compare.py carrier_results/gliner_logits_go_cuda.bin \
                    carrier_results/gliner_logits_py_cpu.bin -le2
  -> max abs diff 0.042142 (CUDA-fp32 vs CPU reference, same inputs)

Usage: gliner_compare.py <a.bin> <b.bin> [-le1] [-le2]
"""
import struct
import sys

args = sys.argv[1:]
if len(args) < 2:
    sys.exit(__doc__)
le1 = "-le1" in args
le2 = "-le2" in args
paths = [a for a in args if not a.startswith("-")]
a_raw, b_raw = open(paths[0], "rb").read(), open(paths[1], "rb").read()
if len(a_raw) != len(b_raw) or len(a_raw) % 4:
    sys.exit(f"size mismatch or not f32-aligned: {len(a_raw)} vs {len(b_raw)}")
fmt = lambda n, le: f"{'<' if le else '>'}{n}f"
a = struct.unpack(fmt(len(a_raw) // 4, le1), a_raw)
b = struct.unpack(fmt(len(b_raw) // 4, le2), b_raw)
d = max(abs(x - y) for x, y in zip(a, b))
print(f"n={len(a)} max_abs_diff={d:.6f}")
