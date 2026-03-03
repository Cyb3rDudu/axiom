# RAG and Knowledge Graph

AXIOM's Retrieval-Augmented Generation (RAG) pipeline goes beyond basic vector search by combining semantic embeddings, fulltext indexing, a PostgreSQL-native knowledge graph, and multimodal image search. Together these layers ensure that chat responses, research missions, and writing tasks are grounded in the actual content of your uploaded documents.

## Document-Aware RAG Chat

When you upload documents to AXIOM, they are automatically chunked, embedded, and indexed. Every subsequent chat message is searched against your document library so the LLM can ground its responses in retrieved passages.

### How It Works

1. You send a message in the chat interface.
2. AXIOM embeds your query with BGE-M3 (dense + sparse vectors).
3. Up to **8 relevant chunks** are retrieved via hybrid search and reranked for precision.
4. The top chunks are injected into the LLM prompt as context.
5. The LLM generates a response with **source attribution** referencing the original documents.

### Key Details

- **No setup required** -- works automatically once documents are uploaded and processed.
- **Hybrid search** -- combines dense embeddings (1024-dim), sparse embeddings (30,000-dim), and optional OpenSearch fulltext.
- **Reranking** -- a cross-encoder reranker scores candidates before the final selection.
- **Document group scoping** -- select a document group in the chat interface to limit retrieval to a specific collection.

!!! tip
    For the best results, upload well-structured documents with extractable text. Scanned image-only PDFs will produce lower-quality chunks unless LLM-enhanced OCR is enabled.

## Knowledge Graph

The knowledge graph is a PostgreSQL-native entity and relationship layer that enriches standard vector retrieval. Entities are extracted from every document chunk and linked together, allowing AXIOM to discover connections that pure embedding similarity might miss.

### Entity Extraction

Entities are extracted using a hybrid approach:

- **spaCy NER** (`en_core_web_lg`) -- always active when the knowledge graph is enabled. Detects persons, organizations, locations, dates, and other named entities with high throughput.
- **LLM extraction** (optional) -- when `ENTITY_ENABLE_LLM=true`, an LLM refines and extends spaCy results, catching domain-specific entities and extracting explicit relationships between them.

### Relationship Types

| Relationship Source | Method | When Active |
|---|---|---|
| Sequential | Chunk ordering within a document | Always |
| Co-occurrence | Entities appearing in the same chunks (min 2 co-occurrences) | Always (with knowledge graph enabled) |
| LLM-extracted | Explicit typed relationships identified by the LLM | Only when `ENTITY_ENABLE_LLM=true` |

### Graph-Enhanced Retrieval

When `ENABLE_GRAPH_RETRIEVAL` is enabled (the default), retrieval follows a two-stage process:

1. **Vector similarity** -- standard hybrid search produces an initial candidate set.
2. **Graph traversal** -- a breadth-first search (BFS) walks entity relationships outward from the candidate chunks, boosting related chunks that share entities or co-occurrence links.

The final ranking blends three weighted signals:

| Signal | Default Weight | Environment Variable |
|---|---|---|
| Vector similarity | 0.6 | `GRAPH_VECTOR_WEIGHT` |
| Graph relevance | 0.3 | `GRAPH_GRAPH_WEIGHT` |
| Diversity | 0.1 | `GRAPH_DIVERSITY_WEIGHT` |

!!! info
    Graph traversal depth, minimum relationship strength, and decay factor are all configurable. See the [Configuration](#configuration) section below for the full list of tuning variables.

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

The rebuild operation clears existing relationships and entities for the specified document(s), then re-extracts entities and rebuilds both co-occurrence and (if enabled) LLM relationships.

## RAG Frontend View

The RAG view provides a dedicated interface for exploring your document chunks, entity statistics, and the knowledge graph visualization. Access it by clicking the **Network icon** in the view toggle at the top of the Documents tab.

### Chunks Tab

- **Paginated list** of all document chunks with text previews
- **Search** across chunk content using the search bar
- **Filter** by document or document group using the left sidebar
- Click any chunk to view its full text, associated entities, and relationships

### Statistics Tab

- **Entity counts** broken down by type (person, organization, location, etc.)
- **Relationship counts** by source (co-occurrence, LLM-extracted)
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
| `ENABLE_KNOWLEDGE_GRAPH` | `true` | Enable entity extraction and graph construction during document processing |
| `ENABLE_GRAPH_RETRIEVAL` | `true` | Use graph traversal to enhance vector search results |
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
| `ENTITY_ENABLE_LLM` | `false` | Enable LLM-based entity refinement and relationship extraction |
| `ENTITY_BATCH_SIZE` | `50` | Number of chunks to process per entity extraction batch |
| `ENTITY_CONFIDENCE_THRESHOLD` | `0.7` | Minimum confidence score for entity acceptance |

### OpenSearch Integration

| Variable | Default | Description |
|---|---|---|
| `ENABLE_OPENSEARCH` | `true` | Enable OpenSearch fulltext search alongside vector search |
| `OPENSEARCH_HOST` | `10.36.0.110` | OpenSearch server hostname or IP |
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
- Ensure the spaCy `en_core_web_lg` model is installed in the container
- Try rebuilding the graph for a specific document via the API

### Images Not Displaying

- Confirm `ENABLE_IMAGE_EXTRACTION=true` in your `.env` file
- Check that the image directory exists: `/app/data/processed/images/`
- Verify the document was processed with Marker (only PDF processing extracts images)

### Graph Retrieval Slow

- Reduce `GRAPH_MAX_DEPTH` to `1` for faster traversal
- Increase `GRAPH_MIN_STRENGTH` to prune weak relationships
- Filter to a specific document group to reduce the search space

## Next Steps

- [Documents Overview](overview.md) - Document processing and management
- [Document Groups](document-groups.md) - Organizing documents into collections
- [Research Overview](../research/overview.md) - Using RAG-grounded research
- [Environment Variables](../../getting-started/configuration/environment-variables.md) - Full configuration reference
