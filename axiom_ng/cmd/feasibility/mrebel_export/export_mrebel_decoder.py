#!/usr/bin/env python3
"""
Restpunkt 6 / Ziel 1 — mREBEL Decoder-ONNX-Export (Workaround optimum external-data-cleanup-Bug).

Problem: optimum 2.3.0 (`export onnx`) scheitert beim BART-Decoder an
`os.remove(.../decoder_model.onnx.data)` -> FileNotFoundError (external-data-cleanup quirk),
bevor `decoder_model.onnx` geschrieben wird; zusätzlich kann der torch 2.13-Dynamo-Exporter
nicht mit transformers 4.57 `EncoderDecoderCache` umgehen.

Lösung: Decoder direkt mit dem LEGACY torchscript-Tracer (`dynamo=False`) + torch.onnx.export
tracen, past_key_values im LEGACY-FORMAT übergeben (12 Tupel (self_k,self_v,cross_k,cross_v))
und ausgegeben. Kein optimum-Exportcode im Pfad, externale-Daten-Cleanup tritt gar nicht auf.

Es entstehen zwei Graphen:
  1) decoder_model.onnx  — First-Forward ohne past (erzeugt den initialen KV-Cache + logits).
  2) decoder_with_past_model.onnx — Continue-Forward (ein Token pro Schritt, KV-Cache rein/raus).

Graphform (beide):
  inputs:  decoder_input_ids [1, dec_len], encoder_hidden_states [1, enc_len, d],
           encoder_attention_mask [1, enc_len], (with_past: past_key_i/past_value_i pro Schicht)
  outputs: present_key_values flat (48 tensors: self_k/v + cross_k/v ×12), logits [1, dec_len, vocab]

Decoder hat 12 Schichten, 16 Heads, d_k = d_v = 64 (siehe gemessene Shapes). Der Go-Loop
(mrebelgo) ruft die Graphen über yalue/onnxruntime_go auf.

Aufruf (Carrier, im Container mit torch+transformers+onnx+GPU):
  python3 export_mrebel_decoder.py /models/mrebel_onnx
"""
import os
import sys

import torch
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer

OUT = sys.argv[1] if len(sys.argv) > 1 else "mrebel_onnx"


class DecoderWrapper(torch.nn.Module):
    """Traceable BART decoder (model.decoder + lm_head).

    Forward accepts the past (if with_past) as *flat tensors in legacy-layer order:
      for each layer i: self_k_i, self_v_i, cross_k_i, cross_v_i
    and reconstructs `past_key_values` as a tuple of `(self_k, self_v, cross_k, cross_v)`
    per layer — the format EncoderDecoderCache.from_legacy_cache expects.
    """
    def __init__(self, model, with_past):
        super().__init__()
        self.dec = model.model.decoder
        self.lm = model.lm_head
        self.with_past = with_past
        self.n_layers = model.config.decoder_layers

    def forward(self, decoder_input_ids, encoder_hidden_states, encoder_attention_mask, *past):
        if self.with_past:
            # past: flat tensors, 4 per layer (self_k,self_v,cross_k,cross_v)
            per = [past[i:i + 4] for i in range(0, len(past), 4)]
            legacy = tuple((p[0], p[1], p[2], p[3]) for p in per)
        else:
            legacy = None
        out = self.dec(input_ids=decoder_input_ids,
                       encoder_hidden_states=encoder_hidden_states,
                       encoder_attention_mask=encoder_attention_mask,
                       past_key_values=legacy,
                       use_cache=True)
        logits = self.lm(out[0])
        cache = out[1]  # EncoderDecoderCache with self/cross DynamicCache (transformers 4.57)
        # present tensors: per layer self_k,self_v,cross_k,cross_v (legacy-layer order)
        dk = cache.self_attention_cache.layers
        ek = cache.cross_attention_cache.layers
        flat_present = []
        for i in range(self.n_layers):
            flat_present += [dk[i].keys, dk[i].values, ek[i].keys, ek[i].values]
        return tuple(flat_present), logits


