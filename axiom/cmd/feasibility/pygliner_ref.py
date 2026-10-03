#!/usr/bin/env python
"""GLiNER-ONNX Go-vs-Python logits parity harness (carrier).

Harvests the EXACT input tensors gliner feeds to its ONNX session during
predict_entities for a fixed German text + labels, runs the onnx forward, and
saves:
  - gliner_inputs.npz (input_ids, attention_mask, words_mask, text_lengths,
    span_idx, span_mask)
  - gliner_logits_py.cpu bin (big-endian f32 logits flatten) for the Python
    reference.
The Go side (gogliner) does NOT read the npz directly — it reads
  gliner_inputs.json (shapes/dtypes/offsets) + gliner_inputs.bin (raw tensor
  bytes). The npz→(json+bin) split was done ad-hoc and the converter is not
  committed; the converted inputs ARE committed as
  carrier_results/gliner_inputs.{json,bin}, and gogliner writes
  gliner_logits_go.bin (big-endian f32); the committed compare step is
  gliner_compare.py (Go vs Python model-execution parity on GLiNER-ONNX-CUDA).
"""
import argparse
import os
import struct
import sys

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _device import add_device_args, ort_providers, pick_device

p = argparse.ArgumentParser(description="GLiNER-ONNX logits parity harness (carrier)")
p.add_argument("outdir")
add_device_args(p)
args = p.parse_args()
outdir = args.outdir
os.makedirs(outdir, exist_ok=True)
MODEL = "/models/model.onnx"   # gliner onnx on the carrier

TEXT = ("Das St. Galler Management-Modell beschreibt die Führung von Unternehmen. "
        "Prof. Andreas Müller von der Universität St. Gallen forscht zu Nachhaltigkeit, "
        "Controlling und dem St. Galler Führungsansatz.")
LABELS = ["person", "organization", "location", "concept", "book or journal", "research method"]

# Monkeypatch gliner's onnx session to harvest inputs. Load gliner with the onnx model dir.
from gliner import GLiNER

orig_run = None
captured = {}
def harvester(session):
    orig = session.run
    def run(output_names, input_feed=None, **kw):
        if input_feed:
            for k, v in input_feed.items():
                captured[k] = np.asarray(v).copy()
        return orig(output_names, input_feed, **kw)
    session.run = run
    return session

# GLiNER.from_pretrained with an onnx model dir -> model.onnx_session
m = GLiNER.from_pretrained("/models", load_onnx_model=True, onnx_model_file="model.onnx")
harvester(m.model.onnx_session)

entities = m.predict_entities(TEXT, LABELS)
print("entities:", [(e['label'], e['text'], round(e['score'],3)) for e in entities])

# Save captured inputs (must be non-empty)
names = ["input_ids", "attention_mask", "words_mask", "text_lengths", "span_idx", "span_mask"]
np.savez_compressed(f"{outdir}/gliner_inputs.npz", **{k: captured.get(k) for k in names if k in captured})
print("captured input keys:", list(captured.keys()))
for k in names:
    print("  ", k, captured.get(k).shape if k in captured else "MISSING")

# Forward via onnxruntime for a same-input reference independent of gliner
import onnxruntime as ort

dev, _fp16 = pick_device(args.device, no_fp16=True, label="gliner")  # fp16 n/a for ONNX EPs
sess = ort.InferenceSession(MODEL, providers=ort_providers(dev))
feed = {k: captured[k] for k in names if k in captured}
logits = sess.run(['logits'], feed)[0]
print("logits shape:", logits.shape, "providers:", sess.get_providers())
with open(f"{outdir}/gliner_logits_py.bin", "wb") as f:
    f.writelines(struct.pack(">f", float(v)) for v in logits.ravel().astype(np.float32))
print(f"wrote logits_py ({logits.size} floats)")
