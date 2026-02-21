"""
RAG API Routes

Endpoints for knowledge graph visualization, chunk exploration, and graph management.
"""

from fastapi import APIRouter, HTTPException, Depends, Query
from typing import List, Optional, Dict
from sqlalchemy import text
from database.database import get_db
import logging

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/api/rag", tags=["RAG"])


@router.post("/documents/{doc_id}/rebuild-graph")
async def rebuild_graph(
    doc_id: str,
    db=Depends(get_db)
):
    """
    Trigger re-graphing for a specific document.
    Rebuilds chunk relationships and extracts entities.
    """
    from ai_researcher import config

    if not config.ENABLE_KNOWLEDGE_GRAPH:
        raise HTTPException(
            status_code=400,
            detail="Knowledge graph feature is disabled. Set ENABLE_KNOWLEDGE_GRAPH=true to enable."
        )

    try:
        # Get document chunks
        query = text("""
            SELECT chunk_id, chunk_text, chunk_metadata
            FROM document_chunks
            WHERE doc_id = :doc_id
            ORDER BY (chunk_metadata->>'chunk_index')::int
        """)
        result = db.execute(query, {'doc_id': doc_id}).fetchall()

        if not result:
            raise HTTPException(
                status_code=404,
                detail="Document not found or has no chunks"
            )

        chunks = [
            {
                'metadata': {'chunk_id': row[0]},
                'text': row[1]
            }
            for row in result
        ]

        # Rebuild graph
        from ai_researcher.core_rag.graph_store import GraphStore
        graph_store = GraphStore()

        # 1. Clear existing relationships for this document
        delete_query = text("""
            DELETE FROM chunk_relationships
            WHERE source_chunk_id IN (
                SELECT chunk_id FROM document_chunks WHERE doc_id = :doc_id
            )
        """)
        db.execute(delete_query, {'doc_id': doc_id})

        delete_entity_query = text("""
            DELETE FROM entity_chunk_occurrences
            WHERE doc_id = :doc_id
        """)
        db.execute(delete_entity_query, {'doc_id': doc_id})
        db.commit()

        # 2. Rebuild sequential relationships
        graph_store.build_sequential_relationships(doc_id, len(chunks))

        # 3. Re-extract entities (spaCy always, LLM if enabled)
        from database.user_settings import get_user_settings
        from ai_researcher.core_rag.metadata_extractor import MetadataExtractor
        from ai_researcher.core_rag.entity_extractor import EntityExtractor
        from ai_researcher.core_rag.embedder import TextEmbedder

        user_settings = get_user_settings()
        metadata_extractor = MetadataExtractor.from_user_settings(user_settings)
        embedder = TextEmbedder()

        # Create entity extractor (uses spaCy + optional LLM)
        entity_extractor = EntityExtractor(
            embedder=embedder,
            llm_client=metadata_extractor.client if config.ENTITY_EXTRACTION_CONFIG['enable_llm_refinement'] else None,
            enable_llm_refinement=config.ENTITY_EXTRACTION_CONFIG['enable_llm_refinement']
        )

        entities_count = 0
        llm_relationships_count = 0
        entity_id_map = {}  # canonical_form -> entity_id

        for chunk in chunks:
            entities, relationships = await entity_extractor.extract_from_chunk(
                chunk['text'],
                chunk['metadata']
            )

            # Store entities
            for entity in entities:
                entity_id = graph_store.add_entity(
                    entity['text'],
                    entity['type'],
                    entity['canonical_form']
                )
                graph_store.link_entity_to_chunk(
                    entity_id,
                    chunk['metadata']['chunk_id'],
                    doc_id
                )
                entities_count += 1

                # Track entity IDs for relationship building
                key = f"{entity['canonical_form']}:{entity['type']}"
                entity_id_map[key] = entity_id

            # Store LLM-extracted relationships if any
            if relationships and config.ENTITY_EXTRACTION_CONFIG['enable_llm_refinement']:
                for rel in relationships:
                    try:
                        # Find entity IDs
                        source_key = None
                        target_key = None

                        for key, eid in entity_id_map.items():
                            canonical = key.split(':')[0]
                            if canonical == entity_extractor._normalize_entity(rel.get('source', '')):
                                source_key = key
                            if canonical == entity_extractor._normalize_entity(rel.get('target', '')):
                                target_key = key

                        if source_key and target_key:
                            graph_store.add_entity_relationship(
                                entity_id_map[source_key],
                                entity_id_map[target_key],
                                rel.get('type', 'RELATED'),
                                rel.get('confidence', 0.8),
                                [chunk['metadata']['chunk_id']],
                                source='llm'
                            )
                            llm_relationships_count += 1
                    except Exception as e:
                        logger.warning(f"Failed to add LLM relationship: {e}")

        # 4. Build co-occurrence relationships (always, regardless of LLM setting)
        logger.info(f"Building co-occurrence relationships for doc_id={doc_id}")
        cooccurrence_count = graph_store.build_cooccurrence_relationships(
            doc_id=doc_id,
            min_cooccurrence=2
        )

        return {
            "status": "success",
            "doc_id": doc_id,
            "chunks_processed": len(chunks),
            "entities_extracted": entities_count,
            "llm_relationships": llm_relationships_count,
            "cooccurrence_relationships": cooccurrence_count,
            "message": "Knowledge graph rebuilt successfully with both co-occurrence and LLM relationships"
        }

    except Exception as e:
        logger.error(f"Graph rebuild failed for doc {doc_id}: {e}")
        raise HTTPException(
            status_code=500,
            detail=f"Graph rebuild failed: {str(e)}"
        )


