# L8 Chunk Quality Assessment (Quality Gate before TC2)

**Date:** 2026-08-15 · **Data basis:** axiom_db after L8 Test Case 1 (16/16 completed)
· **Scope:** 4.810 chunks, 4.810 dense embeddings (1024-dim), 26.353 entities,
55.537 mentions, 10.382 relationships, OpenSearch index `axiom-ng-chunks-v1`
· **Character:** assessing, read-only — pipeline and data left unchanged.

All figures are reproducible against `axiom_db` (PSQL access as in
MASS_CHUNKING_BENCHMARK.md); the core queries are given in the respective
sections.

---

## Part 1 — Quantitative Distributions

### 1.1 Chunk Sizes (token_count)

| Metric | Value |
| --- | --- |
| Chunks | 4.810 |
| Min / P25 / Median / P75 / Max | 1 / 175 / **382** / 730 / 1.199 |
| Mean | 472,3 |
| Micro-chunks < 50 tokens | 445 (9,2 %) |
| Monster chunks > 1.500 tokens | **0** |

Per book, medians range between 150 (Stakeholder-Management) and
965 (Nachhaltige Nicht-Nachhaltigkeit); maxima sit at ~1.199 (chunk limit).

**Micro-chunk autopsy** (12 random of the 445): exclusively
**heading anchors** — the marker emits headings as their own blocks, and the
chunker keeps them as standalone chunks (e.g. `#### **1 Rolle von
Softwaresystemen für das NH-Management**`, 10 tokens). 31 of them are nearly
empty (< 20 characters), none without section_titles. They are structurally
correct (section_titles consistent), but low-value for retrieval — a bare
heading answers no question.

### 1.2 Chunk Density per Book

| Book | Pages | Chunks | C/page | | Book | Pages | Chunks | C/page |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| CSR Mythen | 227 | 266 | 1,17 | | Institutionelle Anleger | 371 | 460 | 1,24 |
| CSR und Finance | 407 | 469 | 1,15 | | Nachh. Nicht-Nachhaltigkeit | 347 | 174 | 0,50 |
| CSR Innovationsmanagement | 249 | 249 | 1,00 | | Nachhaltiges Management | 773 | 754 | 0,98 |
| CSR und Reporting | 258 | 236 | 0,91 | | Perspektiven Wirtschaftswiss. | 382 | 300 | 0,79 |
| CSR Value Chain Mgmt | 308 | 282 | 0,92 | | Stakeholder-Management | 178 | 181 | 1,02 |
| Demystifying ESG (EPUB) | – | 252 | – | | Adventure of Sust. Perf. | 290 | 183 | 0,63 |
| ESG and Sustainability | 124 | 171 | 1,38 | | ESGBS (EPUB) | – | 34 | – |
| ESG Investing (EN) | 359 | 404 | 1,13 | | LCM | 498 | 395 | 0,79 |

Range 0,50–1,38 — no pathological over-/under-chunking; the spread is
explained by the books' sentence density and figure share.

### 1.3 Locator Completeness

| Type | Count | Complete |
| --- | --- | --- |
| PDF page_span (physical + label) | 4.524 | **100 %** |
| EPUB cfi_start/cfi_end | 286 | **100 %** |
| Total | 4.810 | **100 %** |

74 chunks sit on physical_page 0 with label `C1` (PDF cover sheets, correct).
Both EPUBs (Demystifying: 252, ESGBS: 34) carry CFIs.

### 1.4 Entity Quality

Confidence histogram of the 55.537 mentions (GLiNER threshold ≥ 0,5 applies):

| Bucket | 0,5 | 0,6 | 0,7 | 0,8 | 0,9 | 1,0 |
|---|---|---|---|---|---|---|
| Mentions | 5.313 | 9.666 | 11.980 | 9.135 | 10.494 | 8.949 |

Top entities (excerpt): `unternehmen` (ORG, 1.686), `nachhaltigkeit`
(CONCEPT, 1.164), `esg` (697), `deutschland` (LOC, 407), `csr` (333), `sap`
(172), `symrise` (132), `gri` (127), `vaude` (108) — this is the **correct
domain vocabulary** of a CSR/ESG library, not monopoly garbage. Slight noise
from generic nouns as entities (`companies` ORG ×141, `world` LOC ×108).

