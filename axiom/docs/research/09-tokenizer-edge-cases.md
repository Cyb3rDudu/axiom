# Tokenizer edge-cases concretized (Restpunkt 4) — Go vs Python outputs + impact

Concrete Go (`tggo/goSentencePiece` + HF reindex) vs Python (`XLMRobertaTokenizerFast`)
outputs for the cited cases, and their impact on each parity metric. Tokenizer parsing
only (no model-forward) — allowed under the all-ML-on-GPU rule.

## Case-by-case Go vs Python

| Input | Python | Go | Divergence? |
|---|---|---|---|
| `---- --------` | `▁----, ▁--, ------` | `▁----, ▁--, ------` | **No — identical segmentation** |
| `---` | `▁---` | `▁---` | No |
| `contin` | `▁contin` (102548) | `▁contin` (102547+1) | **No — identical** (the earlier chunk-66 label was misattributed) |
| `Müller` (ü U+00FC) | `▁Müller` (138095) | `▁Müller` (138094+1) | No (NFD/NFC fine) |
| `–` (en-dash U+2013) | normal piece | normal piece | No |
| `Ȭ` (U+022C) | `▁, <unk>` (id 3) | `▁, <Ȭ byte-fallback>` | **YES — real:** Python masks to `<unk>`, Go emits a byte-fallback piece |

## Root cause of the sparse rare-char divergence

The real rare-char cases are **non-Latin codepoints** that Python's tokenizer maps to
`<unk>` (id 3) but Go's byte-fallback emits as a byte-piece:

- **Chunk 195** (overlap 0.810, cos 0.944): contains Syriac/Arabic-extended
  `݊ ݇ ݒ` (U+074A/U+0747/U+0752), Ethiopic `ሻ` (U+123B), N'Ko `ߜ` (U+07DC) + en-dash.
- **Chunk 48** (overlap 0.707, cos 0.941): rare-`Ȭ`-class char → Go `<s>`-vs-Py-`<unk>`
  offset (the `id 0 → 3` remap gap).

Hyphen-runs and `contin` **do not diverge** (corrected: my earlier Block-3 attribution
was wrong; the segmenters agree). The divergence is exclusively the rare-non-Latin
`<unk>`/byte-fallback difference.

## Impact per parity metric

| Metric | Robust to these rare-char cases? | Evidence |
|---|---|---|
| **Dense** (cosine) | **Yes** — mean-pool over 1024 dims averages out a few rare tokens | dense avg 0.9996, 217/219 ≥0.999 (chunk 195/48 rare chars don't drop dense below 0.999) |
| **Rerank** (Spearman) | **Yes** — cross-encoder, pair-wise, robust to isolated tokens | Spearman 1.0000 (corrected pair form) |
| **Sparse** (overlap/cos) | **Sensitive** — exact token-set + weight parity, a rare-char piece can be a stdlib weight-bearing token | chunk 195/48: overlap 0.81/0.71, cos 0.944/0.941 (the only real sparse outliers); 217/219 ≥0.98 |

## Fix candidate (for a Go-runner follow-up)

Go needs the `id 0 → 3` remap for Byte-fallback/masked unknown chars (`Ȭ`-class and
non-Latin surrogates): treat a byte-fallback piece that Python masks to `<unk>` as
`<unk>` (id 3), not the raw byte or `<s>`. This closes the 2-3 sparse outliers.
Numerically irrelevant for dense/rerank (already robust).
