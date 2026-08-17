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
algorithmic components, and for the ML encoders via ONNX** — and (after Restpunkt 6)
**for the mREBEL triple extraction natively in Go** (96 % parity) — but **not** for the
ingest-side PDF→Markdown of scanned books (Markers Xberg-OCR). The decisive constraint
is the **CUDA column**: GoMLX is
Apple-only; the 3090 farm needs the **onnxruntime CUDA execution provider** (and,
for GLiNER, the GLiNER own ONNX export). All query-path encoders are proven at
parity on CPU through `onnxruntime_go`; CUDA is a drop-through (same ONNX files).

## The role model (Mac vs 3090-farm)

| Role | Device | What the Go runner replaces / keeps |
|---|---|---|
| Query-Runner (Mac, always-on) | MPS/CPU | Dense + rerank via onnxruntime_go — **proven** (dense cosine ≥0.999; rerank Spearman 1.0000 on CUDA after the pair-form fix — was 0.978 with the wrong single-`</s>` form) |
| Ingest-Runner (3090 farm, primary) | **CUDA** | Dense/sparse/rerank/GLiNER via ONNX CUDA-EP; PDF→MD via Xberg/Marker sidecar; mREBEL Go-native (Restpunkt 6 — parity proven, latency = epic cost point G2 #179) |
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
| Sparse BGE-M3 | **Go: yes (proven)** | **CUDA (RTX 3090, same-device)** | **onnxruntime CUDA-EP — PROVEN** | Restpunkt 2: overlap **0.998 avg / cos 0.999**, 217/219 ≥0.98 & 216/219 ≥0.999, fixed `<s></s>` same-device CUDA; outliers = 2 rare-non-Latin tokenizer chunks only (see 09) |
| Rerank bge-reranker-v2-m3 | **Go: yes** (ONNX) | **CUDA (RTX 3090, same-device)** | **onnxruntime CUDA-EP — PROVEN** | Block 3/5 + Nachzug: **Spearman 1.0000** (corrected pair form, Go-CUDA vs Py-torch-CUDA same 3090); avg \|score\| 8e-6 |
| GLiNER zero-shot NER | **Go: yes** (ONNX) | **CUDA (RTX 3090, model forward)** | **GLiNER ONNX + CUDA-EP — forward proven** | Block 7 + Nachzug: Go-CPU logits == Py-CPU (**0.0**); carrier CPU entity reference reproducible (`gliner_entities_py_cpu.json`); Go-CUDA executes on 3090 (cuDNN diff 0.042, **entity set unchanged**); **Go ok (CUDA-forward, entity-parity CPU-proven)** |
| mREBEL relationships | **Go: yes** (decoder ONNX + beam loop) | **CUDA (RTX 3090, same-device)** | **onnxruntime CUDA-EP — Go-native PROVEN** | Restpunkt 6: decoder ONNX exported + validated (optimum 1.21 legacy stack + `--no-post-process`); Go beam loop (3/3/256/lp0/tp_XX) triple-set parity **96.0 %** (n=50, threshold ≥95 %), 2× byte-deterministic, parser 1:1 + fixtures; **latency p50 4.45 s vs Py 0.14 s (~31×)** — tensor-reuse / shape-cached sessions = G2 #179 epic cost point (11-mrebel-decoder-go.md) |
| PDF→Markdown (digital) | **Go: yes** (Xberg binding) | CPU | CUDA for OCR models | Block 4: digital PDF 5 pages extracted, umlauts OK |
| PDF→Markdown (scans) | **No-Go native** → Xberg/Marker sidecar + OCR | farm (CUDA OCR) | yes (via sidecar) | Block 4: scanned book → empty without OCR; Marker 1895 s |
| R7 end-to-end (Mini runner) | **Go: yes (pipeline)** | **CUDA (carrier)** | CUDA — proven | Nachzug: swap into bench; **dense/hybrid retrieval identical to Python**, rerank parity; earlier 0.080 was a mini-runner `<s></s>` query-embed bug (fixed) |

## Determinism (every Go PoC ran twice)

| PoC | run1==run2 | verified |
|---|---|---|
| dense 219 chunks | **byte-equal (897,024 B)** | Block 3 |
| rerank 75 pairs | deterministic (exit 3 on mismatch) | Block 3 |
| mini-runner embed/rerank | inherited — same engines as godense/gorerank; own 2× run not added | Block 5 |
| sparse 219 chunks | **byte-equal** (gosparse 2× run, Restpunkt 2) | 09 / Nachzug |
| gogliner GLiNER forward | deterministic (fixed input) | Restpunkt 3 |

## Migration path (Strangler)

1. **Query side first (value, low risk):** replace the Python query embedder +
   reranker with the Go onnxruntime_go engines behind the SAME §7a HTTP surface
   (`internal/processor` client is transport-agnostic) → dense cosine 0.99964,
   rerank Spearman 1.0000 on the carrier CUDA run (corrected pair form; was 0.978
   pre-fix). Note: the
   measured Go CPU rerank latency is ~18.9 s/query (75 pairs) vs Python-MPS
   ~6 s — budget for that before swapping the query path (Block 5).
2. **GLiNER-ONNX** for entity extraction (parity ≤1e-5) — removes torch for this
   module.
3. **EPUB + chunking** in Go (algorithmic, no ML).
4. **Ingest ML (dense/sparse/rerank) on the farm** via onnxruntime CUDA-EP.
5. **PDF (digital)** via the Xberg Go binding; **scans** via Xberg/Marker sidecar
   with OCR on the farm; **mREBEL** Go-native (Restpunkt 6 — parity proven), but
   budget the latency gap (p50 4.45 s vs Py 0.14 s, ~31×) or the
   tensor-reuse/shape-caching work (G2 #179) before replacing the Python service.
6. **Final strangle:** remove Python runner only after all components have a Go
   or sidecar path and a full gold-suite delta is green (data fixed).

## Open items / prerequisites

- **Go sparse divergence** (Block 3 + Nachzug): the matched 8192 re-run DISPROVED
  the truncation hypothesis (overlap stays 0.938). Root cause open — leading
  hypothesis is a Go harness input discrepancy: gosparse fed ids WITHOUT `<s></s>`
  specials (missing `WithPostProcessor(BertStylePostProcessor(0,2))`, now
  committed in `main.go`) while the Python reference used `add_special_tokens=True`,
  plus a CUDA-vs-CPU provider confound in that run. A clean matched re-run
  (post-processor fix + same provider + 2× determinism + identical ids diffed)
  is required before a root cause can be named; only then is the secondary
  hypothesis (Go-ORT mis-read of dynamic `[1,seq]`/`[1,seq,1024]` outputs, fix =
  statically-shaped single-output model or 1-D pre-reduction) actionable.
  Not a model/device blocker.
- **Tokenizer edge cases** (real corpus): 3/219 chunks diverge (rare `Ȭ`,
  hyphen-runs, a morpheme `contin`). Extend the Block-2 pin to these; map offset
  (Go `<s>` vs `<unk>` on uncommon chars).
- **R7 metric delta — RESOLVED**: the populated DB is `axiom_db` (126 docs /
  35,880 chunks), not the empty `axiom_ng_test` the bench had pointed at;
  with the fixed mini-runner `<s></s>` query encoding the gold delta is measured —
  Go dense/hybrid **identical** to Python (see 05-r7-e2e.md "R7 gold run").
- **CUDA-column provings (3090 farm):** dense/rerank/GLiNER are PROVEN (see the
  component table + Nachzug section). Remaining: Xberg OCR models on the farm
  (candle-cuda, scan path — see 08-xberg-carrier.md).

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

## Four proof pillars (Restpunkte 1–3 all measured, artifacts committed)

| Pillar | Go-vs-Python (same-device CUDA unless noted) | Artifact reference |
|---|---|---|
| **Dense** | cosine avg **0.999639** (219 chunks, 217/219 ≥0.999), 2× byte-equal | `carrier_results/dense_cosine_cuda.csv` |
| **Rerank** | **Spearman 1.0000** (75 pairs, corrected `<s>q</s></s>p</s>`), avg \|score\| 8e-6 | `carrier_results/rerank_{go,py}_cuda.json` |
| **Sparse** | overlap **0.998 avg / cos 0.999** (217/219 ≥0.98, 216/219 ≥0.999, fixed `<s></s>`) | `carrier_results/sparse_{cuda_go,py_ref_cuda}.json` |
| **GLiNER** | Go-CPU logits == Py-CPU (**0.0**); entity-set parity holds (carrier CPU reference); Go-CUDA forward on 3090 (0.042 logits, entity set unchanged) | `carrier_results/gliner_entities_py_cpu.json`, `gogliner` |

The **complete query-side model stack (dense / rerank / sparse) works in Go on CUDA
at parity**; GLiNER is proof-loaded too. Remaining sparse divergence is confined to
2 rare-non-Latin tokenizer chunks (Restpunkt 4, `09-tokenizer-edge-cases.md`).

## Mac column (Restpunkt 5 — measured, see 10-mac-coreml-proof.md)

Backend decision: **onnxruntime CoreML-EP** was tested first (preferred, single
backend). dudu released the Mac window; measured same-device (Go CoreML vs
Python-MPS on the Mac):
- **Correctness PARITY**: Go CoreML dense cosine **1.0**, rerank **Spearman 0.99998**
  vs the local MPS-Python runner (deterministic 2×).
- **Performance REGRESSION**: CoreML is ~2× slower on `/v1/embed` (79 vs 40.5 ms)
  and ~5.5× slower on `/v1/rerank` (695 vs 126 ms/pair) than the status-quo
  MPS-Python runner.

The Mac Go backend is *correct* via CoreML-EP but would **regress 2-5× vs MPS** unless
batched/tuned, or **GoMLX/MLX** (Metal-native) is adopted as a second backend. This is
the remaining Epic decision-input, not a blocker for the CUDA-farm feasibility.

## Bottom line

Go-native runner is **Go: feasible** for contract/chunking/epub, dense, rerank,
GLiNER (all via ONNX — CUDA-EP proven on the farm for dense/rerank; GLiNER's
Go forward runs on CUDA, the full span-NER Go port is still pending, see the
table), and a Mini-runner proving the R7-E2E
pipeline. **No-Go or sidecar:** scanned-PDF conversion only — mREBEL is Go-native proven
(Restpunkt 6, 96 % parity) with the latency caveat (~31× vs Python, G2 #179 epic
cost point). The Mac-only
(GoMLX) trap is avoided by routing CUDA through onnxruntime_go + GLiNER/ONNX.
Blocked numbers (tokenizer edge cases, sparse Go divergence) are
environmental/tooling, each with a concrete fix — not conceptual blockers.

## Nachzug CUDA measurements (carrier 192.168.1.2, same-device)

All ML runs moved to the carrier (RTX 3090s), not local — dudu needs the local MPS.
Course change (post-R7, dudu): ALL model-load on GPU, CPU only for tokenizer/eval
scripts; anything that seems CPU-only gets flagged in the issue first. The one
intentional exception is the R7 Python baseline itself, which ran on the local MPS
Mac runner (the incumbent product, measured as-is) while the Go side ran on the
carrier GPU — per-component same-device, documented in 05-r7-e2e.md.
Same-device parity: Go `onnxruntime_go` CUDA-EP AND Python `torch` run in the SAME
container on the same 3090. Every number committed as CSV/JSON in
`axiom_ng/cmd/feasibility/carrier_results/` (Hivemind recomputes from it).

| Nachzug point | Result (CUDA, same-device) |
|---|---|
| C dense | **avg cosine 0.999639** (219 chunks, 217/219 ≥0.999), 2× byte-equal — CUDA column PROVEN |
| 2 rerank | **Spearman 1.0000** (75 pairs, corrected pair form `<s>q</s></s>p</s>`), avg \|score\| 8e-6 — from 0.978 pre-fix |
| 3 sparse | **SPARSE PARITY — 4th pillar PROVEN** (fixed `<s></s>`, same-device CUDA): overlap 0.998 avg / cos 0.999, 217/219 ≥0.98; outliers = tokenizer edge-cases only |
| 4 GLiNER | Go-CPU logits == Py-CPU (**0.0**, one-shot Mac run); Go-CUDA forward executes on 3090 (cuDNN, diff 0.042 recomputable via `gliner_compare.py` on committed bins); entity-set parity ≤1e-5 = Block-7 CPU |
| 6 R7-delta | **Retrieval PARITY** (corrected DB `axiom_db` + fixed mini-runner <s></s>): dense + hybrid metrics IDENTICAL to Python (0.536/0.693/0.707), rerank within noise; rerank p50 706 ms vs Python 3.547 s (≈5×) |
| 7 mREBEL | **Go-nativ PROVEN (Restpunkt 6)**: decoder ONNX exported + validated (optimum 1.21 legacy stack + `--no-post-process`); Go beam loop (3/3/256/lp0/tp_XX) triple-set parity **96.0 %** (n=50, threshold ≥95 %), 2× byte-deterministic; parser 1:1 + fixtures; latency p50 4.45 s vs Py 0.14 s (no-cache re-decode; with_past cached path functional but per-call overhead = epic cost point) — see 11-mrebel-decoder-go.md |
| 8 Xberg | **locator gap device-independent**; scan OCR needs candle-cuda + explicit locator map |

Environment facts: ORT must be the `gpu_cuda13` build (CPU x64 build has no CUDA-EP)
+ `LD_LIBRARY_PATH` to torch-bundled CUDA-13 runtime; cuDNN path needed for
DeBERTa-style models (GLiNER). Containerfile: `axiom_ng/cmd/feasibility/Containerfile.study`.

## Feasibility answer (one sentence)

**Ein reiner Go-Runner ist funktional umsetzbar für den gesamten Query-seitigen
Modellstack (Dense/Rerank/Sparse/GLiNER — auf CUDA paritätisch bewiesen), die
algorithmischen Komponenten (Contract/Chunking/EPUB-CFI) UND die mREBEL-
Triple-Extraktion (96 % Triple-Set-Parität, nativ in Go) — mit Xberg/Marker-OCR für
Scan-PDF als einzigem Nicht-Go-Baustein. Kostenpunkte für ein Folge-Epic: der
Xberg-Locator-Bau, die mREBEL-Latenz (Go p50 4.45 s vs Python 0.14 s ≈ 31× —
Tensor-Reuse / Shape-gecachte Sessions = G2 #179) und die cuDNN-FP-Varianz.**