Mentions per entity: Ø 2,12 · one-hit 71,6 % (18.792) · stable ≥3 mentions:
3.749. The one-hit share is normal for open NER over textbooks; the stable
core (14 %) carries the graph.

Type distribution: CONCEPT 10.135 · ORG 6.363 · PERSON 6.060 · LOCATION 1.970 ·
WORK 1.244 · METHOD 581.

### 1.5 Relationship Quality

10.382 relations, **100 % with evidence chunk(s)**, 168 mREBEL types.
Top: `subclass_of` 2.548 · `part_of` 1.173 · `instance_of` 921 · `facet_of`
915 · `country` 409 · `author` 253 · … long tail down to `taxon_rank` ×1
(Wikipedia schema bleed-through from mREBEL training).

Edges with **both** endpoints >1 mention: 3.183 / 10.236 assessable =
**31,1 %**. `strength` is constant 0,7 across all rows — mREBEL delivers no
confidence, we persist a default → currently **no discrimination** possible
via strength.

---

## Part 2 — Content Spot Checks

Books: **CSR und Reporting** (DE) · **ESG Investing** (EN) · **Demystifying
ESG** (EPUB). 3 random chunks each (token_count > 100) inspected — sample
of 9:

- DE/EPUB/EN mixed, 178–1.197 tokens, texts content-complete (no sentence
  cut off midway in 9/9), section_titles hierarchy consistent throughout
  (book → chapter → section).
- EPUB chunks carry clean CFIs (`epubcfi(/6/14!/4/66)`), PDF chunks
  page_span with Roman/Arabic labels.
- A curiosity without consequences: one 1.197-token chunk in the
  Demystifying EPUB is a bibliography section (references running text) —
  locator and section correct, content-wise pure reference material.

### Locator counter-check against the original PDF (pymupdf)

| Chunk | Book | physical | Needle | Hit |
| --- | --- | --- | --- | --- |
| #310 | ESG Investing | 311 | "Travel & Training Fund" | idx 310/311 ✓ |
| #214 | CSR und Reporting | 238 | "Entwicklungsbereich und Innovationstreiber" | idx 238 ✓ exact |
| #160 | ESG Investing | 141 | PROMESA passage | idx 141 ✓ exact (word-for-word) |

3/3 **page-exact** (physical_page_start = 0-based pymupdf index), no
deviation, not even ±1. Two apparent misses in the first search round were
line breaks/comma positions in the needle, not locator errors.

### Entity spot check (5 top + 5 random)

Top 5 all plausibly typed (see above). Random 5: `familienunternehmen` ORG ✓,
`esg` CONCEPT ✓, `otto` PERSON ✓ (citation context), `ozone depletion
potential` CONCEPT ✓, `kennzahlen` CONCEPT ✓. **0 misclassifications of the
type „Tabelle 3" as PERSON in the spot check.**

### Relationship spot check (5 random, evidence read)

- `grundbedürfnislohn ⊂ lohn` — evidenced ✓ plausible
- `polyester ⊂ textiles` — evidenced ✓ plausible
- `datenmanagement ⊂ informationsverarbeitung` — evidenced ✓ plausible
- `human cloning ⊂ embryonic stem cell` — noise ✗ (both METHOD, no subclass
  relation)
- `environmental ⊂ social` — noise ✗ (siblings, not subclasses)

**3/5 plausible, 2/5 semantically off** — consistent with the 31 % stable
edges: the raw graph is a candidate space, not finished knowledge.

---

## Part 3 — OpenSearch

### 3.1 Index Integrity

Mapping: `embedding: knn_vector(1024)` ✓, `text`, `token_count`, `locator`
(nested with page_span+cfi fields), `section_titles`, IDs. Doc count
4.810 == chunks ✓. Spot check of 3 random docs: text-md5 **bytewise
identical** to the DB, token_count exact, embedding present, locator
correctly serialized — 3/3.

