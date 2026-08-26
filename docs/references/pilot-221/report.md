# Pilot #221 — Four-Point Alignment: Databricks EPUB+PDF pair

Offline pilot, feeds #220 Stage 3. Tooling: `axiom_ng_runner/scripts/four_point_pilot.py`
(run with `/opt/axiom/runner/current/env/bin/python`, needs pymupdf). All artifacts in
this directory; the 406-marker EPUB map is regenerable via the script and not committed.

Assets: `Databricks Data Intelligence Platform.epub` (406 `doc-pagebreak`, Apress 2nd ed)
↔ `979-8-8688-2524-8.pdf` (482 physical pages, text layer).

## Headline results

| Point | Evidence |
|---|---|
| EPUB anchors | 406 markers harvested (−30 roman … 448 arabic), spine 50, unique; **marker numbers ARE print folios** — verified by content join, not assumption |
| PDF ground truth | 448 arabic folios in 14 monotone runs; 10 unnumbered pages (title, chapter openers), 0 blanks |
| Alignment | **382/406 verified joins** (`ok`); non-`ok`: 13 non-monotonic (6 frontmatter spine disorder, 6 relocation artifacts, 1 backmatter table marker PB185 in `Table_01_0008_0005.xhtml`), 5 boundary, 3 openers, 2 residual low-sim (markers 217, 244 — folio join correct, content sim only 0.37/0.14, likely display-heavy pages; flagged, unexplained) |

Physical↔folio offset drifts 21–28 across the book (part openers insert unnumbered
pages) — a constant-offset joiner would be wrong; the folio key holds.

## Divergence classification (the #220 Stage-3 format)

| Class | Count | Semantics |
|---|---|---|
| `ok` | 382 | marker number == PDF folio, content containment ≥ 0.4 |
| `marker_boundary_next_page` | 5 boundary (markers 111, 119, 184, 369, 385 land on folio n+1) + 2 relocations (marker 78 → folio 77, marker 391 → 393; 78 is also counted under `non_monotonic` below, so the table sums to 406) | EPUB marker sits at printed page bottom; following text lives on next folio (or, for 78/391, on the previous/next page). Benign, deterministic |
| `opener_unnumbered_in_pdf` | 3 (folios 10, 126, 156 → phys 38/150/180) | chapter opener pages carry no printed number; joiner locates via content between folio n−1 and n+1 |
| `folio_without_marker` | 54 | 13 = index (449–461, markers stop at 448); 41 scattered (94–123 cluster = figure/listing pages where Apress drops markers) |
| `non_monotonic` | 13 rows | 6 roman frontmatter out of spine order (xvii/xviii swap, xxii–xxx block); 6 relocation artifacts (boundary relocations hop +2 phys: 112, 120, 370, 386, 393, and marker 78 → folio 77); 1 backmatter table marker (185) — EPUB spine order ≠ folio order |
| `low_text_sim` (residual) | 2 (markers 217, 244) | folio join itself is correct (217→phys 241, 244→phys 268); content containment below threshold — suspected display-heavy pages; carried as flagged, unexplained |
| `marker_folio_mismatch` (hard) | 0 | no genuine print-divergence found |

76 unmarked PDF pages (482−406) resolve as: 448 arabic − 406 markers = 42 body folios
without marker + 13 index + front/backmatter ≈ the observed classes.

## page_source proposal for this document class

1. **`print` (EPUB marker number)** — wins. Native Apress anchors equal the print folio
   (382/406 direct, remainder explainable); stable across the two EPUB copies and the PDF.
2. **`pdf_folio` (text-layer)** — same values, derived; use when no EPUB anchors exist.
3. **`pdf_physical`** — different scale (offset drifts 21–28); never mix with folios.
4. **Vendor reader pagination (ProQuest `tpg=394`)** — lowest trust; it counts reflow
   screens (394 ≠ 406 ≠ 448), reconcile only as overlay.

## Zotero round-trip (point 4)

Via local connector API (`/connector/saveItems`): book item imports cleanly; the `pages`
field has **no native slot on `itemType=book`** — Zotero demotes it to
`Extra: "Pages: 207"`. Consequence: a citation's page locator must come from the cite
request (CSL locator) or be parsed back out of Extra; alignment tables attach fine as
child notes. *Cleanup note: 2 test items tagged `pilot-221` remain in the library — the
local API refuses DELETE (428, connector-ID only); remove via Zotero UI.*

## ProQuest overlay

Documented skip — no session. The 394-vs-406 delta is already covered by the
trust-ordering proposal above; rerun the overlay when the session exists.

## Duplicate EPUB policy case

Second copy (different MD5) has an **identical marker set and identical marker IDs**
(`PB1…PB448` set-equal) → content-identical acquisition duplicate, container differs.
Confirms #220 "take both, dedupe on anchor fingerprint".

## Proposed standard report format (for #220 Stage 3)

Per-document JSON/CSV with exactly the columns of `alignment.csv`
(`epub_cfi | epub_marker_page | pdf_physical | pdf_folio | source_flags`) plus a
divergence section keyed by the classes above — every non-`ok` flag must map to one
classified cause with counts, never silently dropped.
