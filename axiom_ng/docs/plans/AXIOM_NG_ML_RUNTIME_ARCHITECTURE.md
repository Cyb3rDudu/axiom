# axiom-ng ML runtime architecture

**Status:** design, unimplemented (Phase I below is the first concrete change).
**Author of record:** chat session 2026-04-20, documented here so future contributors see the reasoning.
**Scope:** how ML models (embedders, reranker, NER, vision, PDF conversion, relation extraction) run alongside the Go backend after the Python `axiom-backend` and `axiom-doc-processor` containers are retired.

## Goal

Eliminate always-on Python processes where possible. Where Python must stay (models without non-Python runtime equivalents), keep its surface small and subprocess-isolated so VRAM is reclaimed by process exit.

## Hard constraints

The architecture has to respect the following, which came out of a team research pass on 2026-04-20 across ONNX Runtime, llama.cpp, vLLM, Ollama, Triton, PyTorch:

1. **True VRAM release requires process exit.** Every framework surveyed leaks CUDA context memory when a model is "unloaded" in-process. vLLM sleep mode is the cleanest fallback (~90% reliable); Ollama's `KEEP_ALIVE=-1` is documented to evict anyway (GH issue #13227); Triton's idle eviction never reclaims the arena (#7594); ONNX Runtime's `OrtReleaseSession` leaves 300–700 MB of CUDA context resident (#9839, #7450). The Microsoft-recommended answer is "use a subprocess." The subprocess pattern used by today's `pdf_worker`/`relation_worker` is therefore **industry-correct**, not a Python workaround.
2. **ORT multi-model swap in a single Go process is not production-safe.** Swapping models inside one long-lived Go binary leaks CUDA context fragments over hours. No published production systems do this at scale.
3. **Cold start of a fresh Python+torch+CUDA subprocess is 10–20 seconds** on an RTX 4090 / L4 / A100 before BGE-M3 completes its first forward pass. Cold start of a Go binary linked to ORT + one ONNX model is ~2–3 s.
4. **Query latency must stay < 5 s P95** for interactive search. Per-request spawn would tank this unless the pool is kept warm.
5. **Ingest latency is not user-blocking.** The upload endpoint returns `processing` immediately; spawn-per-document is acceptable for PDF conversion, relation extraction, and image embedding.
6. **Marker (PDF → markdown) has no non-Python equivalent** that matches its quality on academic PDFs. Surya/Texify/table-recognition are PyTorch-only. The closest alternatives are Rust `pdfium-render` + `go-fitz` (fast, native-text only) and cloud APIs (Mistral OCR, LlamaParse) — neither replaces Marker for scanned or image-heavy PDFs.

## Model inventory

| Model | Purpose | VRAM (resident) | Call frequency | Runtime implication |
|---|---|---|---|---|
| BGE-M3 | dense + sparse embeddings (1024-dim) | ~2 GB | query (hot) + ingest (batch) | Hot-path: must be pool-resident. Sparse unavailable in llama.cpp today. |
| BGE-Reranker v2-m3 | cross-encoder reranking | ~2 GB | query (hot) | Hot-path: pool-resident. |
| GLiNER-small-v2.5 | zero-shot multilingual NER | ~500 MB | ingest (batch) | Cold-tolerant; subprocess fine. |
| CLIP ViT-B/32 | image embeddings (512-dim) | ~150 MB | ingest (batch) | Cold-tolerant; small enough to stay resident cheaply. |
| Marker + Surya | PDF → markdown + images + OCR | ~1–2 GB | ingest (per PDF) | Must remain Python subprocess. |
| mREBEL-large | seq2seq relation extraction | ~2.4 GB | ingest (optional, per batch) | Subprocess-isolated today for VRAM reasons. |
| spaCy (en_lg / de_lg) | NER fallback | ~600 MB RAM | rare (GLiNER fallback) | Drop; rely on GLiNER. |

## Architecture

Two paths, because query and ingest have opposite latency requirements.

```
axiom-ng (Go binary)
│
├─ QUERY PATH (interactive, <5s P95)
│  └─ GPU worker pool
│     • N = 1 warm Python worker process (today), 2–4 when concurrent demand warrants.
│     • Owns: BGE-M3 + Reranker (+ GLiNER for ad-hoc entity queries).
│     • Idle timeout: 300 s (5 min) — pool empty after idle → VRAM fully released on process exit.
│     • First query after idle pays cold-start (~10 s). Subsequent calls <50 ms.
│     • Can be Python today, Go+ORT helper binary later per-model.
│
├─ INGEST PATH (batch, not user-blocking)
│  │
│  ├─ PDF classify-and-route
│  │  • pdf-inspector (Rust) or simple heuristic detects native-text vs scanned.
│  │  • Native-text (~85% of academic PDFs) → go-fitz (in-process Go) → markdown in ~50–200 ms.
│  │  • Scanned / image-heavy → existing Marker subprocess (Python).
│  │  • Hybrid routes ~85% of PDFs with zero Python involvement.
│  │
│  ├─ Chunker (Go, in-process — already shipped)
│  │
│  ├─ Embedding — reuses query-path worker pool, or spawns an ingest-only helper for large batches.
│  │
│  ├─ Entity extraction (GLiNER) — via worker pool.
│  │
│  ├─ Image embedding (CLIP) — via worker pool.
│  │
│  └─ Relation extraction (mREBEL) — short-lived Python subprocess per batch (unchanged).
│
└─ IDLE STATE
   After N seconds of no queries + no ingest: zero processes, zero VRAM.
```

## Key properties

- **Scale-to-zero.** After the idle timeout, every worker process has exited. `nvidia-smi` shows zero residency. Axiom is architecturally compatible with dynamic GPU allocation on shared clusters.
- **Burst-tolerant.** A small pool of warm workers absorbs query bursts at <50 ms latency.
- **Subprocess isolation for heavy one-shots.** PDF (Marker), relation extraction (mREBEL), and potentially any large ONNX helper spawn-and-die. VRAM reclaimed reliably.
- **Progressive Go takeover.** Each model with a community ONNX export (BGE-M3, BGE-Reranker, GLiNER, CLIP) can migrate independently from the Python worker to a Go+ORT helper subprocess. Migrations are gated by differential testing, not scheduled as a single cutover.
- **Marker stays Python** but fires only on the ~15 % of PDFs that need it.

## What "Python-free" actually means here

This is not a full pure-Go stack — **Marker has to stay Python**, and the research confirmed there is no serious non-Python alternative for academic-PDF quality. What this architecture removes is:

- The **always-on** Python processes (`axiom-backend`, `axiom-doc-processor`).
- The **long-lived GPU worker** residing as Python; later phases may replace it with Go helper subprocesses model-by-model.
- The **in-process CLIP loader** inside the old doc-processor; it joins the worker instead.
- **spaCy** as a fallback; rely on GLiNER.

What stays Python, by design, not by accident:

- `pdf_worker` subprocess for scanned PDFs (~15 % of the corpus).
- `relation_worker` subprocess for optional mREBEL relation extraction.
- The shared GPU worker, until Phase III migrates its models to Go helpers.

## Rollout

### Phase I — CLIP into the shared worker + aggressive idle-exit

**Effort:** ~1 day.

- Add `handle_embed_images` to `gpu_worker/server.py`. Load CLIP once, batch-encode on call.
- Add `EmbedImages` to Go `gpuworker.Client`.
- Add `ImageProcessor` ingest stage that reads `ImagesDir/{doc_id}/*`, batches through `EmbedImages`, inserts `document_images` rows with 512-dim vectors + alt-text.
- Lower `AXIOM_GPU_WORKER_IDLE_SEC` default from 900 s → 300 s. Confirm the Go client's spawn-on-missing-socket path is exercised (it already is).

**Outcome:** CLIP inconsistency resolved. Four models share one worker process. VRAM released after 5 min of idle.

### Phase II — PDF classify-and-route

**Effort:** 2–3 days.

- Add a classify step to `PDFProcessor.Process`. `pdf-inspector` (Rust via CGO) or a heuristic (count native text chars per page) decides native-text vs scanned.
- Native-text branch: use `gen2brain/go-fitz` (CGO wrapper over MuPDF) to extract markdown + images in-process. No subprocess.
- Scanned branch: existing Marker subprocess.
- Differential test against a 50-PDF corpus (mix of native + scanned academic papers) to pin the classifier's accuracy.

**Outcome:** Marker fires on ~15 % of PDFs. The 85 % majority path is pure Go, zero VRAM, <200 ms per doc.

### Phase III — Prove the Go+ORT helper pattern on one model

**Effort:** ~1 week.

- Pick **BGE-Reranker v2-m3** as the pilot: community ONNX exports exist, it's a simple cross-encoder with no sparse or decoder-loop complications.
- Build a small Go binary `axiom-ng-rerank-helper` that:
  - Loads the ONNX model + tokenizer on startup.
  - Reads a JSON request from stdin, writes JSON scores to stdout, exits.
  - Links ORT CUDA via `yalue/onnxruntime_go` + `daulet/tokenizers`.
- Go parent spawns the helper per rerank call (or keeps a small pool of helpers warm with the same idle-exit policy as the Python worker).
- Differential test: same query + doc set through Python `rerank` and Go helper. Assert score correlation ≥ 0.99.

**Go/no-go criteria:**
- Cold start ≤ 3 s.
- Peak latency (warm) within 20 % of Python path.
- Output correlation ≥ 0.99 on test corpus.

If all three pass, template the pattern for GLiNER and CLIP (both have community ONNX). BGE-M3 follows only if sparse embedding can be decoded in Go correctly — skip if in doubt, keep BGE-M3 on the Python worker.

### Phase IV — Migrate models one at a time

**Effort:** 3–6 weeks total, spread across the migration.

- Each migration is a standalone PR with its own differential test harness.
- Python gpu_worker retains all methods until its last caller is gone.
- Never migrate: Marker, mREBEL (subprocess already; migration ROI is low).

## Non-goals

- **A single statically-linked Go binary.** Even after Phase IV, the production image carries ORT CUDA shared libraries and the daulet/tokenizers Rust FFI object. This is an ops concern, not a runtime concern — the service runs as a Go process; it is not shelled out to.
- **Replacing Marker.** The quality floor on academic PDFs does not permit it today. Revisit in 2027 if Qwen-VL / SmolDocling / successor models show parity.
- **Eliminating mREBEL's subprocess.** A ~2.4 GB seq2seq model with beam-search decoding is a poor fit for in-process residency; the subprocess pattern stays.

## References

- axiom_backend/services/background_document_processor.py — Python baseline being replaced.
- axiom_backend/ai_researcher/gpu_worker/ — existing shared worker (msgpack over AF_UNIX).
- axiom_backend/ai_researcher/pdf_worker/ — reference for the subprocess-per-job pattern.
- docs/plans/AXIOM_NG_GO_MIGRATION.md — overall phase plan.
- axiom_backend_ng/internal/gpuworker/ — current Go RPC client.
- axiom_backend_ng/internal/ingest/builder.go — where the new stages slot in.

### External

- [ONNX Runtime memory release issue #9839](https://github.com/microsoft/onnxruntime/issues/9839) — confirms `OrtReleaseSession` does not release CUDA context.
- [Ollama KEEP_ALIVE eviction issue #13227](https://github.com/ollama/ollama/issues/13227) — unreliable in-process unload.
- [Triton idle GPU memory issue #7594](https://github.com/triton-inference-server/server/issues/7594) — arena allocator never returns to OS.
- [vLLM sleep mode](https://docs.vllm.ai/en/latest/features/sleep_mode/) — closest thing to reliable in-process unload (~90 %).
- [pdfium-render crate](https://crates.io/crates/pdfium-render) — Rust bindings to Chromium's PDFium for the fast path.
- [gen2brain/go-fitz](https://github.com/gen2brain/go-fitz) — Go wrapper over MuPDF.
- [yalue/onnxruntime_go](https://github.com/yalue/onnxruntime_go) — Go ORT bindings for Phase III helpers.
- [daulet/tokenizers](https://github.com/daulet/tokenizers) — Go CGO bindings to HF tokenizers (required for ONNX paths to produce tokens that match training).
