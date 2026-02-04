# Maestro RAG Enhancement Research - Memory File

**Date:** 2026-01-26
**Status:** Plan erstellt, Implementation ausstehend

---

## Context

User will Maestro's RAG verbessern. Hat bereits ApeRAG mit OpenSearch/Qdrant/PostgreSQL laufen, will aber Maestro's lokales Chunking/Embedding behalten und erweitern.

## User Pain Points

1. **Kein Document Chat Mode** - Muss Research Mission erstellen nur um über Dokumente zu chatten
2. **Dokumenten-Organisation schlecht** - Web-Sources vermischen sich mit Uploads, nur 1 Gruppe pro Mission
3. **Kein Bild-RAG** - ApeRAG kann Bilder durchsuchen
4. **Kein GraphRAG** - Für bessere Multi-Hop Queries

## Priorisierung

User will **Document Chat Mode** zuerst implementieren.

---

## Research Summary

### Maestro's Current RAG Architecture
- **Embeddings:** BGE-M3, 1024-dim dense + sparse vectors
- **Storage:** PostgreSQL + pgvector
- **Retrieval:** Hybrid search (dense + sparse) + optional reranking
- **Chunks:** 86,256 total in DB
- **Key Files:**
  - `maestro_backend/ai_researcher/core_rag/retriever.py`
  - `maestro_backend/ai_researcher/core_rag/pgvector_store.py`
  - `maestro_backend/ai_researcher/agentic_layer/tools/document_search.py`

### Vorhandene Infrastruktur (bereits nutzbar!)
- `document_group_id` Parameter in Chat API ✅
- `DocumentSearchTool` mit RAG-Suche ✅
- `SimplifiedWritingAgent` hat bereits RAG-Chat Logik ✅

### GraphRAG Research
- **Microsoft GraphRAG:** Entity extraction, community detection, hierarchical summaries
- **ApeRAG/LightRAG:** Production-ready, simpler approach, 5 index types
- **Vorteile:** Multi-hop queries, entity-centric questions, relationship queries
- **Nachteile:** Höhere Indexing-Kosten (LLM calls), komplexer

---

## Implementation Plan

### Phase 1: Document Chat Mode (2-3 Tage)
1. Neuer Intent "rag_chat" in MessengerAgent
2. RAG-Chat Handler in user_interaction.py
3. Sources in ChatResponse hinzufügen
4. Frontend: Sources anzeigen

### Phase 2: Dokumenten-Organisation (1-2 Tage)
- `source_type` Feld für Documents ('upload', 'web', 'generated')
- M:N Relation Mission ↔ DocumentGroups
- UI Filter nach source_type

### Phase 3: Knowledge Graph (5-7 Tage)
- `entity_extractor.py` - LLM-basierte Extraction
- `knowledge_graph.py` - PostgreSQL-basierter Graph
- `graph_retriever.py` - Hybrid Retrieval
- Schema: entities, entity_chunk_mentions, entity_relationships

### Phase 4: Image RAG (Future, 7-10 Tage)
- Vision Index mit CLIP/SigLIP
- Multimodal Chunks

---

## Key Files to Modify

**Phase 1:**
- `maestro_backend/ai_researcher/agentic_layer/agents/messenger_agent.py`
- `maestro_backend/ai_researcher/agentic_layer/controller/user_interaction.py`
- `maestro_backend/api/chat.py`

**Phase 3:**
- Neue Files in `maestro_backend/ai_researcher/core_rag/`:
  - `entity_extractor.py`
  - `knowledge_graph.py`
  - `graph_retriever.py`
- `init-db/09-knowledge-graph.sql`

---

## Full Plan File

Der detaillierte Plan ist in `/home/dudu/.claude/plans/quirky-frolicking-scone.md`

---

## To Continue

1. Plan File lesen: `cat /home/dudu/.claude/plans/quirky-frolicking-scone.md`
2. Mit Phase 1 (Document Chat Mode) starten
3. MessengerAgent um "rag_chat" Intent erweitern
