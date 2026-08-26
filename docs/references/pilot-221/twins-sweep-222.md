# Twins Sweep #222 — marker=folio across the library + derived page-lists

Follow-up to pilot #221 (n=1 → n=11). Tooling: `axiom_ng_runner/scripts/twins_sweep.py`
(uses `four_point_pilot.py`, extended for two more anchor dialects and positional PDF
folio extraction). Run with `/opt/axiom/runner/current/env/bin/python`.

Enumeration via Zotero local API: documents with ≥1 epub and ≥1 pdf attachment —
**10 twins in Zotero** (+ Databricks pair from #221 = 11 total).

## Aggregated results

### Anchored pairs (marker dialects)

| Book | Publisher | Dialect | Markers | Folio verification |
|---|---|---|---|---|
| Databricks (#221) | Apress/Springer | `epub3_pagebreak` | 406 | **383/406 verified** joins vs PDF folios |
| Bieger 2021 | Springer | `id_page_n` | 197, monotone+unique | PDF sibling is an e-book PDF **without printed folios** → folio join impossible (3/197 via stray band numbers). Marker range 1..197 ≈ print pagination; trust = native anchors, unverifiable against this PDF |
| Jossé 2018 (no PDF twin — dialect fixture only) | — | `id_page_n` (`class="page"` present too, id wins) | 228, range 1..228 | n/a |

Dialect coverage: `epub:type="pagebreak"`, `id="page_N"` (covers Bieger **and** Jossé —
one rule, two publishers), `class="page"` fallback. Manifest hrefs escaping via `../`
(Jossé) required zip-path normalization. The Jossé fixture is regenerable via
`four_point_pilot.py --epub <Zotero storage FFMTJA3S copy> ...` (no PDF twin, harvest only).

### Unanchored twins — derived + injected page-lists (`derived_from_sibling`)

| Book | PDF folios | Anchored | Injected EPUB round-trip |
|---|---|---|---|
| D'heur 2013 | 296 | 279 (94%) | 276/279 |
| Altenburger 2013 | 242 | 214 (88%) | 213/214 |
| Fifka 2014 | 239 | 213 (89%) | 210/213 |
| Schulz 2014 | 369 | 306 (83%) | 302/306 |
| Hentze 2014 | 168 | 151 (90%) | 149/151 |
| Burckhardt 2014 | 197 | 160 (81%) | 160/160 |
| Schäfer 2014 | 354 | 309 (87%) | 308/309 |
| Geursen 2022 | 511 | 380 (74%) | 378/380 |
| Abeln 2019 | 275 | 250 (91%) | 247/250 |

Working copies with inline `doc-pagebreak` spans + Adobe `page-map.xml`:
`~/Downloads/epub-injected-222/` (`*.injected.epub`; originals untouched).
Every injected copy was re-harvested and re-joined against the PDF folios —
**~99% of injected markers verify** (residual = boundary/off-by-one, same classes as #221).

Matching hardening that made this work (all reusable for Stage 1 crawler work):
- ASCII-fold umlauts identically on both sides (naive non-ASCII stripping splits German words)
- tolerate glued PDF words (`ÜberdieAutoren`) via prefix-index + substring containment
- filter running-head/footer boilerplate by per-document page-frequency (>30% pages)
- anchor pages independently, then enforce monotonicity via LIS — a cursor walk cascades

## Per-publisher verdict

- **Apress/Springer (Databricks, Bieger, CSR series)**: marker=folio holds wherever
  verifiable; unanchored Springer EPUBs still derive cleanly from the sibling PDF.
- **No diverging publisher found** in this sample; the only "divergence" class is
  e-book-PDF-without-folios (Bieger), which is a missing-ground-truth case, not drift.
- Geursen (74%) is the weakest derivation — longest book (547 phys. pages) with
  TOC-style headers; flagged, page_source stays `derived_from_sibling`.

## page_source implication for #220

Confirmed trust ladder at n=11: `print` (native anchors) > `pdf_folio` (text layer) >
`derived_from_sibling` (injected from sibling, verified round-trip, never native) >
`pdf_physical` > vendor reflow. Derived maps must carry their coverage (anchored/total)
as first-class metadata — 74–94% here, not 100%.

## Stage-1 handoff (documented)

Strang A's Stage-1 verification logic did not exist at sweep time — the round-trip
verification above (re-harvest + join) is the interim check; hand the injected EPUBs
to Stage 1 when the runner-side verifier lands.
