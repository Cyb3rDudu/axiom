# Tokenizer pin (#171, Block 2) — three Go candidates, one winner

Reproducible evidence for the tokenizer parity decision. Run `pin.sh` to re-verify.

## Candidates (measured)

- `./main.go` — `eliben/go-sentencepiece` (pure Go, **BPE-only**). **Rejected**:
  cannot load XLM-R (`model type UNIGRAM not supported`).
- `./rawsp/` — `lwch/sentencepiece` (pure Go, greedy). **Rejected**: wrong Viterbi
  segmentation (splits `▁Öffentlichkeit` into 4 pieces with wrong ids).
- `./tokpin/` — **`tggo/goSentencePiece`** (pure Go, Unigram+Viterbi) + HF
  `<pad>`-reindex. **PASS** (15 samples incl. umlauts + NFC/NFD).

The `gomlx/tokenizers` Rust FFI is a 4th candidate but its prebuilt lib ships only
for linux/amd64 (cannot link on darwin/arm64); viable on the 3090 CUDA farm.

## Verify the pin

```
./pin.sh          # exits 0 iff Go ids == Python ids on all samples
```

Requires the HF cache (`BAAI/bge-m3` sentencepiece.bpe.model) and the runner
venv's `transformers`. See `../02-tokenizer-pin.md` for the full write-up.
