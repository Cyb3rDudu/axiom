#!/usr/bin/env python3
"""
Restpunkt 6 / Ziel 1 — mREBEL Decoder-ONNX-Export (Workaround optimum external-data-cleanup-Bug).

Problem: optimum 2.3.0 (`export onnx`) scheitert beim BART-Decoder an
`os.remove(.../decoder_model.onnx.data)` -> FileNotFoundError (external-data-cleanup quirk),
bevor `decoder_model.onnx` geschrieben wird.

Workaround: Decoder direkt mit torch.onnx.export tracen — kein optimum-Exportcode im
Pfad, also tritt der Cleanup-Bug gar nicht auf. Es werden zwei Graphformen erzeugt:

  1) decoder_model.onnx  — First-Forward ohne past:
     inputs:  decoder_input_ids [1, dec_len], encoder_hidden_states [1, enc_len, d],
              encoder_attention_mask [1, enc_len]
     outputs: logits [1, dec_len, vocab], present_key_values (n_layers*(k,v))

  2) decoder_with_past_model.onnx — Continue-Forward (ein Token pro Schritt):
     inputs:  decoder_input_ids [1, 1], past_key_i/past_value_i für jede Schicht,
              encoder_hidden_states [1, enc_len, d], encoder_attention_mask [1, enc_len]
     outputs: logits [1, 1, vocab], present_key_values (n_layers*(k',v'))

Beide werden mit opset 14 + dynamic axes (batch, seq) exportiert. Der Go-Decoding-Loop
(mrebelgo) ruft sie über yalue/onnxruntime_go auf.

Aufruf (Carrier, im Container mit torch+transformers+onnx+GPU):
  python3 export_mrebel_decoder.py /models/mrebel_onnx
"""
import os
import sys

import torch
from transformers import AutoModelForSeq2SeqLM, AutoTokenizer

OUT = sys.argv[1] if len(sys.argv) > 1 else "mrebel_onnx"
npast = 0  # number of KV layers (computed from config)


class DecoderWrapper(torch.nn.Module):
    """Tracable BART decoder = model.decoder + lm_head, forward(full input_ids[, past])."""
    def __init__(self, model, with_past):
        super().__init__()
        self.dec = model.model.decoder
        self.lm = model.lm_head
        self.with_past = with_past

    def forward(self, decoder_input_ids, encoder_hidden_states, encoder_attention_mask, *past):
        pastkv = tuple((past[i], past[i + 1]) for i in range(0, len(past), 2)) if self.with_past else None
        out = self.dec(input_ids=decoder_input_ids,
                       encoder_hidden_states=encoder_hidden_states,
                       encoder_attention_mask=encoder_attention_mask,
                       past_key_values=pastkv,
                       use_cache=True)
        logits = self.lm(out[0])
        return out[1], logits  # present_key_values (list of (k,v)), lm_logits


def export_decoders():
    os.makedirs(OUT, exist_ok=True)
    dev = "cuda" if torch.cuda.is_available() else "cpu"
    print(f"[mrebel-export] device={dev}")
    model = AutoModelForSeq2SeqLM.from_pretrained("Babelscape/mrebel-large",
                                                  torch_dtype=torch.float32).to(dev).eval()
    tok = AutoTokenizer.from_pretrained("Babelscape/mrebel-large")
    cfg = model.config
    n_layers = cfg.decoder_layers
    global npast
    npast = n_layers

    # --- Probe inputs (batch=1) ---
    enc = tok(["Virchow entdeckte 1821 die Zelle und ihre Teilung."], max_length=64,
              padding=True, truncation=True, return_tensors="pt").to(dev)
    with torch.no_grad():
        enc_out = model.model.encoder(input_ids=enc["input_ids"],
                                      attention_mask=enc["attention_mask"])
    enc_hidden = enc_out[0].detach()
    enc_mask = enc["attention_mask"].detach()
    dec_ids = torch.tensor([[tok.convert_tokens_to_ids("tp_XX")]], dtype=torch.long, device=dev)

    # First-Forward present KV (to derive shapes + give the with_past graph real past)
    with torch.no_grad():
        dec0 = model.model.decoder(input_ids=dec_ids,
                                   encoder_hidden_states=enc_hidden,
                                   encoder_attention_mask=enc_mask,
                                   use_cache=True)
        past_kv = dec0[1]  # list of (k,v)
    past_shapes = [list(t.shape) for kv in past_kv for t in kv]
    print(f"[mrebel-export] decoder_layers={n_layers}, past k/v shapes={past_shapes}")

    past_names = []
    for i in range(n_layers):
        past_names += [f"past_key_{i}", f"past_value_{i}"]

    # --- 1) decoder_model.onnx (no past) ---
    wrapper = DecoderWrapper(model, False).to(dev).eval()
    with torch.no_grad():
        torch.onnx.export(
            wrapper, (dec_ids, enc_hidden, enc_mask),
            os.path.join(OUT, "decoder_model.onnx"),
            input_names=["decoder_input_ids", "encoder_hidden_states", "encoder_attention_mask"],
            output_names=["present_key_values", "logits"],
            dynamic_axes={
                "decoder_input_ids": {0: "batch", 1: "decoder_seq"},
                "encoder_hidden_states": {0: "batch", 1: "enc_seq"},
                "encoder_attention_mask": {0: "batch", 1: "enc_seq"},
                "logits": {0: "batch", 1: "decoder_seq"},
            },
            opset_version=14,
            do_constant_folding=True,
        )
    print("[ok] decoder_model.onnx written:", os.path.getsize(os.path.join(OUT, "decoder_model.onnx")), "bytes")

    # --- 2) decoder_with_past_model.onnx (KV-cache continue) ---
    # real past from a 2-token decoder run
    dec2_ids = torch.cat([dec_ids, torch.tensor([[tok.convert_tokens_to_ids("entdeckte")]],
                                                dtype=torch.long, device=dev)], dim=1)
    with torch.no_grad():
        dec2 = model.model.decoder(input_ids=dec2_ids,
                                   encoder_hidden_states=enc_hidden,
                                   encoder_attention_mask=enc_mask,
                                   use_cache=True)
        real_past = dec2[1]
    flat_past = tuple(t.detach() for kv in real_past for t in kv)
    wp = DecoderWrapper(model, True).to(dev).eval()
    last_ids = dec2_ids[:, -1:]  # continue with last token only
    with torch.no_grad():
        torch.onnx.export(
            wp, (last_ids, enc_hidden, enc_mask, *flat_past),
            os.path.join(OUT, "decoder_with_past_model.onnx"),
            input_names=["decoder_input_ids", *past_names,
                         "encoder_hidden_states", "encoder_attention_mask"],
            output_names=["present_key_values", "logits"],
            dynamic_axes={
                "decoder_input_ids": {0: "batch", 1: "decoder_seq"},
                "logits": {0: "batch", 1: "decoder_seq"},
            },
            opset_version=14,
            do_constant_folding=True,
        )
    print("[ok] decoder_with_past_model.onnx written:", os.path.getsize(os.path.join(OUT, "decoder_with_past_model.onnx")), "bytes")


if __name__ == "__main__":
    export_decoders()
