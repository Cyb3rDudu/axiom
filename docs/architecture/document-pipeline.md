# Document Processing Pipeline

This page describes the full pipeline that a document traverses from upload to searchable, graph-indexed content.

## Pipeline Overview

```
Upload (frontend/CLI)
  |
  v
Queue (PostgreSQL, status = "queued")
  |
  v
Background Document Processor (polls every 5s)
  |
  +-- 1. Validate & copy file to processor directory
  +-- 2. Extract initial text (header/footer for PDFs, first N chars for others)
  +-- 3. LLM metadata extraction (title, authors, year, DOI, ISBN, ...)
  +-- 4. Metadata enrichment (CrossRef, OpenLibrary, OpenAlex)
  +-- 5. Convert to Markdown (pdf_worker subprocess for PDF, python-docx for Word)
  +-- 6. Retry metadata extraction with markdown content if initial attempt failed
  +-- 7. Extract page labels (3-tier fallback for PDFs)
  +-- 8. Chunk content (token-based, structure-aware, with page tracking)
  +-- 9. Embed chunks via GPU worker RPC (BGE-M3 dense + sparse)
  +-- 10. Store in pgvector + index in OpenSearch
  +-- 11. Build knowledge graph:
  |       a. Sequential chunk relationships
  |       b. GLiNER entity extraction via GPU worker RPC
  |       c. Co-occurrence relationships (min 2 shared chunks)
  |       d. Kill GPU worker subprocess (model_cache.unload_all)
  |       e. relation_worker subprocess (mREBEL, exits after use)
  +-- 12. Embed and store images (CLIP, if enabled)
  +-- 13. Fallback metadata enrichment (if LLM-only sources)
  |
  v
Document ready (status = "completed")
```

!!! info "Subprocess Isolation"
    GPU-intensive steps run in isolated subprocesses. PDF conversion runs in `pdf_worker` (loads Marker, exits after use). Embedding and entity extraction use the shared GPU worker over Unix socket RPC. Relation extraction runs in `relation_worker` (loads mREBEL, exits after use). See [GPU Worker Architecture](gpu-worker.md) for the full design.

## Step Details

### 1-2. File Handling and Initial Text

The uploaded file is copied to the processor directory with a `{doc_id}_{filename}` naming convention. For PDFs, initial text is extracted from the first and last pages using PyMuPDF to provide the metadata extractor with title page and bibliography information.

### 3. LLM Metadata Extraction

The `MetadataExtractor` sends the initial text to an LLM with a structured JSON schema requesting:

- `document_type` -- paper, book, legal, institutional, web, other
- `title`, `authors`, `publication_year`
- Type-specific fields: `doi`, `isbn`, `journal_or_source`, `publisher`, `edition`, `chapters`
- `description` / abstract, `keywords`

Uses the [JSON fallback system](json-fallback.md) to work with any LLM provider.

### 4. Metadata Enrichment

After LLM extraction, the `enrich_metadata()` pipeline runs external lookups to validate and supplement the LLM's output:

1. **Identifier detection** -- regex patterns scan the text for DOI (`10.xxxx/yyyy`), ISBN (10 or 13 digit), and arXiv IDs.
2. **CrossRef** (for DOI) -- looks up the DOI via the CrossRef API. Returns authoritative title, authors, year, journal, publisher, and ISBN.
3. **OpenLibrary** (for ISBN) -- looks up the ISBN. Returns title, resolved author names, publisher, page count.
4. **OpenAlex** (for title) -- title-based search with similarity scoring (threshold 0.6). Returns authors, year, journal, DOI, and cited-by count.

Each enrichment source is recorded in `metadata_sources` (e.g., `["llm", "crossref"]`) so the system can track provenance. External sources override LLM-extracted fields when they provide higher-confidence data.

If the initial LLM extraction failed (e.g., image-only cover page), a **retry** runs using the first portion of the converted markdown content.

### 5. Document Conversion

- **PDF** -- converted in a short-lived `pdf_worker` subprocess that loads Marker with `paginate_output=True` for page boundary markers. The subprocess exits after conversion, freeing all Marker GPU memory (~2-4 GB). Output is markdown text and extracted images, communicated via JSON on stdout.
- **Word** -- python-docx extracts text and structure.
- **Markdown** -- read directly.

### 7. Page Label Extraction

For PDFs, `extract_page_labels()` determines logical page numbers using a 3-tier fallback:

| Tier | Method | Details |
|---|---|---|
| 1 | PDF embedded labels | Publisher metadata. Used when >50% of pages have labels. |
| 2 | Header/footer parsing | Parses standalone numbers from top/bottom 8% of pages. Includes outlier detection and sequential validation. Extrapolates using median offset. |
| 3 | Physical + 1 | Fallback: page index + 1. |

The resulting `page_label_map` is passed to the chunker so each chunk stores the correct display page numbers (`page_start`, `page_end`).

### 8. Chunking

The `Chunker` uses token-based, structure-aware splitting:

- **Token budget** -- default 512 tokens per chunk, 64 token overlap, 50 token minimum.
- **Section boundaries** -- new chunks start at markdown headings to preserve topic coherence.
- **Hierarchical title padding** -- each chunk records its ancestor heading hierarchy for embedding context.
- **Recursive semantic splitting** -- oversized chunks are split using a separator hierarchy: paragraph breaks, line breaks, sentence ends, clause breaks, then binary midpoint split.
- **Page tracking** -- Marker pagination markers (`{N}---...`) are parsed and mapped to logical page labels via the `page_label_map`.
- **Image references** -- markdown image syntax is detected and stored in chunk metadata.

### 9-10. Embedding and Indexing

Chunks are embedded via the GPU worker's `embed_chunks` RPC method, which calls **BGE-M3** to produce both dense (1024-dim) and sparse (30,000-dim) vectors. The doc-processor connects to the backend's GPU worker over a shared Unix socket (see [GPU Worker Architecture](gpu-worker.md)). Chunks are stored in:

- **pgvector** -- dense embeddings with HNSW index for approximate nearest neighbor search.
- **OpenSearch** -- BM25 fulltext index for lexical matching (when enabled).

### 11. Knowledge Graph Construction

See [RAG and Knowledge Graph](../user-guide/documents/rag-knowledge-graph.md) for full details. The pipeline:

1. Builds sequential relationships between adjacent chunks.
2. Runs GLiNER entity extraction on each chunk via GPU worker RPC.
3. Builds co-occurrence relationships (entities appearing in 2+ shared chunks).
4. Kills the GPU worker subprocess (`model_cache.unload_all()`) to free all VRAM.
5. Spawns `relation_worker` subprocess -- loads mREBEL, extracts triples, exits.
6. Stores triples as typed entity relationships.

### 13. Fallback Metadata Enrichment

After all processing, if the document's `metadata_sources` contains only LLM sources (no external API matches), AXIOM runs a second enrichment attempt using a text sample from the full markdown content. This catches cases where identifiers (DOI, ISBN) appear in the body or bibliography rather than the first pages.

An auto-generated description is created when no abstract was found by the LLM.

## Error Handling

- Each major step is wrapped in try/except blocks. Non-fatal failures (image processing, knowledge graph, OpenSearch indexing) are logged as warnings but do not fail the document.
- If the entire processing fails, the document is marked as `failed` and a cleanup routine removes orphaned chunks, entities, and relationships.
- The worker loop continues polling for the next queued document.

## Next Steps

- [VRAM Management](vram-management.md) - GPU memory sharing between models
- [RAG and Knowledge Graph](../user-guide/documents/rag-knowledge-graph.md) - Retrieval pipeline details
- [Architecture Overview](index.md) - System design and components
