# Feasibility Study — Block 2 · Tokenizer-Pin (#171)

Status: **PASS** on Apple Silicon (aarch64/MPS, go 1.26.5). Reproducible commands
committed under `axiom_ng/cmd/feasibility/tokenizer/`.

## Why this is the gate

Before ANY Go-vs-Python model comparison, the tokenizer must be pinned: if the Go
side tokenizes differently (segment boundaries, ID space), an embedding mismatch
would be a tokenizer artifact, not a model one. The reference is the HF
`XLMRobertaTokenizerFast` the runner actually uses (`BAAI/bge-m3`, `tokenizer_class
= XLMRobertaTokenizerFast`, SentencePiece inside, `model_max_length=8192`).

## Candidate Go tokenizers — measured, not assumed

| Candidate | Type | XLM-R result |
|---|---|---|
| `eliben/go-sentencepiece` v0.7.0 | pure Go, **BPE only** | **cannot load** XLM-R: `model type UNIGRAM not supported` (XLM-R is a Unigram LM SentencePiece, despite the `sentencepiece.bpe.model` filename) |
| `lwch/sentencepiece` | pure Go, greedy longest-match | **wrong segmentation** (`▁Öffentlichkeit` splits into 4 pieces, ids `[8654,26041,72966,21887,90514]` vs reference `[0,135591,90515,2]`) |
| `gomlx/tokenizers` (Rust FFI) | Rust huggingface/tokenizers via cgo | prebuilt `libgomlx_tokenizers.a` **only for linux/amd64**; fails to link on darwin/arm64 (`#cgo noescape ... no matched C function`) |
| **`tggo/goSentencePiece` v1.1.0** | **pure Go, Unigram + Viterbi** | **✅ matches reference** (see below) |

Research-correcting finding: go-sentencepiece's BPE-only + XLM-R's unigram means the
earlier working assumption ("any SentencePiece port works") is false; `gomlx/tokenizers`
(a Rust FFI) needs a linux/amd64 host or a self-built Rust lib — not a drop-in on the
Mac. `tggo/goSentencePiece` (pure Go Unigram/Viterbi, byte-identical to reference
C++/Python sentencepiece) is the winner.

## The pin (measured)

Tool: `tokenizer/tokpin` (go run), `tokenizer/pin.sh` (assertion harness, 15 samples).

XLM-R wrap reproduced: `BertStylePostProcessor(cls=0, sep=2)` + a deterministic
**HF reindex** — `XLMRobertaTokenizerFast` inserts `<pad>` at 1, so every normal
SentencePiece piece ID is **+1** relative to the raw model id (`<s>`=0, `</s>`=2
unchanged). Verified vocab: `0=<s> 1=<pad> 2=</s> 3=<unk> … 250000=稣 250001=<mask>`.

Sample, Go == Python:

```
input="Öffentlichkeit Straße" → ids=[0, 135591, 90515, 2]   (pure 2 tokens)
input="Marktanteile des Unternehmens" → ids=[0, 34618, 2733, 1340, 224, 70948, 2]
```

**PIN RESULT: PASS over 15 samples.** Covers umlauts (`Ö ä ü ß ü`), mixed case,
technical/"straße-form" words, punctuation, and 4 **NFC/NFD pairs** — the
normalized form produces byte-identical IDs (`#8/#9`, `#10/#11`, `#12/#13`,
`#14/#15`), proving the Go normalizer does the same NFKC the Fast tokenizer does
(the exact "Normalisierungs-Falle" the issue named).

## Dense parity on the pinned IDs (early, Block 3 lead-in)

With the pin in place, the encoder is comparable. Go `onnxruntime_go` ran the
HF-cached BGE-M3 `onnx/model.onnx` on the pinned IDs; Python `BGEM3FlagModel`
(MPS, fp32) on the same text. Max |Δ| ≈ 5e-8, both L2-normalized to 1.0:

```
ref ids [0, 135591, 90515, 2]
py  sentence_embedding [-0.05815725  0.00370091 -0.05278185 -0.01108420 -0.00969718  0.00694390]
go  sentence_embedding [-0.058157224 0.0037008824 -0.052781902 -0.011084199 -0.009697114 0.0069438783]
```

cos ≈ 1.000000. Full Block 3 (≥200 chunks, 2× byte-determinism) is next.

## Artifacts (committed on the study branch)

- `cmd/feasibility/tokenizer/{main.go,rawsp,tokpin,pin.sh,sample corpus}` — reproducible.
- ORT dylib not committed (42 MB, local /tmp); documented in `ortprobe/README.md`.
- Block 2 dependency pins: `tggo/goSentencePiece v1.1.0`.