@router.post("/documents/bulk-rebuild-graph")
async def bulk_rebuild_graph(
    doc_ids: List[str],
    db=Depends(get_db)
):
    """
    Trigger re-graphing for multiple documents.
    """
    results = []
    for doc_id in doc_ids:
        try:
            result = await rebuild_graph(doc_id, db)
            results.append({"doc_id": doc_id, "status": "success"})
        except Exception as e:
            results.append({"doc_id": doc_id, "status": "failed", "error": str(e)})

    return {
        "total": len(doc_ids),
        "successful": len([r for r in results if r["status"] == "success"]),
        "failed": len([r for r in results if r["status"] == "failed"]),
        "results": results
    }


@router.get("/chunks")
async def get_all_chunks(
    page: int = Query(1, ge=1),
    limit: int = Query(50, ge=1, le=500),
    doc_id: Optional[str] = None,
    search: Optional[str] = None,
    db=Depends(get_db)
):
    """
    List all chunks with pagination and optional filtering.
    """
    offset = (page - 1) * limit

    # Build query
    where_clauses = []
    params = {'limit': limit, 'offset': offset}

    if doc_id:
        where_clauses.append("dc.doc_id = :doc_id")
        params['doc_id'] = doc_id

    if search:
        where_clauses.append("dc.chunk_text ILIKE :search")
        params['search'] = f"%{search}%"

    where_sql = "WHERE " + " AND ".join(where_clauses) if where_clauses else ""

    # Count total
    count_query = text(f"""
        SELECT COUNT(*)
        FROM document_chunks dc
        {where_sql}
    """)
    total_count = db.execute(count_query, params).scalar()

    # Fetch chunks
    query = text(f"""
        SELECT
            dc.chunk_id,
            dc.chunk_text,
            dc.chunk_metadata,
            dc.doc_id,
            d.original_filename,
            d.metadata_->>'title'
        FROM document_chunks dc
        LEFT JOIN documents d ON dc.doc_id = d.id
        {where_sql}
        ORDER BY dc.created_at DESC
        LIMIT :limit OFFSET :offset
    """)

    results = db.execute(query, params).fetchall()

    chunks = [
        {
            "chunk_id": row[0],
            "text": row[1][:500] + "..." if len(row[1]) > 500 else row[1],  # Preview
            "metadata": row[2],
            "doc_id": row[3],
            "document_filename": row[4],
            "document_metadata_title": row[5]
        }
        for row in results
    ]

    return {
        "chunks": chunks,
        "pagination": {
            "page": page,
            "limit": limit,
            "total_count": total_count,
            "total_pages": (total_count + limit - 1) // limit
        }
    }


