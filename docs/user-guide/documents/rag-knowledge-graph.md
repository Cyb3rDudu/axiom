# RAG and Knowledge Graph

AXIOM's Retrieval-Augmented Generation (RAG) pipeline goes beyond basic vector search by combining semantic embeddings, BM25 fulltext indexing, a PostgreSQL-native knowledge graph with NER and relation extraction, cross-encoder reranking, and multimodal image search. Together these layers ensure that chat responses, research missions, and writing tasks are grounded in the actual content of your uploaded documents.

## Document-Aware RAG Chat

When you upload documents to AXIOM, they are automatically chunked, embedded, and indexed. Every subsequent chat message is searched against your document library so the LLM can ground its responses in retrieved passages.

### How It Works

1. You send a message in the chat interface.
2. AXIOM embeds your query with BGE-M3 (dense + sparse vectors).
3. A **hybrid search** runs in parallel: pgvector dense similarity + OpenSearch BM25 fulltext.
4. Results are merged using **Reciprocal Rank Fusion** (RRF, k=60).
5. If the knowledge graph is enabled, **graph-enhanced retrieval** expands the candidate set using query-entity extraction and entity relationship traversal (see below).
6. Up to **8 relevant chunks** are reranked by a cross-encoder (BGE-reranker-v2-m3).
7. The top chunks are injected into the LLM prompt alongside a document library summary.
8. The LLM generates a response with **source attribution** referencing the original documents with page numbers.

### Key Details

- **No setup required** -- works automatically once documents are uploaded and processed.
- **Hybrid search** -- combines dense embeddings (1024-dim) with OpenSearch BM25 fulltext, merged via RRF. When OpenSearch is unavailable, falls back to pgvector-only with sparse embeddings.
- **Reranking** -- BGE-reranker-v2-m3 cross-encoder scores all candidates (seeds + graph-discovered + query-entity matches) before final selection.
- **Document group scoping** -- select a document group in the chat interface to limit retrieval to a specific collection.

!!! tip
    For the best results, upload well-structured documents with extractable text. Scanned image-only PDFs will produce lower-quality chunks unless LLM-enhanced OCR is enabled.

## Knowledge Graph

The knowledge graph is a PostgreSQL-native entity and relationship layer that enriches standard vector retrieval. Entities and relations are extracted from every document using dedicated ML models, allowing AXIOM to discover connections that pure embedding similarity might miss.

### Entity Extraction (GLiNER)

Entities are extracted using **GLiNER** (`urchade/gliner_multi-v2.1`), a zero-shot named entity recognition model that supports custom entity types at inference time without fine-tuning.

**Key properties:**

- **Zero-shot NER** -- entity types are defined as plain-text labels, not trained classes. New types can be added without retraining.
- **Multilingual** -- a single model handles German, English, French, Spanish, and Portuguese documents.
- **Custom academic entity types:**

| GLiNER Label | Internal Type | Description |
|---|---|---|
| `person` | PERSON | People, researchers, historical figures |
| `organization` | ORGANIZATION | Institutions, companies, government bodies |
| `location` | LOCATION | Countries, cities, geographic regions |
| `concept` | CONCEPT | Theories, abstract ideas, research topics |
| `book or journal` | WORK | Published works, journals, cited titles |
| `research method` | METHOD | Methodologies, techniques, frameworks |

- **Confidence threshold** -- default 0.45; entities below this score are discarded.
- **Noise filtering** -- strips "et al." suffixes, skips generic words (e.g., "firms", "government"), deduplicates by (text, type) pairs.

**Fallback:** If GLiNER is unavailable (e.g., CPU-only deployment without the `gliner` package), AXIOM falls back to **language-aware spaCy NER**. The correct spaCy model (`de_core_news_lg` or `en_core_web_lg`) is loaded based on automatic language detection using stopword frequency analysis.

### Relation Extraction (mREBEL)

Relations between entities are extracted using **mREBEL** (`Babelscape/mrebel-large`), a multilingual relation extraction model based on BART that produces structured (subject, predicate, object) triples.

