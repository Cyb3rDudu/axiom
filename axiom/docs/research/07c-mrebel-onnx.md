# mREBEL-ONNX (carrier, Nachzug point 7) — export clarification

Research-corrected facts about ONNX-exporting `Babelscape/mrebel-large` (BART-large
Seq2Seq), measured on the carrier. **Artifacts are NOT committed** (per the issue:
only facts reported).

## 1. No pre-built mREBEL ONNX on the HF hub

HF API for `Babelscape/mrebel-large` (10 files) lists **no `.onnx`** sibling — no
ready-made export to reuse. A Go mREBEL must export it itself (or sidecar it).

## 2. optimum-cli export test (carrier, GPU)

`optimum-cli export onnx --model Babelscape/mrebel-large --task text2text-generation`:

- **Encoder exports cleanly**: `encoder_model.onnx` (880 KB) + `encoder_model.onnx_data`
  (1.63 GB) written to `/models/mrebel_onnx/`.
- **Decoder export trips an optimum external-data cleanup bug**: `os.remove(.../decoder_model.onnx.data)`
  → `FileNotFoundError`, aborting before the decoder model is written.

So the **encoder is ONNX-exportable**, but the **autoregressive decoder loop**
(attention cache, greedy/beam decoding) — the true hard part — is not cleanly
exportable through this optimum path and must be reimplemented in Go if native.

## 3. Tokenizer round-trip `<triplet>/<subj>/<obj>`

The mREBEL output/prompt markers (`<triplet>`, `<concept>`, …) are **plain
characters in the decoded text** (the runner's `_parse_mrebel_output` regex-parses
them — see `axiom_ng_runner/compute_core/relation_extractor.py`), not special
tokenizer vocabulary. `tggo/goSentencePiece` on the shared XLM-R SentencePiece
vocab round-trips them faithfully (measured on the Mac with `tokpin`):
`<triplet>` → `▁< / trip / let / >`; `<concept>` → `▁< / con / cept / >` — the
angle brackets + letters + word boundaries survive encode/decode losslessly, so the
Go tokenizer reproduces the text structure the regex parser needs.

## 4. Report correction (Option-1)

The decision doc's Option-1 (sidecar) stands: mREBEL is confirmed as the **hard
ONNX item** — the encoder is exportable, but the **decoder loop** (Seq2Seq
autoregressive) is the fragile part that no clean optimum export produces. Keep
mREBEL as a Python sidecar (option 1); do not pursue the Go decode loop (option 2).
The Go tokenizer round-trip is proven, so a sidecar boundary needs no additional
tokenizer work.
