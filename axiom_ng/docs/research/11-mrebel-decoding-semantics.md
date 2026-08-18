# Restpunkt 6 — mREBEL decoder in Go: decoding semantics (oracle + generation_config.json)

Recorded before any implementation (§9.1). Source: the Python oracle
`axiom_ng_runner/compute_core/relation_extractor.py` (main) + `generation_config.json`
from `~/models/mrebel_onnx/` (carrier).

## Model & tokenizer

- `Babelscape/mrebel-large` — BART-large seq2seq (EN + DE multilingual).
- SentencePiece BPE (`sentencepiece.bpe.model`), round-trip of `<triplet>`/`<subj>`/`<obj>`
  via `tggo/goSentencePiece` proven (07) — these are **plain characters**, not special tokens.

## generation_config.json (real model defaults — BUT the oracle overrides almost everything)

```
bos_token_id = 0
eos_token_id = 2
forced_eos_token_id = 2
pad_token_id = 1
max_length = 200        # default from config
num_beams = 5           # default from config
decoder_start_token_id = 0
```

## Effective generation (the oracle in extract_relations_from_chunks)

All following values come from the oracle's `generate(...)` call and **override** the config:

| Parameter | Value (oracle) | Meaning |
|---|---|---|
| `num_beams` | **3** | Beam width (config says 5 — the oracle forces 3) |
| `num_return_sequences` | = `num_beams` = **3** | 3 return sequences, beam-score-desc |
| `max_length` | **256** | max total decoder length incl. decoder_start |
| `length_penalty` | **0** | **no length normalization** — beam score = Σ log-probs over the sequence |
| `decoder_start_token_id` | = `tokenizer.convert_tokens_to_ids("tp_XX")` | Start token = **tp_XX** (a token, not <bos>!) |
| (implicit) `do_sample` | false | **pure beam search**, no sampling |
| (implicit) `early_stopping` | false | Generate until all 3 beams are complete (EOS) OR max_length reached |
| (implicit) `forced_eos_token_id` | 2 | EOS is forced at the end |

Important:
- **`decoder_start_token_id = tp_XX`, NOT bos.** The decoder starts the sequence with the
  `tp_XX` token (this shapes the `<triplet>` structure in the output). In seq2seq this
  gives `decoder_input_ids = [tp_XX]` as the first decoder step.
- **`length_penalty=0`**: no exp/division on length. Beam score = sum of
  log-probabilities. Order of the 3 return sequences = score **descending**
  (best first).
- `padding=True` in the input encoding (batch of 1, but padded → attention mask needed).
- No `repetition_penalty`, no `diversity_penalty`, no `no_repeat_ngram`.
- `max_length` in `generate` = relative length of the **generation** (decoder), not input.

## Input preprocessing

```
text = chunk["text"]; if not text or len(text.strip()) < 50: skip
input_text = text[:1500]                                   # hard-capped at 1500 characters
input_ids = tokenizer(input_text, max_length=512, padding=True, truncation=True, return_tensors="pt")
```
→ tokenize with max 512, truncation, padding (batch 1). Attention mask = 1 for real tokens.

## Output selection + parser

```
tokens = model.generate(...)         # [3, seq_len]
for seq in tokens:
    decoded = tokenizer.decode(seq, skip_special_tokens=False)
    triples = _parse_mrebel_output(decoded)
# Triples are deduplicated ACROSS ALL 3 sequences:
key   = (triple["head"].lower(), triple["relation"], triple["tail"].lower())
seen.add(key) — first occurrences win, order of extraction is kept.
```

`_parse_mrebel_output` (regex `re.finditer`):
```
decoded = decoded.replace("</s>","").replace("<pad>","")
Regex: r'<triplet>\s*(.+?)\s*<(\w+)>\s*(.+?)\s*<(\w+)>\s*([^<]+)'
groups: head, head_type, tail, tail_type, relation
head/tail/relation each .strip();
only triples with head && tail && relation && len(head)>=2 && len(tail)>=2.
head_type/tail_type mapping: per→PERSON, org→ORGANIZATION, loc→LOCATION,
                              media→WORK, else→CONCEPT  (concept/event/misc→CONCEPT).
```

## Parity target (definition of done)

- **Triple-set equality per chunk** (union over the 3 return sequences, deduplicated)
  ≥ 95% (n ≥ 50).
- **String equality** of the raw generated sequences: **measured and reported**,
  no hidden threshold (identical sparse-outlier methodology).

## Beam-search semantics to replicate (transformers, num_return=num_beams)

1. Start: `beam_scores = -inf`, only `decoder_start_token_id = tp_XX`.
2. Per step: logits → log-softmax over the vocab; for every live hypothesis the
   top-k (k=2*num_beams=6) token candidates; score += logprob.
3. `length_penalty=0` → no length normalization in beam sorting.
4. Done when `num_beams` beams have reached EOS (`early_stopping=False` for
   beam search: it generates until `num_beams` complete hypotheses exist; EOS beams
   are no longer expanded, but the others run to max_length).
5. Output = top-`num_return_sequences` complete beams, score-desc.

Note: since the target's strongest criterion (string equality) is only "measured and reported",
a faithful beam replication suffices for ≥95% triple-set parity; exact byte-string parity
depends on tokenizer.decode (spaces/`▁`) and is measured separately.