**Key properties:**

- **Multilingual** -- supports 18 languages including German and English.
- **Triple format** -- each extraction produces: head entity, head type, tail entity, tail type, and relation predicate.
- **Beam search** -- generates multiple candidate sequences (default 3 beams) per chunk, with deduplication across beams.
- **Input handling** -- chunks are truncated to 1,500 characters / 512 tokens for mREBEL input.
- **VRAM management** -- mREBEL requires approximately 2.4 GB of GPU memory. It is loaded on demand after all other GPU models (embedder, reranker, GLiNER, Marker) are unloaded, and is unloaded immediately after extraction completes. See [VRAM Management](../../architecture/vram-management.md) for details.

### Database Schema

The knowledge graph is stored in three PostgreSQL tables:

| Table | Contents | Populated By |
|---|---|---|
| `document_entities` | Entity text, type, canonical form, description | GLiNER (or spaCy fallback) |
| `entity_chunk_occurrences` | Links entities to the chunks they appear in | GLiNER, mREBEL |
| `entity_relationships` | Typed relationships between entities with strength scores | mREBEL triples, co-occurrence analysis |

### Graph-Enhanced Retrieval

When `ENABLE_GRAPH_RETRIEVAL` is enabled (the default), retrieval follows a multi-phase process:

1. **Vector + BM25 hybrid search** -- produces an initial seed set (up to 2x the requested results).
2. **Query-entity extraction** -- GLiNER runs on the query text (~5ms) to identify entities mentioned in the question. These are matched against `document_entities.canonical_form` to find chunks containing those entities across **all documents**, regardless of vector similarity.
3. **Seed-based entity expansion** -- entities from seed chunks are looked up in `entity_relationships` (1-hop traversal). Chunks containing related entities are added to the candidate pool.
4. **Merge and rerank** -- all candidates (seeds + query-entity chunks + graph-discovered chunks) are merged, deduplicated, and reranked by the cross-encoder.

This approach ensures cross-document entity coverage: if the user asks about "Keynes" and a document mentions Keynes only in passing (low vector similarity), graph retrieval can still surface that passage through entity matching.

### Cleanup on Document Deletion

When a document is deleted or reprocessed, AXIOM cleans up all associated knowledge graph data:

- `entity_relationships` where either source or target entity belongs to the document
- `entity_chunk_occurrences` for the document's chunks
- `document_entities` that were linked to the document

This ensures no orphaned graph data accumulates over time.

### Rebuilding the Graph

If you change extraction settings or want to re-process a document's entities, you can trigger a rebuild through the API:

```bash
# Rebuild graph for a single document
curl -X POST http://localhost/api/rag/documents/{doc_id}/rebuild-graph

# Bulk rebuild for multiple documents
curl -X POST http://localhost/api/rag/documents/bulk-rebuild-graph \
  -H "Content-Type: application/json" \
  -d '["doc_id_1", "doc_id_2"]'
```

The rebuild operation clears existing relationships and entities for the specified document(s), then re-extracts entities and rebuilds both co-occurrence and mREBEL relationships.

## PDF Page Numbers for Citations

AXIOM tracks logical page numbers throughout the pipeline so that citations in research notes and generated reports reference the actual printed page number rather than internal indices.

### 3-Tier Page Label Extraction

The `extract_page_labels()` function in `processor.py` determines the display page number for each physical PDF page using a three-tier fallback:

| Tier | Method | When Used |
|---|---|---|
| 1 | **PDF embedded labels** | Publisher-set page labels in the PDF metadata. Used when more than 50% of pages have labels. |
| 2 | **Header/footer parsing** | Extracts standalone numbers from the top/bottom 8% of each page. Includes outlier detection (e.g., a volume number "60" among page numbers "536, 537, 538" is removed). Validates that parsed numbers are roughly sequential. Extrapolates to all pages using the median offset. |
| 3 | **Physical index + 1** | Simple fallback when no reliable labels are found. |

