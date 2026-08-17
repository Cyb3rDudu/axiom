#!/usr/bin/env python3
"""
Restpunkt 6 / Ziel 1 — mREBEL BART decoder ONNX via transformers LEGACY exporter.

optimum-cli `export onnx` fails (subcommand not registered in optimum 2.3 + the
external-data cleanup FileNotFoundError); the manual torch.onnx trace produced
corrupt logits. The transformers LEGACY `transformers.onnx` exporter is the
battle-tested path that emits the canonical BART seq2seq graphs:

  encoder_model.onnx
  decoder_model.onnx          (first forward, no past)
  decoder_with_past_model.onnx (autoregressive step, KV-cache)
  decoder_model_merged.onnx    (optional merged)

We generate them via `transformers.onnx.export` and then ALSO dump the exact
input/output signatures to a JSON the Go decoder can build against.

Aufruf (Carrier, study-mrebel container with torch+transformers+onnx+GPU):
  python3 export_mrebel_transformers.py /models/mrebel_onnx
"""
import json
import os
import sys

import torch
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer


def gen(model_id, out_dir, opset=14):
    os.makedirs(out_dir, exist_ok=True)
    dev = "cuda" if torch.cuda.is_available() else "cpu"
    model = AutoModelForSeq2SeqLM.from_pretrained(model_id, torch_dtype=torch.float32).to(dev).eval()
    tok = AutoTokenizer.from_pretrained(model_id)
    # default pt seq2seq config for BART
    from transformers.onnx.features import FeaturesManager
    model_kind, model_onnx_config = FeaturesManager.check_supported_model_or_raise(model, "pt")
    onnx_config = model_onnx_config(model.config, has_dynamic_axes=True)
    # generate a tiny dummy batch for shape inference
    dummy = tok(["Virchow entdeckte die Zelle."], return_tensors="pt").to(dev)
    from transformers.onnx import export as t_export
    inputs, outputs = t_export(
        preprocessor=tok,
        model=model,
        config=onnx_config,
        opset=opset,
        output=os.fspath(out_dir),
        device=dev,
    )
    print("exported models:", inputs, "->", outputs)
    sig = header(out_dir)
    print(json.dumps(sig, indent=2))
    return sig


def header(out_dir):
    import onnxruntime as ort
    res = {}
    for f in ["encoder_model.onnx", "decoder_model.onnx",
              "decoder_with_past_model.onnx", "decoder_model_merged.onnx"]:
        p = os.path.join(out_dir, f)
        if not os.path.exists(p):
            continue
        sess = ort.InferenceSession(p, providers=["CPUExecutionProvider"])
        res[f] = {
            "size": os.path.getsize(p),
            "inputs": [{"name": i.name, "shape": i.shape, "type": i.type}
                       for i in sess.get_inputs()],
            "outputs": [{"name": o.name, "shape": o.shape, "type": o.type}
                        for o in sess.get_outputs()],
        }
    return res


if __name__ == "__main__":
    out = sys.argv[1] if len(sys.argv) > 1 else "mrebel_onnx"
    gen("Babelscape/mrebel-large", out)