@router.get("/chunks/{chunk_id}")
async def get_chunk_detail(
    chunk_id: str,
    db=Depends(get_db)
):
    """
    Get full details for a specific chunk including relationships.
    """
    # Get chunk data
    chunk_query = text("""
        SELECT
            dc.chunk_id,
            dc.chunk_text,
            dc.chunk_metadata,
            dc.doc_id,
            d.original_filename,
            d.metadata_->>'title'
        FROM document_chunks dc
        LEFT JOIN documents d ON dc.doc_id = d.id
        WHERE dc.chunk_id = :chunk_id
    """)
    chunk_result = db.execute(chunk_query, {'chunk_id': chunk_id}).fetchone()

    if not chunk_result:
        raise HTTPException(status_code=404, detail="Chunk not found")

    # Get relationships
    rel_query = text("""
        SELECT
            cr.target_chunk_id,
            cr.relationship_type,
            cr.strength,
            dc.chunk_text
        FROM chunk_relationships cr
        LEFT JOIN document_chunks dc ON cr.target_chunk_id = dc.chunk_id
        WHERE cr.source_chunk_id = :chunk_id
        ORDER BY cr.strength DESC
    """)
    relationships = db.execute(rel_query, {'chunk_id': chunk_id}).fetchall()

    # Get entities
    entity_query = text("""
        SELECT
            de.entity_text,
            de.entity_type,
            eco.occurrence_count,
            eco.relevance_score
        FROM entity_chunk_occurrences eco
        JOIN document_entities de ON eco.entity_id = de.id
        WHERE eco.chunk_id = :chunk_id
        ORDER BY eco.relevance_score DESC
    """)
    entities = db.execute(entity_query, {'chunk_id': chunk_id}).fetchall()

    return {
        "chunk_id": chunk_result[0],
        "text": chunk_result[1],
        "metadata": chunk_result[2],
        "doc_id": chunk_result[3],
        "document_filename": chunk_result[4],
        "document_metadata_title": chunk_result[5],
        "relationships": [
            {
                "target_chunk_id": r[0],
                "type": r[1],
                "strength": r[2],
                "target_preview": r[3][:200] if r[3] else None
            }
            for r in relationships
        ],
        "entities": [
            {
                "text": e[0],
                "type": e[1],
                "occurrences": e[2],
                "relevance": e[3]
            }
            for e in entities
        ]
    }


