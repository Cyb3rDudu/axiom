# Xberg locator-fidelity on the carrier (Nachzug point 8) — scoped finding

## Digital PDFs: default-output locator gap CONFIRMED

The Block-4 Mac finding holds regardless of device: Xberg's Go binding default
`Extract` output aggregates clean Markdown (`Content`) but exposes **no contract
§11 chunk locators** — `Counts.Pages` and per-page `Pages[].PageNumber` are
populated only when page segmentation runs (empty in default for the test PDFs),
and there is no `locator.page_label_start/end`, `physical_page_start/end`, or
`structure.section_titles` mapping in the default output. This is a **binding-level
default-output limitation**, not an OCR/device one.

## Scans: candle-cuda is the right place, but the locator chain still depends on Xberg

- The management book is a full scan (39 physical pages, 0 with an embedded text
  layer — pymupdf). Text recovery requires OCR (Xberg Candle/ONNX), which is the
  carrier's GPU job (candle-cuda).
- **Importantly: OCR changes text recovery, not locator-chain exposure.** Xberg's
  default output omits the contract locators for BOTH digital and OCR'd sources;
  a per-chunk `page_span`/`physical_page`/`section` mapping must be built around
  Xberg's page events regardless of device.
- The full candle-cuda OCR run on the scan is a heavy setup (Xberg OCR model
  download on the carrier). The marginal new evidence beyond Block 4 is text
  recovery on the scan, NOT the locator chain — which is the citation-critical
  piece and remains gated on an explicit Xberg locator-mapping layer.

## epub_cfi

Already Go-portable (Block 1/4, algorithmic), unaffected by device.

## Verdict (point 8)

**Xberg locator-fidelity gap is device-independent** — proven on the Mac (digital),
and the carrier candle-cuda path would only add scan text-recovery, not the
contract locator chain. A Go runner using Xberg must wrap it with an explicit
per-chunk locator mapping (`page_span`/`physical_page`/`section_titles`) — the
citation-level prerequisite (Epic C). The candle-cuda scan-OCR run is the natural
follow-up on the farm once such a mapping layer exists (and Marker ground truth is
available for the 39-page book).
