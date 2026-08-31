---
name: axiom
description: Query the local axiom RAG (retrieval-augmented library over the Zotero corpus) via its HTTP API. Use when a task needs evidence, quotes, citations, or facts from the ingested books/PDFs — searching literature, finding citable passages, checking what a source says, or grounding an answer in the corpus. Explains endpoints, request/response shapes, the locator trust ladder, and citation rules.
---

# axiom RAG

The axiom RAG indexes the local Zotero library (books, PDFs, EPUBs) and serves chunk-level retrieval with citation-grade provenance. It is a **retrieval system, not an oracle**: every claim you make from it must carry its locator, and the locator's trust level dictates how boldly you may cite.

## Access

```
Base URL:  http://127.0.0.1:8011   (LAN: http://192.168.0.107:8011)
Health:    GET /api/health         → {"status":"ok","build":"axiom-ng v…"}
```

## Endpoints

### Search (the main tool)

```
POST /api/search
{"query": "VAUDE Nachhaltigkeit Vorbildfunktion", "top_n": 5}
optional: {"filters": {"document_ids": ["<uuid>"]}}
```

Response:

```
{
  "arms": {"dense": true, "bm25": true},   // recall arms used
  "reranked": true,                         // cross-encoder reranking applied
  "took_ms": 11075,
  "hits": [{
    "chunk_id": "…",
    "text": "## 4\n\nErfolge und Vorbildfunktion\n…",
    "score": 0.87,                          // RRF score (or rerank score when reranked)
    "source": {…},                          // document metadata (title, authors, year)
    "locator": {…},                         // provenance — see trust ladder
    "section": ["Kapitel 4", "4.2 Erfolge"]
  }]
}
```

### Passage / Knowledge Graph

```
GET /api/passage/{chunk_id}         → full chunk with source + locator
GET /api/kg/entities                → knowledge-graph entities
GET /api/kg/relations               → extracted relations
GET /api/kg/entities/{id}/neighbors → entity neighborhood
```

## Practical rules

1. **Latency is real**: dense+rerank on CPU takes 10–25 s per query. Batch your questions; do not fire queries in a loop when one well-formed query answers all.
2. **Query in the document's language**: German corpus → German queries, English books → English queries. Mixed works but ranks worse.
3. **Scoping**: get `document_ids` from a broad search first, then use `filters.document_ids` to interrogate one book.
4. **Search is text-only**: images extracted from documents ride along as artifacts but are pixel-blind — you cannot search for what a chart *shows*, only for its caption/surrounding text (see issue #230).
5. **Quotes come from `text`, not from memory**: always re-read the chunk text before quoting. Never paraphrase a hit you did not read.

## The locator trust ladder (citation discipline)

Every hit carries `locator` with `kind`, `label`, `chapter`, and — decisive — `page_source`. The trust level is a **contract**: it tells you the strongest citation you are allowed to make.

### EPUB sources

| page_source | meaning | citation rule |
|---|---|---|
| `print_verified` | Print pages proven **inside the book**: native page anchors verified against the book's own printed TOC | Cite the page: "(Yip et al. 2026, S. 307)" |
| `print_unverified` | Page anchors exist but nothing proved they are print folios | Cite section + page **with reservation**: "S. 41 (unverifizierte Seitenangabe)" — or the section alone |
| `derived_from_sibling` | Pages injected by aligning the PDF twin; ~84 % within ±1, known drift cases up to +8 | Cite section first, page as approximation: "Kap. 15 (S. ~184)" |
| `none` | No trustworthy page data (no anchors, or the monotony guard refused) | Cite **section only**: "(Yip et al. 2026, Abschnitt ‚What Is a Lakehouse?')" — never assert a page, even if the book's index lists one |

### PDF sources

| page_source | meaning | citation rule |
|---|---|---|
| `folio_verified` | Printed folio read from the text layer and verified in a run | Cite the printed page |
| `pdf_label_sane` | Page label plausible | Page cite acceptable |
| `physical_only` | Only a PDF index exists — label renders as **"PDF-S. N"**, never a print page | Cite as PDF page explicitly, or the section |
| `blind` | Scanned, no text layer (needs OCR rebuild) | Treat locator as unreliable; prefer section |

### Worked example (real case, 2026-08-31)

Query for the Lakehouse definition returned the section "What Is a Lakehouse?" with `page_source: none`. The book's index says page 3 — **the locator does not confirm it**. Correct scholarly behavior: cite the section, name the chapter, state that no verified print page exists. An external agent (chatty) did exactly this and additionally recommended the primary source (Armbrust et al., CIDR 2021) that the book itself references. That is the intended citation culture: **the system's honesty propagates to your footnote.**

## Anti-patterns

- Citing a page number when `page_source` is `none`/absent
- Treating `PDF-S. 12` (physical index) as a print page
- Quoting the query-snippet instead of reading the full `text`
- Inferring "the book says X" from one chunk — check `section` context and, when in doubt, the passage endpoint
- Hammering /api/search in a tight loop (CPU-bound; one good query beats five lazy ones)
