"""
RAG API Routes

Endpoints for knowledge graph visualization, chunk exploration, and graph management.
"""

from fastapi import APIRouter, HTTPException, Depends, Query
from typing import List, Optional, Dict
from sqlalchemy import text
from database.database import get_db
from database.models import User
from auth.dependencies import get_current_user_from_cookie
import logging

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/api/rag", tags=["RAG"])


@router.post("/documents/{doc_id}/rebuild-graph")
async def rebuild_graph(
    doc_id: str,
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
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

    # Resolve all imports BEFORE reading chunks or mutating the DB so a
    # missing module can never leave the document in a half-deleted state.
    from ai_researcher.core_rag.graph_store import GraphStore
    from ai_researcher.core_rag.entity_extractor import EntityExtractor

    try:
        graph_store = GraphStore()
        entity_extractor = EntityExtractor()

        # Read chunks (no writes yet). The JOIN on documents scopes the read to
        # the current user's own documents; a foreign doc_id yields no rows.
        query = text("""
            SELECT dc.chunk_id, dc.chunk_text, dc.chunk_metadata
            FROM document_chunks dc
            JOIN documents d ON d.id = dc.doc_id
            WHERE dc.doc_id = :doc_id
              AND d.user_id = :user_id
            ORDER BY (dc.chunk_metadata->>'chunk_index')::int
        """)
        result = db.execute(query, {
            'doc_id': doc_id,
            'user_id': current_user.id,
        }).fetchall()

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

        # Extract entities in memory FIRST so that a failure here happens
        # before any relationship rows are deleted.
        extracted: List[Dict] = []
        for chunk in chunks:
            entities, _ = entity_extractor.extract_from_chunk(
                chunk['text'],
                chunk['metadata']
            )
            for entity in entities:
                extracted.append({
                    "entity": entity,
                    "chunk_id": chunk['metadata']['chunk_id'],
                })

        # Clear old graph data for this doc and rebuild it — ALL within the
        # endpoint's `db` session, using the atomic branch of GraphStore
        # (session=db). Every write lands in one transaction and is committed
        # once at the end; any error rolls the whole rebuild back so the old
        # graph is never left half-deleted.
        # The DELETEs are additionally scoped to the current user's documents.
        delete_query = text("""
            DELETE FROM chunk_relationships
            WHERE source_chunk_id IN (
                SELECT dc.chunk_id
                FROM document_chunks dc
                JOIN documents d ON d.id = dc.doc_id
                WHERE dc.doc_id = :doc_id AND d.user_id = :user_id
            )
        """)
        db.execute(delete_query, {
            'doc_id': doc_id,
            'user_id': current_user.id,
        })

        delete_entity_query = text("""
            DELETE FROM entity_chunk_occurrences eco
            USING documents d
            WHERE eco.doc_id = d.id
              AND eco.doc_id = :doc_id
              AND d.user_id = :user_id
        """)
        db.execute(delete_entity_query, {
            'doc_id': doc_id,
            'user_id': current_user.id,
        })

        # Clear stale entity_relationships (co-occurrence/mREBEL edges) whose
        # evidence chunk_ids point into THIS user-owned document. Plain
        # additions/linking below would otherwise leave old edges visible in
        # GET /api/rag/graph even when the rebuilt occurrences no longer
        # support them. Only edges evidenced by this doc are removed, so
        # relationships supported exclusively by other documents survive.
        delete_relationships_query = text("""
            DELETE FROM entity_relationships er
            WHERE EXISTS (
                SELECT 1
                FROM jsonb_array_elements_text(er.relationship_metadata->'chunk_ids') j(chunk_id)
                JOIN document_chunks dc ON dc.chunk_id = j.chunk_id
                JOIN documents d ON d.id = dc.doc_id
                WHERE dc.doc_id = :doc_id AND d.user_id = :user_id
            )
        """)
        db.execute(delete_relationships_query, {
            'doc_id': doc_id,
            'user_id': current_user.id,
        })

        # Rebuild sequential relationships (same transaction as the deletes).
        graph_store.build_sequential_relationships(doc_id, len(chunks), session=db)

        # Re-create entities and chunk links (same transaction).
        entities_count = 0
        for item in extracted:
            entity = item["entity"]
            entity_id = graph_store.add_entity(
                entity['text'],
                entity['type'],
                entity['canonical_form'],
                session=db,
            )
            graph_store.link_entity_to_chunk(
                entity_id,
                item['chunk_id'],
                doc_id,
                session=db,
            )
            entities_count += 1

        # Build co-occurrence relationships (same transaction).
        logger.info(f"Building co-occurrence relationships for doc_id={doc_id}")
        cooccurrence_count = graph_store.build_cooccurrence_relationships(
            doc_id=doc_id,
            min_cooccurrence=2,
            session=db,
        )

        # Commit the whole rebuild atomically.
        db.commit()

        return {
            "status": "success",
            "doc_id": doc_id,
            "chunks_processed": len(chunks),
            "entities_extracted": entities_count,
            "cooccurrence_relationships": cooccurrence_count,
            "message": "Knowledge graph rebuilt successfully"
        }

    except HTTPException:
        # Let client errors (404 document not found, etc.) pass through as-is
        # instead of being swallowed by the generic 500 handler.
        raise
    except Exception as e:
        # Roll back the endpoint transaction so a failed rebuild leaves the
        # previous graph intact.
        db.rollback()
        logger.error(f"Graph rebuild failed for doc {doc_id}: {e}")
        raise HTTPException(
            status_code=500,
            detail=f"Graph rebuild failed: {str(e)}"
        )


@router.post("/documents/bulk-rebuild-graph")
async def bulk_rebuild_graph(
    doc_ids: List[str],
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
):
    """
    Trigger re-graphing for multiple documents.
    """
    results = []
    for doc_id in doc_ids:
        try:
            result = await rebuild_graph(doc_id, db, current_user)
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
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
):
    """
    List all chunks with pagination and optional filtering.
    """
    offset = (page - 1) * limit

    # Build query — every clause is scoped to the current user's documents via
    # the join on `documents` (d.user_id).
    where_clauses = []
    params = {'limit': limit, 'offset': offset}

    where_clauses.append("d.user_id = :user_id")
    params['user_id'] = current_user.id

    if doc_id:
        where_clauses.append("dc.doc_id = :doc_id")
        params['doc_id'] = doc_id

    if search:
        where_clauses.append("dc.chunk_text ILIKE :search")
        params['search'] = f"%{search}%"

    where_sql = "WHERE " + " AND ".join(where_clauses)

    # Count total (only this user's chunks)
    count_query = text(f"""
        SELECT COUNT(*)
        FROM document_chunks dc
        JOIN documents d ON d.id = dc.doc_id
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
        JOIN documents d ON d.id = dc.doc_id
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
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
):
    """
    Get full details for a specific chunk including relationships.
    """
    # Get chunk data — the owner of the chunk's document must be the user.
    chunk_query = text("""
        SELECT
            dc.chunk_id,
            dc.chunk_text,
            dc.chunk_metadata,
            dc.doc_id,
            d.original_filename,
            d.metadata_->>'title'
        FROM document_chunks dc
        JOIN documents d ON dc.doc_id = d.id
        WHERE dc.chunk_id = :chunk_id
          AND d.user_id = :user_id
    """)
    chunk_result = db.execute(chunk_query, {
        'chunk_id': chunk_id,
        'user_id': current_user.id,
    }).fetchone()

    if not chunk_result:
        raise HTTPException(status_code=404, detail="Chunk not found")

    # Get relationships — restrict to chunks whose documents belong to the user.
    rel_query = text("""
        SELECT
            cr.target_chunk_id,
            cr.relationship_type,
            cr.strength,
            dc.chunk_text
        FROM chunk_relationships cr
        JOIN document_chunks sc ON sc.chunk_id = cr.source_chunk_id
        JOIN documents sd ON sd.id = sc.doc_id
        JOIN document_chunks dc ON cr.target_chunk_id = dc.chunk_id
        JOIN documents td ON td.id = dc.doc_id
        WHERE cr.source_chunk_id = :chunk_id
          AND sd.user_id = :user_id
          AND td.user_id = :user_id
        ORDER BY cr.strength DESC
    """)
    relationships = db.execute(rel_query, {
        'chunk_id': chunk_id,
        'user_id': current_user.id,
    }).fetchall()

    # Get entities — restrict to entities occurring in the user's own chunks.
    entity_query = text("""
        SELECT
            de.entity_text,
            de.entity_type,
            eco.occurrence_count,
            eco.relevance_score
        FROM entity_chunk_occurrences eco
        JOIN document_entities de ON eco.entity_id = de.id
        JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id
        JOIN documents d ON d.id = dc.doc_id
        WHERE eco.chunk_id = :chunk_id
          AND d.user_id = :user_id
        ORDER BY eco.relevance_score DESC
    """)
    entities = db.execute(entity_query, {
        'chunk_id': chunk_id,
        'user_id': current_user.id,
    }).fetchall()

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
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
):
    """
    Get knowledge graph data (nodes and edges) for visualization.
    Returns entity nodes and relationship edges in a format suitable for D3.js or Cytoscape.
    """
    where_clauses = []
    params = {'min_strength': min_strength, 'limit': limit}

    # Scoped to the current user's documents via the chain
    # entity_chunk_occurrences -> document_chunks -> documents(user_id).
    where_clauses.append("d.user_id = :user_id")
    params['user_id'] = current_user.id

    if doc_id:
        where_clauses.append("eco.doc_id = :doc_id")
        params['doc_id'] = doc_id

    if entity_types:
        where_clauses.append("de.entity_type = ANY(:types)")
        params['types'] = entity_types

    entity_where = "WHERE " + " AND ".join(where_clauses)

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
        JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id
        JOIN documents d ON d.id = dc.doc_id
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

    # Get relationship edges between these entities. Edges are only returned if
    # their evidence (chunk_ids recorded in relationship_metadata) can be proven
    # to belong to the current user's documents. This prevents leaking a
    # relationship that was only ever observed inside another user's corpus even
    # though both canonical entity rows exist globally.
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
          AND EXISTS (
            SELECT 1
            FROM jsonb_array_elements_text(er.relationship_metadata->'chunk_ids') j(chunk_id)
            JOIN document_chunks dc ON dc.chunk_id = j.chunk_id
            JOIN documents d ON d.id = dc.doc_id
            WHERE d.user_id = :user_id
              AND (:doc_id IS NULL OR dc.doc_id = CAST(:doc_id AS uuid))
          )
        ORDER BY er.relationship_strength DESC
        LIMIT :limit
    """)
    edges_result = db.execute(edges_query, {
        'entity_ids': entity_ids,
        'min_strength': min_strength,
        'limit': limit,
        'user_id': current_user.id,
        'doc_id': doc_id,
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
    db=Depends(get_db),
    current_user: User = Depends(get_current_user_from_cookie),
):
    """
    List entities with pagination and filtering.
    """
    offset = (page - 1) * limit
    # Entities are scoped to the current user's documents: only entities that
    # occur in document_chunks owned by this user are visible.
    scoped_join = (
        " FROM document_entities de "
        " JOIN entity_chunk_occurrences eco ON eco.entity_id = de.id "
        " JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id "
        " JOIN documents d ON d.id = dc.doc_id "
    )
    where_clauses = ["d.user_id = :user_id"]
    params = {'limit': limit, 'offset': offset, 'user_id': current_user.id}

    if entity_type:
        where_clauses.append("de.entity_type = :entity_type")
        params['entity_type'] = entity_type

    if search:
        where_clauses.append("de.entity_text ILIKE :search")
        params['search'] = f"%{search}%"

    where_sql = "WHERE " + " AND ".join(where_clauses)

    count_query = text(f"SELECT COUNT(DISTINCT de.id){scoped_join}{where_sql}")
    total_count = db.execute(count_query, params).scalar()

    query = text(f"""
        SELECT
            de.id,
            de.entity_text,
            de.entity_type,
            de.canonical_form,
            (SELECT COUNT(*) FROM entity_chunk_occurrences e2
              JOIN document_chunks dc2 ON dc2.chunk_id = e2.chunk_id
              JOIN documents d2 ON d2.id = dc2.doc_id
              WHERE e2.entity_id = de.id
                AND d2.user_id = :user_id) as occurrence_count
        FROM document_entities de
        JOIN entity_chunk_occurrences eco ON eco.entity_id = de.id
        JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id
        JOIN documents d ON d.id = dc.doc_id
        {where_sql}
        GROUP BY de.id, de.entity_text, de.entity_type, de.canonical_form
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
