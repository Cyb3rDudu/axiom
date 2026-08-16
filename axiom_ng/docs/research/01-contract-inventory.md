# Feasibility Study: Go-native Runner — Block 1 · Contract Inventory + Device Matrix

Study: `research/go-runner-feasibility` (from main `d9729c5`). Issue #171.
Block 1 output. Every claim verified against live code + `PROCESSOR_CONTRACT.md`,
not assumptions.

## 0. The measuring rod: correct endpoint surface (app.py ↔ contract)

Live route registrations (`axiom_ng_runner/app.py`) vs `PROCESSOR_CONTRACT.md` §5
and §7a — **all 10 endpoints present, verbatim**, no drift:

| Route (live app.py) | Contract § | Behavior (verified in app.py) |
|---|---|---|
| `GET /v1/health` | §5 | stub health {"status":"ok"} (app.py:339) |
| `GET /v1/capabilities` | §5/§6 | `_capabilities()` (app.py:97) — implementation `axiom-python-marker`, formats pdf+epub+zip, full feature set, model table, limits |
| `POST /v1/process` | §5/§7 | `process()` async (202 Accepted), idempotency dedup, source hash gate, feature flags |
| `GET /v1/jobs/{job_id}` | §5/§9 | status + `stage` + ordinal progress; stage vocab from `PIPELINE_STAGES` |
| `GET /v1/jobs/{job_id}/result` | §5/§10 | only for completed; job-local refs |
| `GET /v1/jobs/{job_id}/artifacts/{artifact_ref}` | §5/§13 | durable derived artifacts |
| `POST /v1/jobs/{job_id}/cancel` | §5 | cooperative cancel |
| `POST /v1/jobs/{job_id}/ack` | §5 | removes temp files after ack |
| `POST /v1/embed` | §7a (#131) | dense-only by default; `sparse` present ONLY when `include_sparse`; `EMBEDDING_SHAPE_MISMATCH` on dim/len drift; 4xx guards with contract codes |
| `POST /v1/rerank` | §7a (#132) | sigmoid scores descending, `top_n` archive-slicing semantics, `RERANK_SHAPE_MISMATCH` self-check |

Contract-vs-code deltas found (honest list, none blocking):

| Area | Contract states | Code does | Verdict |
|---|---|---|---|
| `images` | capabilities `images: true` (§6 example) | `_capabilities()` emits `"images": False` | **corrected in code; contract §6 example is illustrative ("examples"), not normative** — record code as truth |
| `models.name` | `entity_extraction`/`relationship_extraction` show `gliner`/`mrebel` example | reference backend advertises `reference-gliner`/`reference-mrebel`; real backend advertises real names via `capabilities()` | contract correctly says "must not assume names from this document" |
| `features.page_locators` | bool | true | consistent |

## 1. Component model — what must be ported / reimplemented in Go

The processor is a **pipeline of heterogeneous components**; parity must be argued
per component, not as one "runner":

| Component | Live Python | Params | Output |
|---|---|---|---|
| A. PDF → normalized Markdown | `compute_core/pdf_processing.py` → Marker v1 (`marker-pdf==1.10.2`, torch) | device from `DEVICE_MARKER`, `auto` on host | markdown + page labels (pymupdf) + pagination markers |
| B. EPUB → CFI + text | `epub_cfi.py` (`_collector` HTMLParser) | CPU, algorithmic, no ML | `{cfi, text, tag}` map |
| C. Text chunking | `compute_core/chunker.py` + `chunking.chunk_markdown` | — | contract chunks with locator (`page_span`/`epub_cfi`), section hierarchy, paragraph indices |
| D. Dense embeddings (passage) | `compute_core/embedder.py::TextEmbedder.embed_chunks` — `BAAI/bge-m3`, FP32 | `DEVICE_EMBEDDER` auto | 1024-dim FP32 (list→DB, OS index) |
| E. Dense query embeddings | `embed_queries_dense` — same model, `max_length=512` | warm singleton | 1024-dim FP32 |
| F. Sparse embeddings (lexical) | `embed_queries_with_sparse` / `embed_chunks` — BGE-M3 `lexical_weights` | — | `{token_id: float}` (contract §10 dual string/number form) |
| G. Reranking (cross-encoder) | `compute_core/reranker.py::QueryReranker` — `BAAI/bge-reranker-v2-m3`, `compute_score(normalize=True)`, FP16 on CUDA / FP32 on CPU+MPS | `DEVICE_RERANKER` auto | sigmoid-normalized 0..1 sorted scores |
| H. Entity extraction | `compute_core/entity_extractor.py` — GLiNER (`gliner_multi-v2.1`), zero-shot; spaCy fallback | `DEVICE_GLINER` **default `cpu`** (L8 lesson) | entities with mentions/confidence |
| I. Relation extraction | `compute_core/relation_extractor.py` — mREBEL (`Babelscape/mrebel-large`, Seq2Seq via transformers `AutoModelForSeq2SeqLM`) | `DEVICE_MREBEL` auto | entity relationships + evidence refs |

Note E=F are the *same* BGE-M3 encode pass with `return_sparse` toggled (symmetric
encoder — queries and passages cosine-comparable; the OS roundtrip test proves it).

## 2. Device matrix — per component, proven device → CUDA question

Local test host = **Apple Silicon (aarch64, MPS)**: torch 2.13.0, `mps=True`,
`cuda=False`. CUDA carries the 3090 farm (ingest workhorses) via
`EXTERNAL_RUNNER_DEPLOYMENT.md`. The matrix makes the **GoMLX = Apple-only**
blind spot explicit: a Mac-only-parity "green" study is NOT Go for the ingest
replacement.

Device resolution in live code: `compute_core/devices.py::get_model_device`
(direct env reads `DEVICE_EMBEDDER|DEVICE_MARKER|DEVICE_MREBEL|DEVICE_GLINER|DEVICE_RERANKER`,
`FORCE_CPU_MODE`, `PREFERRED_DEVICE_TYPE`, container env wins — TC2 rootless
Podman trap avoided via explicit per-model directive).

| Component | Model / impl | Proven path (this study) | Runs on CUDA? | CUDA candidate (research hypothesis — verify) |
|---|---|---|---|---|
| A. PDF→MD | Marker v1 | GoMLX **cannot**; this is a huge nn pipeline, not a single encoder. **No-Go for Go-native** on any device; keep Python/Marker OR Xberg-tabula hybrid (Block 4) | yes (marker on cuda) via as-is | mark as **No-Go native**; investigate only as subprocess/bridge |
| B. EPUB→CFI | algorithmic (HTMLParser) | pure Go, trivial | n/a (CPU) | n/a — **Go yes** |
| C. Chunking | custom | pure Go, algorithmic | n/a (CPU) | n/a — **Go yes** |
| D/E. Dense 1024 | BGE-M3 (XLM-R) | GoMLX (Apple-only) **or** onnxruntime_go CUDA-EP | **CUDA: onnxruntime_go CUDA-EP** (verify API/version) | Candle-cuda (Xberg dep) as secondary |
| F. Sparse lexical | BGE-M3 output | GoMLX / ONNX (same model) | CUDA: as D/E | same as D/E |
| G. Reranker | XLM-R cross-enc | GoMLX (`mxbai-rerank` ref) / ONNX | CUDA: onnxruntime_go CUDA-EP | Candle-cuda |
| H. GLiNER | XLM-R zero-shot | GoMLX? / **ONNX export** (Block 7 — zero-shot parity is the crux) | CUDA: onnxruntime_go | Candle-cuda |
| I. mREBEL | Seq2Seq | **No-Go native** (decoding loop) — options only | out of scope (opts) | sidecar / ONNX-decoding / replace |

**Summary of the CUDA question per component** (the finding the study is built
around): GoMLX is **Apple-only**. The 3090 farm needs either onnxruntime_go with
the CUDA execution provider, or Candle-cuda (pulled transitively by Xberg). Both
are hypotheses to verify in later blocks; the tokenizer pin (Block 2) is the
precondition before ANY of these model-comparison numbers are meaningful.

## 3. Normative anchors the study must satisfy (from the issue)

- Endpoints and behavior assessed against the **real contract** (done above).
- Sparse parity is **NOT** value-equality (ReLU/top-k boundary tokens drift under
  FP); measure token-overlap ≥ 0.98 + shared-weight cosine ≥ 0.999. DB values
  parsed via the **§10 duality**: `repo.ParseSparse` accepts both the canonical
  string form `{"12":"0.5"}` and the native number form `{"12":0.5}`
  (`axiom_ng/internal/repo/outbox_sparse_test.go`) — a Go sparse reader must
  handle both.
- Tokenizer pin (Block 2) must precede all model compares, incl. the German
  **umlaut/NFD-NFC** case.
- Determinism class: a Go port is a new risk class (same math, different kernel
  order). **Every Go PoC runs twice; byte-equality of own outputs is the
  precondition** for any parity measurement.

## 4. File map (verified exist on the study branch)

- Contract + cross-check: `axiom_ng/docs/PROCESSOR_CONTRACT.md`, `axiom_ng_runner/app.py`
- Real models: `axiom_ng_runner/compute_core/{embedder,reranker,entity_extractor,relation_extractor,pdf_processing}.py`, `devices.py`
- Canonical R7 benchmark + gold: `axiom_ng/docs/RETRIEVAL_BENCHMARK.md`, `axiom_ng/cmd/retrieval-bench/{main.go,gold_suite.json}`
- Deploy/CUDA path: `axiom_ng/docs/EXTERNAL_RUNNER_DEPLOYMENT.md`
