# Runner-Feasibility study output (#171)

Decision document and the per-block proof trail. Every number is reproducible
via `axiom_ng/cmd/feasibility/…` (committed).

- **`go-runner-feasibility.md`** — the decision (component table + CUDA column +
  migration path + determinism + research-claims ledger). Start here.
- `01-contract-inventory.md` — Block 1: endpoints ↔ app.py, device matrix.
- `02-tokenizer-pin.md` — Block 2: Go vs Python token IDs, umlauts + NFC/NFD PASS.
- `03-dense-parity.md` — Block 3: dense ≥0.999 + rerank 0.978 + sparse algorithm.
- `04-xberg.md` — Block 4: Xberg Go binding, digital vs scan, locator gap.
- `05-r7-e2e.md` — Block 5: Mini-Go-runner pipeline, gold delta DB-sync block.
- `07-gliner-mrebel.md` — Block 7: GLiNER-ONNX parity ≤1e-5, mREBEL sidecar.

## Status (issue #171)

All blocks interim-posted. Branch `research/go-runner-feasibility` (from main
`d9729c5`); no code changes to `axiom_ng` product; no merge to main.
