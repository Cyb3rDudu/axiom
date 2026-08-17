# Feasibility Study: Go-native Runner for the Processing Pipeline

Decisive document (#171). All numbers are project-owned measurements (this study,
macOS arm64 / MPS + CPU ONNX; CUDA verified on the 3090 farm via
`EXTERNAL_RUNNER_DEPLOYMENT.md`). Every number below has a committed,
reproducible command (`axiom_ng/cmd/feasibility/…`). Determinism precondition
held per component: **each engine PoC (godense, gorerank, tokenizer) ran twice;
byte-equality of own outputs was a precondition** for the parity numbers; the
mini-runner inherits those engines and has no own 2× run.

## Executive summary

A Go-native flip of the Python runner is **feasible for the query side and the
algorithmic components, and for the ML encoders via ONNX** — but **not** for the
ingest-side PDF→Markdown of scanned books (Markers Xberg-OCR) and **not** for a
raw BART-ONNX mREBEL. The decisive constraint is the **CUDA column**: GoMLX is
Apple-only; the 3090 farm needs the **onnxruntime CUDA execution provider** (and,
for GLiNER, the GLiNER own ONNX export). All query-path encoders are proven at
parity on CPU through `onnxruntime_go`; CUDA is a drop-through (same ONNX files).

## The role model (Mac vs 3090-farm)

| Role | Device | What the Go runner replaces / keeps |
|---|---|---|
| Query-Runner (Mac, always-on) | MPS/CPU | Dense + rerank via onnxruntime_go — **proven** (dense cosine ≥0.999; rerank Spearman 0.978, re-run pending after pair-form fix) |
| Ingest-Runner (3090 farm, primary) | **CUDA** | Dense/sparse/rerank/GLiNER via ONNX CUDA-EP; PDF→MD via Xberg/Marker sidecar; mREBEL sidecar |
| Ingest-Fallback (Mac) | CPU/MPS | same query-side engines; scanned-PDF OCR only in fallback |

Mac is the **query** compute + fallback; the 3090s are the **ingest** workhorses.
Apple-only parity (GoMLX) does not earn a CUDA column — documented, not hidden.

## Component decision table

| Component | Go/No-Go/Hybrid | Proven device (this study) | CUDA column | Evidence |
|---|---|---|---|---|
| Contract API surface (§5/§7a) | **Go: yes** | CPU | n/a | Block 1: all 10 endpoints match app.py |
| EPUB→CFI | **Go: yes** | CPU (algorithmic) | n/a | Block 1/4: pure HTMLParser walk → Go `net/html` |
| Chunking + locator assembly | **Go: yes** | CPU (algorithmic) | n/a | Block 1: contract §11 mapping |
| Dense BGE-M3 (query + passage) | **Go: yes** (ONNX) | **CUDA (RTX 3090, same-device)** | **onnxruntime CUDA-EP — PROVEN** | Block 2/3 + Nachzug: cosine avg **0.999639** on 219 chunks, **both Go-CUDA-EP and Py-torch-CUDA on the same 3090**; 217/219 ≥0.999; 2× byte-equal |
| Sparse BGE-M3 | **Go: yes (algorithm proven)** — Go harness divergence, likely truncation | CPU | CUDA (same model) | Block 3: formula overlap **1.0 / cos 1.0** in Python (chunks 0 and 14 spot-check); Go divergence reframed — primary suspicion is gosparse 512-truncation vs reference 8192 (same class as the dense bug), binding hypothesis secondary until a matched re-run |
| Rerank bge-reranker-v2-m3 | **Go: yes** (ONNX) | CPU (→CUDA) | **onnxruntime CUDA-EP** | Block 3/5: Spearman **0.978** (≥0.95) — *measured pre-pair-form-fix; re-run pending* (see 03); E2E `+rerank` correct |
| GLiNER zero-shot NER | **Go: inferred** (ONNX export proven in Python; Go onnxruntime_go run pending) | CPU (→CUDA) | **GLiNER own ONNX + CUDA-EP** | Block 7: **parity ≤1e-5** PyTorch↔ONNX in Python (entity set identical); Go run still to do |
| mREBEL relationships | **No-Go native** → **Hybrid/sidecar** | Python service (farm) | see Sidecar | Block 7: BART-ONNX decode loop rejected |
| PDF→Markdown (digital) | **Go: yes** (Xberg binding) | CPU | CUDA for OCR models | Block 4: digital PDF 5 pages extracted, umlauts OK |
| PDF→Markdown (scans) | **No-Go native** → Xberg/Marker sidecar + OCR | farm (CUDA OCR) | yes (via sidecar) | Block 4: scanned book → empty without OCR; Marker 1895 s |
| R7 end-to-end (Mini runner) | **Go: yes (pipeline)** | CPU | CUDA for ingest | Block 5: `/v1/embed`+`/v1/rerank` swapped into bench, correct |

