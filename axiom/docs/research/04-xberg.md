# Feasibility Study — Block 4 · Xberg PDF→Markdown + Locator-Fidelity (#171)

**Research-corrected.** The `xberg-io/xberg` in research_findings.md as a "Candle-go
PDF→Markdown with locator fidelity" is a stale hypothesis. The current `xberg-io/xberg`
is the **Kreuzberg successor** (Rust core, 100 formats, Candle/ONNX OCR, table/layout
ML, 15 language bindings) — a broad document-intelligence engine, not a dedicated
locator-preserving chunker. **Verified/corrected** in the report.

## Addendum (measured on this study)

`github.com/xberg-io/xberg/packages/go` v1.0.14 (**Go binding installs and runs on
macOS arm64, no brew** — it downloads a prebuilt `libxberg_ffi` from GitHub releases
into `~/Library/Caches/xberg/`, + a cgo link shim; the Rust core is a prebuilt FFI,
not a Cargo build). One setup quirk: the `cmd/setup` writes a shim whose sentinel is
one version behind (`1_0_13` vs the module's `1_0_14`); patch the sentinel to build
(0-min fix, documented).

POC `cmd/feasibility/xberg/`:
- `xextract` (Go): `Extract(ExtractInputFromURI(pdf), config)` → `Results[0].Content`
  + `Counts.Pages` + `Pages[].PageNumber`.

## Measured results

| Input | Xberg markdown | Xberg pages | Marker ground truth (reference) |
|---|---|---|---|
| Digital 5-page PDF (text layer, generated) | **679 chars, all 5 pages, umlauts correct** | `pages=0` (no per-page granularity in default output) | Marker produces per-page blocks |
| Management book `Dubs_…_2004.pdf` (8.3 MB) | **empty `markdown_len=0`** | 39 pages | 122 Marker-pages, 1895 s runtime |

Two hard findings:

1. **Scanned PDFs → empty without OCR.** The management book is a **full scan**: 39
   physical pages, **0 with an embedded text layer** (pymupdf check). Xberg default
   returns empty content; only OCR (`Ocr`/`ForceOcr`) recovers text (heavy: layout
   models + tesseract/ONNX — the deploy-path cost). Marker handled it (1895 s) because
   its OCR/layout pipeline ran.
2. **Locator chain not exposed by default.** Xberg's binding output aggregates Markdown
   (`Content`) but does **not** provide, in the default config, the contract-§11 chunk
   locator fields the citation layer needs: `locator.page_label_start/end`,
   `physical_page_start/end`, `structure.section_titles`, `start/end_paragraph_index`.
   It exposes `Pages[].PageNumber` only when page segmentation runs (empty here), and
   the chunker (`ChunkerTypeMarkdown`) would need explicit config + a per-chunk locator
   mapping to reconstruct page_spans. Without an equivalent, the **Zitier-Ebene breaks**
   even with good search (Epic-C-Fundament).

## epub_cfi (inventory)

`axiom_ng_runner/epub_cfi.py` is **algorithmic, no ML** (`HTMLParser` walking the EPUB
spine + XHTML to build `{cfi, text}` maps). This is **pure Go-portable** (Go `golang.org/x/net/html`
mirrors the HTMLParser walk; CFI path composition is string logic). Already inventoried
in the Block-1 device matrix as **Go: yes**.

## Verdict (Block 4)

- Go binding install: **green** (no brew, prebuilt FFI).
- Digital text extraction: **green** (umlauts, multi-page).
- **Locator fidelity: red** in default config — Xberg does not emit the contract's
  page/section locator chain on the tested path; reconstruction needs per-page OCR
  + explicit chunk-locator mapping. For scanned books, OCR (heavy) is unavoidable
  regardless of engine.
- **epub_cfi chain: Go-portable** (pure algorithm).

**Follow-up for the CUDA path / 3090 farm:** Xberg's OCR/layout models (Candle/ONNX)
on the farm, where tesseract + LSTM models are packaged via EXTERNAL_RUNNER_DEPLOYMENT,
is the realistic scanned-PDF route. Locator mapping must be built around Xberg's page
events (or an explicit per-page OCR output).