def export_decoders():
    os.makedirs(OUT, exist_ok=True)
    dev = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"[mrebel-export] device={dev}")
    model = AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large",
                                                  torch_dtype=torch.float32).to(dev).eval()
    tok = AutoTokenizer.from_pretrained("Babelscape/mrebel-large")
    cfg = model.config
    n_layers = cfg.decoder_layers
    print(f"[mrebel-export] decoder_layers={n_layers}")

    # encoder_hidden_states + attention for probe (batch 1)
    enc = tok(["Virchow entdeckte 1821 die Zelle und ihre Teilung."], max_length=64,
              padding=True, truncation=True, return_tensors="pt").to(dev)
    with torch.no_grad():
        enc_out = model.model.encoder(input_ids=enc["input_ids"],
                                      attention_mask=enc["attention_mask"])
    enc_hidden = enc_out[0].detach()
    enc_mask = enc["attention_mask"].detach()
    dec_ids = torch.tensor([[tok.convert_tokens_to_ids("tp_XX")]], dtype=torch.long, device=dev)

    past_names = []
    for i in range(n_layers):
        past_names += [f"self_key_{i}", f"self_value_{i}", f"cross_key_{i}", f"cross_value_{i}"]

    # --- 1) decoder_model.onnx (no past) ---
    w = DecoderWrapper(model, False).to(dev).eval()
    with torch.no_grad():
        torch.onnx.export(
            w, (dec_ids, enc_hidden, enc_mask),
            os.path.join(OUT, "decoder_model.onnx"),
            input_names=["decoder_input_ids", "encoder_hidden_states", "encoder_attention_mask"],
            output_names=[("o_" + n) for n in past_names] + ["logits"],
            dynamic_axes={
                "decoder_input_ids": {0: "batch", 1: "decoder_seq"},
                "encoder_hidden_states": {0: "batch", 1: "enc_seq"},
                "encoder_attention_mask": {0: "batch", 1: "enc_seq"},
                "logits": {0: "batch", 1: "decoder_seq"},
            },
            opset_version=14, dynamo=False,
        )
    print("[ok] decoder_model.onnx:", os.path.getsize(os.path.join(OUT, "decoder_model.onnx")), "bytes")

    # --- 2) decoder_with_past_model.onnx (KV-cache) ---
    # real 2-token past for the trace
    dec2_ids = torch.cat([dec_ids, torch.tensor([[tok.convert_tokens_to_ids("entdeckte")]],
                                                dtype=torch.long, device=dev)], dim=1)
    with torch.no_grad():
        dec2 = model.model.decoder(input_ids=dec2_ids,
                                   encoder_hidden_states=enc_hidden,
                                   encoder_attention_mask=enc_mask,
                                   use_cache=True)
        cache = dec2[1]
        dk = cache.self_attention_cache.layers
        ek = cache.cross_attention_cache.layers
        flat_past = []
        for i in range(n_layers):
            flat_past += [dk[i].keys.detach(), dk[i].values.detach(),
                          ek[i].keys.detach(), ek[i].values.detach()]
    last_ids = dec2_ids[:, -1:]
    wp = DecoderWrapper(model, True).to(dev).eval()
    with torch.no_grad():
        torch.onnx.export(
            wp, (last_ids, enc_hidden, enc_mask, *tuple(flat_past)),
            os.path.join(OUT, "decoder_with_past_model.onnx"),
            input_names=["decoder_input_ids", *past_names,
                         "encoder_hidden_states", "encoder_attention_mask"],
            output_names=[("o_" + n) for n in past_names] + ["logits"],
            dynamic_axes={
                "decoder_input_ids": {0: "batch", 1: "decoder_seq"},
                "logits": {0: "batch", 1: "decoder_seq"},
            },
            opset_version=14, dynamo=False,
        )
    print("[ok] decoder_with_past_model.onnx:", os.path.getsize(os.path.join(OUT, "decoder_with_past_model.onnx")), "bytes")


if __name__ == "__main__":
    export_decoders()