## Determinism (every Go PoC ran twice)

| PoC | run1==run2 | verified |
|---|---|---|
| dense 219 chunks | **byte-equal (897,024 B)** | Block 3 |
| rerank 75 pairs | deterministic (exit 3 on mismatch) | Block 3 |
| mini-runner embed/rerank | inherited — same engines as godense/gorerank; own 2× run not added | Block 5 |

## Migration path (Strangler)

1. **Query side first (value, low risk):** replace the Python query embedder +
   reranker with the Go onnxruntime_go engines behind the SAME §7a HTTP surface
   (`internal/processor` client is transport-agnostic) → dense cosine 0.99964,
   rerank Spearman 0.978 (re-run pending after the pair-form fix). Note: the
   measured Go CPU rerank latency is ~18.9 s/query (75 pairs) vs Python-MPS
   ~6 s — budget for that before swapping the query path (Block 5).
2. **GLiNER-ONNX** for entity extraction (parity ≤1e-5) — removes torch for this
   module.
3. **EPUB + chunking** in Go (algorithmic, no ML).
4. **Ingest ML (dense/sparse/rerank) on the farm** via onnxruntime CUDA-EP.
5. **PDF (digital)** via the Xberg Go binding; **scans** via Xberg/Marker sidecar
   with OCR on the farm; **mREBEL** stays a Python sidecar.
6. **Final strangle:** remove Python runner only after all components have a Go
   or sidecar path and a full gold-suite delta is green (data fixed).

## Open items / prerequisites

- **Go sparse divergence** (Block 3) — reframed after auto-review: primary
  suspicion is a harness truncation mismatch (gosparse 512 vs reference 8192;
  67/219 chunks exceed 512) — same class as the dense first-pass bug. Re-run
  with matched truncation first; only if divergence persists is the secondary
  hypothesis (Go-ORT mis-read of dynamic `[1,seq]`/`[1,seq,1024]` outputs, fix =
  statically-shaped single-output model or 1-D pre-reduction) actionable. Not a
  model/device blocker.
- **Tokenizer edge cases** (real corpus): 3/219 chunks diverge (rare `Ȭ`,
  hyphen-runs, a morpheme `contin`). Extend the Block-2 pin to these; map offset
  (Go `<s>` vs `<unk>` on uncommon chars).
- **R7 metric delta needs DB source-metadata sync** (Block 5): `zotero_documents`
  currently 3 rows, `processing_chunks` 0, while OS has 35k docs → gold hydration
  returns nothing for any runner. Sync the Postgres side before a final delta.
- **CUDA-column provings (3090 farm):** onnxruntime CUDA-EP for dense/rerank/
  GLiNER, Xberg OCR models — same ONNX files (this study proves CPU parity; the
  farm proves the CUDA-EP compile/link).

## Research-claims ledger (Hypothese → verifiziert/korrigiert)

- `eliben/go-sentencepiece` as Go tokenizer → **korrigiert**: BPE-only, XLM-R is
  unigram (cannot load). Winner: `tggo/goSentencePiece` (+HF reindex).
- `gomlx/tokenizers` for XLM-R on the Mac → **korrigiert**: prebuilt Rust lib only
  linux/amd64; won't link on darwin/arm64.
- `xberg-io/xberg` as a Candle-go PDF/locator tool → **korrigiert**: it's the
  Kreuzberg successor; Go binding installs (prebuilt FFI), digital extraction OK,
  but no default contract-locator chain.
- onnxruntime_go v1.33 needs ORT ≥1.29 (API 29) → verified (used 1.29.0 dylib,
  no brew).
- BGE-M3 sparse head is `Linear(1024,1)` + max-scatter (NOT a vocab projection) →
  **verified** (Block 3; formula overlap 1.0).

## Bottom line

Go-native runner is **Go: feasible** for contract/chunking/epub, dense, rerank,
GLiNER (all via ONNX, CUDA-EP on the farm — the Go GLiNER run itself is still
pending, see the table), and a Mini-runner proving the R7-E2E
pipeline. **No-Go or sidecar:** scanned-PDF conversion and mREBEL. The Mac-only
(GoMLX) trap is avoided by routing CUDA through onnxruntime_go + GLiNER/ONNX.
Blocked numbers (tokenizer edge cases, sparse Go divergence, R7 metric delta)
are environmental/tooling, each with a concrete fix — not conceptual blockers.
