# Restpunkt 6 — mREBEL-Decoder in Go: Decoding-Semantik (Orakel + generation_config.json)

Festgehalten vor jeder Implementierung (§9.1). Quelle: das Python-Orakel
`axiom_ng_runner/compute_core/relation_extractor.py` (main) + `generation_config.json`
aus `~/models/mrebel_onnx/` (Carrier).

## Modell & Tokenizer

- `Babelscape/mrebel-large` — BART-large Seq2Seq (engl. + dt. Multi-Sprache).
- SentencePiece BPE (`sentencepiece.bpe.model`), round-trip von `<triplet>`/`<subj>`/`<obj>`
  über `tggo/goSentencePiece` bewiesen (07) — das sind **normale Zeichen**, keine Spezial-Tokens.

## generation_config.json (echte Modell-Defaults — ABER das Orakel überschreibt fast alles)

```
bos_token_id = 0
eos_token_id = 2
forced_eos_token_id = 2
pad_token_id = 1
max_length = 200        # Default aus Config
num_beams = 5           # Default aus Config
decoder_start_token_id = 0
```

## Effektive Generation (das Orakel in extract_relations_from_chunks)

Alle folgenden Werte kommen aus dem Orakel-`generate(...)`-Aufruf und **überschreiben** die Config:

| Parameter | Wert (Orakel) | Bedeutung |
|---|---|---|
| `num_beams` | **3** | Beam-Breite (Config sagt 5 — Orakel zwingt 3) |
| `num_return_sequences` | = `num_beams` = **3** | 3 Return-Sequenzen, Beam-Score-desc |
| `max_length` | **256** | max. Gesamt-Decoder-Länge inkl. decoder_start |
| `length_penalty` | **0** | **keine Längen-Normalisierung** — Beam-Score = Σ log-probs über die Sequenz |
| `decoder_start_token_id` | = `tokenizer.convert_tokens_to_ids("tp_XX")` | Start-Token = **tp_XX** (Token, kein <bos>!) |
| (implizit) `do_sample` | false | **reiner Beam-Search**, kein Sampling |
| (implizit) `early_stopping` | false | Generiere, bis alle 3 Beams komplett (EOS) ODER max_length erreicht |
| (implizit) `forced_eos_token_id` | 2 | EOS wird am Ende erzwungen |

Wichtig:
- **`decoder_start_token_id = tp_XX`, NICHT bos.** Der Decoder beginnt die Sequenz mit dem
  `tp_XX`-Token (das prägt das `<triplet>`-Gefüge im Output). In seq2seq ergibt das
  `decoder_input_ids = [tp_XX]` als ersten Decoder-Schritt.
- **`length_penalty=0`**: kein Exp/Division auf die Länge. Beam-Score = Summe der
  Log-Wahrscheinlichkeiten. Reihenfolge der 3 Return-Sequenzen = Score **absteigend**
  (beste zuerst).
- `padding=True` im Input-Encoding (Batch von 1, aber gepaddet → Attention-Mask nötig).
- Kein `repetition_penalty`, kein `diversity_penalty`, kein `no_repeat_ngram`.
- `max_length` im `generate` = rel. Länge der **Generation** (Decoder), nicht Input.

## Input-Preprocessing

```
text = chunk["text"]; if not text or len(text.strip()) < 50: skip
input_text = text[:1500]                                   # hart auf 1500 Zeichen gekappt
input_ids = tokenizer(input_text, max_length=512, padding=True, truncation=True, return_tensors="pt")
```
→ tokenisieren mit max 512, truncation, padding (Batch 1). Attention-Mask = 1 für echte Toks.

## Output-Auswahl + Parser

```
tokens = model.generate(...)         # [3, seq_len]
for seq in tokens:
    decoded = tokenizer.decode(seq, skip_special_tokens=False)
    triples = _parse_mrebel_output(decoded)
# Triples werden ÜBER ALLE 3 Sequenzen dedupliziert:
key   = (triple["head"].lower(), triple["relation"], triple["tail"].lower())
seen.add(key) — erste Vorkommen gewinnen, Reihenfolge der Extraktion bleibt.
```

`_parse_mrebel_output` (Regex `re.finditer`):
```
decoded = decoded.replace("</s>","").replace("<pad>","")
Regex: r'<triplet>\s*(.+?)\s*<(\w+)>\s*(.+?)\s*<(\w+)>\s*([^<]+)'
groups: head, head_type, tail, tail_type, relation
head/tail/relation jeweils .strip();
nur Triples mit head && tail && relation && len(head)>=2 && len(tail)>=2.
head_type/tail_type mapping: per→PERSON, org→ORGANIZATION, loc→LOCATION,
                              media→WORK, else→CONCEPT  (concept/event/misc→CONCEPT).
```

## Paritäts-Ziel (Definition of Done)

- **Triple-Set-Gleichheit pro Chunk** (Union über die 3 Return-Seq, dedupliziert) ≥ 95 % (n ≥ 50).
- **String-Gleichheit** der rohen generierten Sequenzen: **gemessen und berichtet**,
  keine versteckte Schwelle (identische Sparse-Outlier-Methodik).

## Zu replizierende Beam-Search-Semantik (transformers, num_return=num_beams)

1. Start: `beam_scores = -inf`, nur `decoder_start_token_id = tp_XX`.
2. Pro Schritt: logits → log-softmax über Vocab; für jede lebende Hypothesis die
   top-k (k=2*num_beams=6) Token-Kandidaten; Score += logprob.
3. `length_penalty=0` → keine Längen-Normalisierung bei der Beamsortierung.
4. Fertig, wenn `num_beams` Beams EOS erreicht haben (`early_stopping=False` bei
   Beam-Search: es generiert bis `num_beams` komplette Hypothesen vorliegen; EOS-Beams
   werden nicht mehr expandiert, aber die anderen laufen bis max_length).
5. Output = top-`num_return_sequences` komplette Beams, Score-desc.

Hinweis: Da das Ziel starkeste Kriterium (String-Gleichheit) "gemessen und berichtet" ist,
reicht für ≥95 % Triple-Set-Parität eine getreue Beam-Replikation; exakte byte-String-Parität
hängt an tokenizer.decode (spaces/`▁`) und wird separat gemessen.
