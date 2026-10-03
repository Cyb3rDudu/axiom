# Citation Granularity — Decision Memo (#194 part 2)

**Question:** keep current chunk sizes + the per-paragraph page map (part 1), or resize
chunks (page-aligned / smaller) for exact-page citations? **Owner hypothesis:** the map
delivers sentence-level precision without sacrificing the retrieval quality the current
sizes bought.

## Measurements

**Book decomposition — healed Altenburger (6HYURPZH, current generation, print truth
proven):** 249 chunks, 83% folio_verified. Page-span distribution: 1-page 39%, 2-page
37%, 3-page 15%, 4-page 3%, 6-page 1 (mean span 1.85 pages). Tokens: median 401, mean
469, max 1,189 (cap 1,200).

**Corpus (33,017 numerically spanned active chunks):** 53% single-page, mean span 1.70,
mean 426 tokens, 14.0M tokens total.

**Page-aligned simulation (Altenburger):** splitting every chunk at its page boundaries
yields 417 fragments (×1.84), mean 247 tokens, 15 fragments under the 50-token merge
floor. Corpus extrapolation: ~×1.70 → ~56,100 chunks — **+70% chunk count, embedding
compute, and index size**, plus a full re-chunk wave to realize.

**Smaller-cap variant (600 tokens):** only 27% of Altenburger chunks exceed 600 today —
~×1.27 chunks, no precision gain beyond the map (spans remain spans).

## Citation precision — the decisive comparison

| Approach | Citation granularity | Retrieval risk | Cost |
|---|---|---|---|
| Today (span only) | mean 1.85 pages (book) / 1.70 (corpus); 61% of Altenburger chunks cite spans | — (measured baseline) | — |
| **Page map (part 1)** | **exact page per hit position**, independent of chunk size | **zero** — additive locator field, search/ranking untouched | ~zero (bytes per chunk; rides the next generation) |
| Page-aligned resize | exact page by construction — **same precision as the map** | baseline invalidated; smaller context fragments the answer-span class the size tuning guards; full reindex + gold_suite_v21 re-proof required | +70% chunks/embeddings/index |
| 600-token resize | spans remain (multi-page chunks persist) — **worse precision than the map** | same invalidation + re-proof | +27% chunks |

The map strictly dominates the resize options on precision-per-risk: it produces the
identical citation outcome (exact page) without touching a single ranking-relevant byte.

## Retrieval baseline at stake

The R7/R7b measurements (gold_suite_v21, 52 entries, reproducible via
`cmd/retrieval-bench`): P@1 0.615, hit@5 0.808, MRR 0.702, hit@10 0.865 at CURRENT
chunk sizes, plus the hygiene flip-sonde suite (K1–K6). The tuned config won on these
measurements; any resize resets the evidence to zero and must re-earn the numbers on a
fully reindexed corpus — a W9-scale effort bought for nothing the map doesn't already
deliver.

## Recommendation

**Keep sizes + page map.** Concretely:

1. Part 1 (merged with this analysis branch) ships as-is: additive `paragraph_pages` in
   the locator, `/api/passage/{id}/page?at=N` derivation. The proven test case
   (Altenburger chunk 04881089, S.9 sentence → page 9) is pinned at chunker and API
   level.
2. Maps materialize with the NEXT natural generation (no dedicated rechunk for maps —
   pre-#194 chunks keep the honest span envelope; `/page?at=` answers 404-with-span for
   them).
3. Resize stays a benchmark-gated option (full gold_suite_v21 + perq re-proof on a
   reindexed corpus) — with the measured +70% cost and zero precision upside over the
   map, the burden of proof lies with anyone proposing it.

## Honest caveats

- Map coverage = marker-pagination coverage: books whose PDFs yield no page markers
  carry no map (span remains the envelope). The Dubs-class OCR rebuilds DO carry
  markers per leaf.
- The page-aligned corpus figure is an extrapolation from span arithmetic (proportional
  token split, 50-token merge floor); the book-level simulation is exact to that model.
- The 83% folio_verified share on Altenburger reflects print-truth healing; unhealed
  books cite PDF indices until their sources are truth (#188 wave handles those).