@router.get("/graph")
async def get_knowledge_graph(
    doc_id: Optional[str] = None,
    entity_types: Optional[List[str]] = Query(None),
    min_strength: float = Query(0.1, ge=0.0, le=1.0),
    limit: int = Query(500, ge=1, le=2000),
    db=Depends(get_db)
):
    """
    Get knowledge graph data (nodes and edges) for visualization.
    Returns entity nodes and relationship edges in a format suitable for D3.js or Cytoscape.
    """
    where_clauses = []
    params = {'min_strength': min_strength, 'limit': limit}

    if doc_id:
        where_clauses.append("eco.doc_id = :doc_id")
        params['doc_id'] = doc_id

    if entity_types:
        where_clauses.append("de.entity_type = ANY(:types)")
        params['types'] = entity_types

    entity_where = "WHERE " + " AND ".join(where_clauses) if where_clauses else ""

    # Get entity nodes
    nodes_query = text(f"""
        SELECT DISTINCT
            de.id,
            de.entity_text,
            de.entity_type,
            de.canonical_form,
            COUNT(DISTINCT eco.chunk_id) as chunk_count,
            COUNT(DISTINCT eco.doc_id) as doc_count
        FROM document_entities de
        JOIN entity_chunk_occurrences eco ON de.id = eco.entity_id
        {entity_where}
        GROUP BY de.id, de.entity_text, de.entity_type, de.canonical_form
        ORDER BY chunk_count DESC
        LIMIT :limit
    """)
    nodes_result = db.execute(nodes_query, params).fetchall()

    # Get entity IDs for edge filtering
    entity_ids = [str(row[0]) for row in nodes_result]

    if not entity_ids:
        return {"nodes": [], "edges": [], "stats": {"total_nodes": 0, "total_edges": 0, "entity_types": []}}

    # Get relationship edges between these entities
    # Cast entity_ids to UUID array to match database column type
    edges_query = text("""
        SELECT
            er.source_entity_id,
            er.target_entity_id,
            er.relationship_type,
            er.relationship_strength,
            er.evidence_count
        FROM entity_relationships er
        WHERE er.source_entity_id = ANY(CAST(:entity_ids AS uuid[]))
          AND er.target_entity_id = ANY(CAST(:entity_ids AS uuid[]))
          AND er.relationship_strength >= :min_strength
        ORDER BY er.relationship_strength DESC
        LIMIT :limit
    """)
    edges_result = db.execute(edges_query, {
        'entity_ids': entity_ids,
        'min_strength': min_strength,
        'limit': limit
    }).fetchall()

    # Format for frontend visualization
    nodes = [
        {
            "id": str(row[0]),
            "label": row[1],
            "type": row[2],
            "canonical": row[3],
            "chunk_count": row[4],
            "doc_count": row[5]
        }
        for row in nodes_result
    ]

    edges = [
        {
            "source": str(row[0]),
            "target": str(row[1]),
            "type": row[2],
            "strength": row[3],
            "evidence_count": row[4]
        }
        for row in edges_result
    ]

    return {
        "nodes": nodes,
        "edges": edges,
        "stats": {
            "total_nodes": len(nodes),
            "total_edges": len(edges),
            "entity_types": list(set(n["type"] for n in nodes))
        }
    }


@router.get("/entities")
async def get_entities(
    page: int = Query(1, ge=1),
    limit: int = Query(50, ge=1, le=200),
    entity_type: Optional[str] = None,
    search: Optional[str] = None,
    db=Depends(get_db)
):
    """
    List entities with pagination and filtering.
    """
    offset = (page - 1) * limit
    where_clauses = []
    params = {'limit': limit, 'offset': offset}

    if entity_type:
        where_clauses.append("entity_type = :entity_type")
        params['entity_type'] = entity_type

    if search:
        where_clauses.append("entity_text ILIKE :search")
        params['search'] = f"%{search}%"

    where_sql = "WHERE " + " AND ".join(where_clauses) if where_clauses else ""

    count_query = text(f"SELECT COUNT(*) FROM document_entities {where_sql}")
    total_count = db.execute(count_query, params).scalar()

    query = text(f"""
        SELECT
            id,
            entity_text,
            entity_type,
            canonical_form,
            (SELECT COUNT(*) FROM entity_chunk_occurrences WHERE entity_id = document_entities.id) as occurrence_count
        FROM document_entities
        {where_sql}
        ORDER BY occurrence_count DESC
        LIMIT :limit OFFSET :offset
    """)

    results = db.execute(query, params).fetchall()

    entities = [
        {
            "id": str(row[0]),
            "text": row[1],
            "type": row[2],
            "canonical_form": row[3],
            "occurrence_count": row[4]
        }
        for row in results
    ]

    return {
        "entities": entities,
        "pagination": {
            "page": page,
            "limit": limit,
            "total_count": total_count,
            "total_pages": (total_count + limit - 1) // limit
        }
    }