### End-to-End Page Flow

1. **PDF processing** -- `extract_page_labels()` produces a `page_label_map: Dict[int, str]` mapping physical page index to display label.
2. **Marker conversion** -- Marker runs with `paginate_output=True`, inserting `{N}------------------------------------------------` markers between pages.
3. **Chunking** -- the chunker parses these markers, maps each paragraph to its physical page, then translates via `page_label_map` to the logical label. Each chunk stores `page_start` and `page_end` in its metadata.
4. **Research notes** -- when the research agent retrieves chunks, page metadata flows through to note source metadata.
5. **Writing agent** -- formats citations with page numbers, producing references like `(Keat, 2012, S. 542)` instead of `(Keat, 2012, S. XX)`.
6. **Chat RAG** -- the chat prompt includes page numbers in source references for inline citations.

## Chat RAG Prompt Architecture

The chat system constructs a unified prompt with two distinct sections, each serving a different purpose:

### DOCUMENT LIBRARY Section

Built from the `documents` database table (not chunk metadata). Contains authoritative metadata for every document the user has access to:

- Title, authors, publication year
- Document type, journal/source
- Description/abstract

This section answers metadata questions ("What documents do I have about X?", "Who authored Y?").

### TEXT EXCERPTS Section

Contains the actual retrieved chunk text from hybrid search + graph retrieval. Each excerpt is labeled with its source number `[N]` and followed by a source reference line.

### Source Reference Format

Each cited source includes:

- Title
- Authors
- Year
- Chapter/section heading (from chunk section titles)
- Page number (from chunk `page_start` metadata)

Example: `[1] Volkswirtschaftslehre -- Samuelson, Nordhaus (2010), Kap. "Makrookonomische Grundlagen", S. 542`

### Image Handling Rules

The prompt instructs the LLM to:

- When the user asks generally about images/figures: list available images by description, ask which ones to show.
- When the user asks for a specific figure: only show it if the image path is explicitly referenced in the text excerpts.
- Never guess which image file corresponds to a figure number.

## RAG Frontend View

The RAG view provides a dedicated interface for exploring your document chunks, entity statistics, and the knowledge graph visualization. Access it by clicking the **Network icon** in the view toggle at the top of the Documents tab.

### Chunks Tab

- **Paginated list** of all document chunks with text previews
- **Search** across chunk content using the search bar
- **Filter** by document or document group using the left sidebar
- Click any chunk to view its full text, associated entities, and relationships

### Statistics Tab

- **Entity counts** broken down by type (person, organization, location, etc.)
- **Relationship counts** by source (co-occurrence, mREBEL-extracted)
- Overview of your knowledge graph density and coverage

### Interactive Graph Tab

- **Force-directed layout** powered by D3.js
- **Zoom, pan, and drag** to navigate large graphs
- **Hover tooltips** show entity details, chunk count, and document count
- **Color-coded nodes** by entity type for quick visual identification
- **Filter** by document, entity type, and minimum relationship strength

!!! tip
    For large libraries, filter the graph to a single document or group first. Rendering thousands of nodes at once can be slow in the browser.

## Image Extraction and Multimodal Search

Documents processed via Marker can have their embedded images automatically extracted, stored, and made searchable alongside text content.

### How Image Processing Works

1. During PDF processing, Marker extracts embedded images from each page.
2. Images are stored at `/app/data/processed/images/{doc_id}/`.
3. Each image is embedded using **CLIP** (`clip-ViT-B-32`, 512-dimensional vectors).
4. Image embeddings are indexed alongside text embeddings for multimodal retrieval.

### Multimodal Search

When you search in chat or the RAG interface, AXIOM can combine:

- **Text similarity** -- standard BGE-M3 embeddings against document chunks
- **Image similarity** -- CLIP embeddings match your query against extracted images

Images that match your query appear **inline in chat responses** with click-to-expand functionality.

### Image Serving