Sparse embeddings live only in Postgres
(`processing_chunk_sparse_embeddings`), **not** in the index — hybrid
retrieval will need a second index field for that later (noted, not a gate
blocker).

### 3.2 kNN search test (5 queries, DE+EN, BGE-M3 dense)

Encoding: `BGEM3FlagModel.encode` on the query — BGE-M3 dense is symmetric
and instruction-free, the same encoder as for the passages (a documented
choice). `k=5`, pure knn against `axiom-ng-chunks-v1`.

**Q1 „Was ist CSR-Reporting und welche Standards gibt es dafür?"** — Top 5
all CSR books, 3× from *CSR und Reporting* itself: G4 reporting standard,
IDW PS 821 (assurance standard for sustainability reports), ISO 26000, EU
strategy. → **fully answers the question.**

**Q2 „Grundlagen der Stakeholder-Theory nach Freeman"** — 4/5 from
*Stakeholder-Management…* (sections 2.1–2.5, „Freemans Ansatz…"), 1× ESG
book („2.6.5 Stakeholder Theory"). → **textbook-exact.**

**Q3 „criticism of ESG ratings and rating divergence"** — GRI standards
criticism, third-party ESG rating section, reporting standards fragments.
→ **on-topic** (spread okay, scores 0,55–0,59).

**Q4 „Beispiele für nachhaltige Lieferketten in der Textilbranche"** —
supply chain/supplier management sections, „38.4.2 Transparente, faire und
saubere Lieferkette" (outdoor/textile!). Hit #1 is a bibliography chunk
(low-value, harmless). → **answered.**

**Q5 „Wie funktioniert die Methodik des Life Cycle Assessment?"** — 4/5
from *Ganzheitliches Life Cycle Management*: „Ökologische
Lebensweganalyse", „Zieldefinition", „Vorgehensweise". → **Bullseye.**

Score band 0,54–0,63 (cosine). Cross-lingual works: EN queries find DE
books and vice versa, wherever the corpus offers the content. **Semantic
garbage: zero hits in 25 results.**

---

## Part 4 — Conclusion

| Dimension | Traffic light | Rationale |
| --- | --- | --- |
| Chunk sizes | 🟢 | Median 382, no monsters; 9,2 % heading anchors (structurally correct, low retrieval value) |
| Locator | 🟢 | 100 % coverage (page_span + CFI), 3/3 page-exact against the original PDF |
| Entities | 🟢/🟡 | Domain vocabulary correct, 0 misclassifications in spot check; 71,6 % one-hits normal, generic nouns slightly noisy |
| Relationships | 🟡 | 100 % evidence, but only 31 % stable edges, spot check 3/5 plausible, strength constant 0,7 → candidate space, filterable |
| Retrieval | 🟢 | 5/5 queries on-topic, correct books/sections, cross-lingual, no garbage — passes the acid test |

### Go/No-Go recommendation: **GO for TC2**

The data is **good enough** for RAG retrieval: the core (dense retrieval
over chunks with exact locators) delivers precise results faithful to book
and section. The knowledge graph is a usable candidate space as soon as
stability/evidence filters apply at query time — not a pipeline blocker.

### Findings list by severity

**Blockers:** none.

**Address before/after TC2 (non-blocking):**

1. `strength` without discrimination (constant 0,7) — derive mREBEL
   confidence or evidence-based strength; until then, filter graph querying
   by mention stability of the endpoints (the 31 % stable edges are the
   core).
2. Sparse embeddings not in the OS index — add field + population for
   hybrid retrieval once search quality needs to grow beyond kNN.

**Nice-to-have (later):**
3. Merge heading-only micro-chunks (9,2 %) with the following piece — better
   retrieval yield per index doc.
4. Generic nouns as entities (`companies`, `world`) — stoplist or
   minimum-length/mention threshold at entity onboarding.
5. Bibliography chunks — optionally downweight via a section signal.

— *Generated in the L8 quality gate work package; figures verifiable
against axiom_db as of 2026-08-15.*
