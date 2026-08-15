# Chunk Quality Assessment (quality gate before TC2)

**Report type:** Measurement report (dated) · **Date:** 2026-08-15 · **Data
basis:** DB after L8 test case 1 (16/16 completed) · **Scope:** 4,810 chunks,
4,810 dense embeddings (1024-dim), 26,353 entities, 55,537 mentions, 10,382
relationships, OpenSearch index. Original:
`axiom_ng/docs/benchmarks/CHUNK_QUALITY_ASSESSMENT.md`.

> **Character:** evaluative, read-only — pipeline and data left unchanged. This
> report documents the **system state as of 2026-08-15**.

## Part 1 — Quantitative distributions

### Chunk sizes (token_count)

| Metric | Value |
| --- | --- |
| Chunks | 4,810 |
| Min / P25 / Median / P75 / Max | 1 / 175 / **382** / 730 / 1,199 |
| Mean | 472.3 |
| Micro-chunks < 50 tokens | 445 (9.2%) |
| Monster-chunks > 1,500 tokens | **0** |

Micro-chunk autopsy (12 of 445): exclusively **heading anchors** — Marker emits
headings as own blocks, the chunker keeps them (e.g.
`#### **1 Rolle von Softwaresystemen…**`, 10 tokens). They are structurally
correct (consistent section_titles) but retrieval-low-value — a bare heading
answers no question.

### Chunk density per book

Range **0.50–1.38 chunks/page** — no pathological over-/under-chunking; the
spread is explained by sentence density and figure share of the books.

### Locator completeness

| Type | Count | Complete |
| --- | --- | --- |
| PDF page_span (physical + label) | 4,524 | **100%** |
| EPUB cfi_start/cfi_end | 286 | **100%** |
| Total | 4,810 | **100%** |

### Entity quality

Confidence histogram of the 55,537 mentions (GLiNER threshold ≥ 0.5):

| Bucket | 0.5 | 0.6 | 0.7 | 0.8 | 0.9 | 1.0 |
| --- | --- | --- | --- | --- | --- | --- |
| Mentions | 5,313 | 9,666 | 11,980 | 9,135 | 10,494 | 8,949 |

Top entities (excerpt): `unternehmen` (ORG, 1,686), `nachhaltigkeit` (CONCEPT,
1,164), `esg` (697), `deutschland` (LOC, 407), `csr` (333), `sap` (172),
`symrise` (132), `gri` (127), `vaude` (108) — **correct domain vocabulary** of a
CSR/ESG library, no monopoly garbage. Some noise from generic nouns as entities
(`companies` ORG ×141, `world` LOC ×108).

Mentions per entity: avg 2.12 · one-hit 71.6% (18,792) · stable ≥3 mentions
3,749. The one-hit share is normal for open NER over specialized books; the
stable core (14%) carries the graph.

### Relationship quality

10,382 relations, **100% with evidence chunk(s)**, 168 mREBEL types. Top:
`subclass_of` 2,548 · `part_of` 1,173 · `instance_of` 921 · `facet_of` 915 ·
`country` 409 · `author` 253 · long tail to `taxon_rank` ×1 (Wikipedia-schema
leak from mREBEL training).

Edges with **both** ends >1 mention: 3,183 / 10,236 assessable = **31.1%**.
`strength` is constant at 0.7 across all rows — mREBEL yields no confidence, we
persist a default → currently **no discrimination** over strength.

## Part 2 — Content spot checks

3 books (DE/EN/EPUB), 3 random chunks each inspected: texts substantively
complete (no mid-sentence cut in 9/9), section_titles hierarchy consistent. EPUB
chunks carry clean CFIs, PDF chunks page_span with roman/arabic labels.
Harmless curiosity: a 1,197-token chunk is a bibliography running text (locator
and section are correct).

### Locator counter-check against the original PDF (pymupdf)

3/3 **page-exact** (physical_page_start = 0-based pymupdf index), no deviation,
not even ±1. Two apparent misses in the first search pass were line breaks/
comma position in the needle, not locator errors.

### Entity spot check

Top-5 all plausibly typed. Random-5: `familienunternehmen` ORG ✓, `esg` CONCEPT
✓, `otto` PERSON ✓ (citation context), `ozone depletion potential` CONCEPT ✓,
`kennzahlen` CONCEPT ✓. **0 misclassifications of the "Table 3 as PERSON" type**
in the sample.

### Relationship spot check (5 random, evidence read)

- `grundbedürfnislohn ⊂ lohn`, `polyester ⊂ textiles`, `datenmanagement ⊂
  informationsverarbeitung` — plausible ✓
- `human cloning ⊂ embryonic stem cell` (both METHOD), `environmental ⊂ social`
  (siblings) — noise ✗

**3/5 plausible, 2/5 semantically off** — consistent with the 31% stable edges:
the raw graph is a candidate space, not finished knowledge.

## Part 3 — OpenSearch

- **Index integrity:** mapping `embedding: knn_vector(1024)` ✓, doc-count
  4,810 == chunks ✓. Spot-check 3 random docs: text-md5 **byte-identical** to
  the DB, token_count exact, embedding present, locator serialized correctly.
- **Sparse embeddings** live only in Postgres, **not** in the index — hybrid
  retrieval later needs a second index field (not a gate blocker).

### kNN search test (5 queries, DE+EN, BGE-M3 dense, k=5)

The query strings below are the literal English/German test inputs used in the
run; results are described in English.

| Query | Result |
| --- | --- |
| "What is CSR reporting and which standards exist for it?" (DE test query) | Top-5 all CSR books; **answered fully** |
| "Fundamentals of Stakeholder theory per Freeman" (DE test query) | 4/5 from *Stakeholder Management*; **textbook-exact** |
| "criticism of ESG ratings and rating divergence" | GRI criticism, third-party rating; **on-topic** |
| "Examples of sustainable supply chains in the textile sector" (DE test query) | supply-chain/supplier sections; **answered** (one low-value bibliography hit) |
| "How does the Life Cycle Assessment methodology work?" (DE test query) | 4/5 from *Life Cycle Management*; **bullseye** |

Score band 0.54–0.63 (cosine). Cross-lingual: an EN query finds DE books and
vice versa where content warrants. **Semantic garbage: zero hits in 25.**

## Part 4 — Conclusion & recommendation

| Dimension | Light | Rationale |
| --- | --- | --- |
| Chunk sizes | 🟢 | median 382, no monsters; 9.2% heading anchors |
| Locator | 🟢 | 100% coverage, 3/3 page-exact against original PDF |
| Entities | 🟢/🟡 | domain vocabulary correct, 0 misclassifications; 71.6% one-hits normal, generic nouns slightly noisy |
| Relationships | 🟡 | 100% evidence, but only 31% stable edges, 3/5 sample plausible, constant strength → candidate space, filterable |
| Retrieval | 🟢 | 5/5 queries on-topic, cross-lingual, no garbage — the hardest test passes |

**Recommendation: GO for TC2.** The core (dense retrieval over chunks with exact
locators) delivers precise, book- and section-accurate results; the knowledge
graph is a usable candidate space once stability/evidence filters apply — no
pipeline blocker.

**Findings by severity:** blockers: none.

Before/after TC2: (1) `strength` without discrimination — mREBEL confidence or
evidence-based strength, until then a mention-stability filter; (2) sparse
embeddings not in the index. Nice-to-have: merge heading micro-chunks; stop
generic nouns; downweight bibliography chunks.

Continue: [TC2 Parallel Test](tc2-parallel.md) · [Reports](../benchmarks.md)