Extracted images are served through an authenticated API endpoint:

```
GET /api/documents/images/{doc_id}/{image_filename}
```

!!! note
    Image extraction and embedding are enabled by default. If you do not need image search or want to reduce processing time and storage, set `ENABLE_IMAGE_EXTRACTION=false` and `ENABLE_IMAGE_EMBEDDINGS=false` in your `.env` file.

## LLM-Enhanced OCR

Marker supports an optional LLM integration for improved extraction of complex elements such as mathematical formulas, tables, and diagrams.

### When to Enable

- Documents contain **mathematical notation** or complex formulas
- Tables have **irregular layouts** that standard extraction mishandles
- You need higher-fidelity text from **mixed-content pages**

### How It Works

When `MARKER_USE_LLM=true`, Marker sends difficult page regions to a vision-capable LLM for enhanced recognition. The LLM service can be configured to use OpenAI-compatible APIs or a local Ollama instance.

!!! warning
    LLM-enhanced OCR is **disabled by default** because it adds significant cost and processing time. Each page region sent to the LLM incurs an API call. Enable this only when document quality demands it.

### Marker LLM Configuration

```bash
# Enable LLM-enhanced OCR
MARKER_USE_LLM=true

# LLM service class (OpenAI-compatible or Ollama)
MARKER_LLM_SERVICE=marker.services.openai.OpenAIService

# Model to use for vision OCR
MARKER_LLM_MODEL=gpt-5.2

# API key (falls back to OPENAI_API_KEY if not set)
MARKER_LLM_API_KEY=your-api-key

# Custom base URL for OpenAI-compatible endpoints
MARKER_LLM_BASE_URL=https://api.example.com/v1
```

## Configuration

All RAG and knowledge graph features are controlled through environment variables in your `.env` file. Features are enabled by default where practical.

### Image Processing

| Variable | Default | Description |
|---|---|---|
| `ENABLE_IMAGE_EXTRACTION` | `true` | Extract images from PDFs during Marker processing |
| `ENABLE_IMAGE_EMBEDDINGS` | `true` | Generate CLIP embeddings for extracted images |
| `IMAGE_EMBEDDING_MODEL` | `clip-ViT-B-32` | Sentence-transformers model for image embeddings |
| `IMAGE_EMBEDDING_BATCH_SIZE` | `4` | Batch size for image embedding generation |

### Knowledge Graph

| Variable | Default | Description |
|---|---|---|
| `ENABLE_KNOWLEDGE_GRAPH` | `true` | Enable entity extraction (GLiNER + mREBEL) and graph construction during document processing |
| `ENABLE_GRAPH_RETRIEVAL` | `true` | Use graph traversal and query-entity extraction to enhance vector search results |
| `GRAPH_MAX_DEPTH` | `2` | Maximum BFS traversal depth from seed entities |
| `GRAPH_MIN_STRENGTH` | `0.3` | Minimum relationship strength to traverse |
| `GRAPH_DECAY_FACTOR` | `0.6` | Score decay per hop during graph traversal |
| `GRAPH_VECTOR_WEIGHT` | `0.6` | Weight of vector similarity in final ranking |
| `GRAPH_GRAPH_WEIGHT` | `0.3` | Weight of graph relevance in final ranking |
| `GRAPH_DIVERSITY_WEIGHT` | `0.1` | Weight of diversity bonus in final ranking |
| `GRAPH_CACHE_SIZE` | `1000` | Number of graph query results to cache |

### Entity Extraction

| Variable | Default | Description |
|---|---|---|
| `ENTITY_BATCH_SIZE` | `50` | Number of chunks to process per entity extraction batch |
| `ENTITY_CONFIDENCE_THRESHOLD` | `0.7` | Minimum confidence score for entity acceptance (spaCy fallback) |

### OpenSearch Integration

| Variable | Default | Description |
|---|---|---|
| `ENABLE_OPENSEARCH` | `true` | Enable OpenSearch fulltext search alongside vector search |
| `OPENSEARCH_HOST` | `localhost` | OpenSearch server hostname or IP (change to your OpenSearch instance address) |
| `OPENSEARCH_PORT` | `9200` | OpenSearch server port |
| `OPENSEARCH_INDEX` | `axiom_chunks` | Index name for document chunks |
| `OPENSEARCH_USE_SSL` | `false` | Enable SSL for OpenSearch connections |
| `OPENSEARCH_USERNAME` | (empty) | OpenSearch authentication username |
| `OPENSEARCH_PASSWORD` | (empty) | OpenSearch authentication password |

### Marker LLM

| Variable | Default | Description |
|---|---|---|
| `MARKER_USE_LLM` | `false` | Enable LLM-enhanced OCR during PDF processing |
| `MARKER_LLM_SERVICE` | `marker.services.openai.OpenAIService` | Marker LLM service class |
| `MARKER_LLM_MODEL` | `gpt-5.2` | Vision model for enhanced OCR |
| `MARKER_LLM_API_KEY` | (falls back to `OPENAI_API_KEY`) | API key for the Marker LLM service |
| `MARKER_LLM_BASE_URL` | (empty) | Custom base URL for OpenAI-compatible endpoints |

## API Endpoints Reference

The RAG system exposes the following endpoints under the `/api/rag` prefix. All endpoints require authentication.

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/rag/documents/{doc_id}/rebuild-graph` | Rebuild knowledge graph for a single document |
| `POST` | `/api/rag/documents/bulk-rebuild-graph` | Rebuild knowledge graph for multiple documents (accepts JSON array of doc IDs) |
| `GET` | `/api/rag/chunks` | List all chunks with pagination, search, and document filtering |
| `GET` | `/api/rag/chunks/{chunk_id}` | Get full chunk detail including relationships and entities |
| `GET` | `/api/rag/graph` | Get knowledge graph nodes and edges for visualization (filterable by document, entity type, strength) |
| `GET` | `/api/rag/entities` | List entities with pagination, type filtering, and search |
| `GET` | `/api/documents/images/{doc_id}/{image_filename}` | Serve an extracted image from a processed document |

!!! note
    The `/api/rag/chunks` endpoint returns text previews (first 500 characters). Use `/api/rag/chunks/{chunk_id}` to retrieve the full chunk text along with its entity and relationship data.

## Troubleshooting

### No Chunks Appearing in RAG View

- Verify documents have completed processing (status should be **Completed**)
- Check that the document processor service is running
- Review logs for embedding generation errors

### Knowledge Graph Empty

- Confirm `ENABLE_KNOWLEDGE_GRAPH=true` in your `.env` file
- Check that GLiNER is installed (`pip install gliner`) or that spaCy models are present in the container
- Try rebuilding the graph for a specific document via the API

### Images Not Displaying

- Confirm `ENABLE_IMAGE_EXTRACTION=true` in your `.env` file
- Check that the image directory exists: `/app/data/processed/images/`
- Verify the document was processed with Marker (only PDF processing extracts images)

### Graph Retrieval Slow

- Reduce `GRAPH_MAX_DEPTH` to `1` for faster traversal
- Increase `GRAPH_MIN_STRENGTH` to prune weak relationships
- Filter to a specific document group to reduce the search space

### Page Numbers Showing as XX or Missing

- The document may have been processed before page label extraction was implemented. Reprocess the document.
- For scanned PDFs, Tier 2 header/footer parsing may not find consistent numbers. Check the document processor logs for `Page labels: Tier X` messages.
- Ensure Marker is running with `paginate_output=True` (the default).

## Next Steps

- [Documents Overview](overview.md) - Document processing and management
- [Document Groups](document-groups.md) - Organizing documents into collections
- [Research Overview](../research/overview.md) - Using RAG-grounded research
- [VRAM Management](../../architecture/vram-management.md) - GPU memory sharing between models
- [Environment Variables](../../getting-started/configuration/environment-variables.md) - Full configuration reference
