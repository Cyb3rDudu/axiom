"""
Graph-Enhanced Retriever

Combines vector search with knowledge graph traversal for cross-document
entity-based chunk discovery. Uses entity relationships (from mREBEL) to
find related chunks that vector similarity alone would miss.
"""

from typing import List, Dict, Any, Optional, Set, Tuple
from collections import deque
import asyncio
import logging

logger = logging.getLogger(__name__)


class GraphEnhancedRetriever:
    """
    Enhanced retrieval combining vector search with entity graph expansion.

    Flow:
    1. Vector search for seed chunks
    2. Extract entities from seed chunks (via entity_chunk_occurrences)
    3. Walk entity_relationships to find related entities
    4. Find chunks containing related entities
    5. Merge and rerank all candidates
    """

    def __init__(
        self,
        base_retriever,
        graph_store,
        max_depth: int = 2,
        vector_weight: float = 0.6,
        graph_weight: float = 0.3,
        decay_factor: float = 0.6,
        min_relationship_strength: float = 0.3,
        **kwargs,
    ):
        self.base_retriever = base_retriever
        self.graph_store = graph_store
        self.max_depth = max_depth
        self.vector_weight = vector_weight
        self.graph_weight = graph_weight
        self.decay_factor = decay_factor
        self.min_relationship_strength = min_relationship_strength

    async def retrieve(
        self,
        query_text: str,
        n_results: int = 5,
        filter_metadata: Optional[Dict[str, Any]] = None,
        use_graph: bool = True,
        **kwargs,
    ) -> List[Dict[str, Any]]:
        """Retrieve with optional entity graph enhancement."""
        # Phase 1: Vector search for seeds
        seed_size = min(n_results * 2, 15)
        seed_results = await self.base_retriever.retrieve(
            query_text=query_text,
            n_results=seed_size,
            filter_metadata=filter_metadata,
            use_reranker=False,
            use_graph=False,
            **kwargs,
        )

        if not use_graph or not seed_results:
            return seed_results[:n_results]

        # Phase 2a: Query-level entity extraction → direct cross-document lookup
        query_entity_chunks = await asyncio.to_thread(
            self._find_chunks_by_query_entities,
            query_text,
            {c["id"] for c in seed_results},
            max_results=n_results,
        )

        # Phase 2b: Seed-based entity graph expansion (existing)
        graph_chunks = await asyncio.to_thread(
            self._expand_via_entities,
            seed_results,
            max_extra=n_results,
        )

        # Phase 3: Merge seeds + query entity chunks + graph-discovered chunks
        candidates = {}
        for chunk in seed_results:
            candidates[chunk["id"]] = chunk

        for chunk in query_entity_chunks:
            if chunk["id"] not in candidates:
                candidates[chunk["id"]] = chunk

        for chunk in graph_chunks:
            if chunk["id"] not in candidates:
                candidates[chunk["id"]] = chunk

        merged = list(candidates.values())
        logger.info(
            f"Graph expansion: {len(seed_results)} seeds + "
            f"{len(query_entity_chunks)} query-entity + "
            f"{len(graph_chunks)} graph → {len(merged)} merged"
        )

        # Phase 4: Rerank merged set
        if self.base_retriever.reranker and len(merged) > n_results:
            try:
                reranked = await asyncio.to_thread(
                    self.base_retriever.reranker.rerank,
                    query_text,
                    merged[:n_results * 3],
                    top_n=n_results,
                )
                return [item for _, item in reranked]
            except Exception as e:
                logger.error(f"Reranking failed: {e}")

        return merged[:n_results]

    def _find_chunks_by_query_entities(
        self,
        query_text: str,
        exclude_chunk_ids: set,
        max_results: int = 10,
    ) -> List[Dict]:
        """
        Run GLiNER on the query text to extract entities, then find chunks
        containing those entities across ALL documents. This ensures
        cross-document coverage regardless of vector similarity.
        """
        from database.database import get_db
        from sqlalchemy import text

        try:
            from .entity_extractor import EntityExtractor, GLINER_AVAILABLE, _get_gliner_model
            if not GLINER_AVAILABLE:
                return []

            gliner = _get_gliner_model()
            if not gliner:
                return []

            # Extract entities from the query (fast, ~5ms)
            from .entity_extractor import GLINER_LABELS
            raw_entities = gliner.predict_entities(
                query_text, GLINER_LABELS, threshold=0.4, multi_label=False,
            )
            if not raw_entities:
                return []

            entity_texts = [e["text"].lower().strip() for e in raw_entities if len(e["text"]) >= 2]
            if not entity_texts:
                return []

            logger.info(f"Query entities: {entity_texts}")

            # Find matching entities in the knowledge graph by canonical_form
            db = next(get_db())
            try:
                # Match query entities against canonical_form
                conditions = " OR ".join(f"canonical_form ILIKE :e{i}" for i in range(len(entity_texts)))
                params = {f"e{i}": f"%{et}%" for i, et in enumerate(entity_texts)}
                params["limit"] = max_results

                entity_rows = db.execute(text(f"""
                    SELECT DISTINCT id::text FROM document_entities
                    WHERE {conditions}
                    LIMIT 50
                """), params).fetchall()

                if not entity_rows:
                    return []

                entity_ids = [r[0] for r in entity_rows]

                # Find chunks containing these entities
                eid_placeholders = ", ".join(f":eid{i}" for i in range(len(entity_ids)))
                eid_params = {f"eid{i}": eid for i, eid in enumerate(entity_ids)}

                # Exclude seed chunks
                excl_parts = []
                for i, cid in enumerate(exclude_chunk_ids):
                    eid_params[f"excl{i}"] = cid
                    excl_parts.append(f":excl{i}")
                excl_clause = f"AND dc.chunk_id NOT IN ({', '.join(excl_parts)})" if excl_parts else ""

                eid_params["limit"] = max_results

                rows = db.execute(text(f"""
                    SELECT DISTINCT ON (dc.chunk_id)
                        dc.chunk_id, dc.chunk_text, dc.chunk_metadata, dc.doc_id::text
                    FROM entity_chunk_occurrences eco
                    JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id
                    WHERE eco.entity_id::text IN ({eid_placeholders})
                    {excl_clause}
                    LIMIT :limit
                """), eid_params).fetchall()

                chunks = []
                for row in rows:
                    meta = row[2] or {}
                    meta["doc_id"] = row[3]
                    chunks.append({
                        "id": row[0],
                        "text": row[1],
                        "metadata": meta,
                        "doc_id": row[3],
                        "score": 0.45,
                    })

                logger.info(f"Query-entity lookup: {len(entity_texts)} entities → {len(chunks)} cross-doc chunks")
                return chunks

            finally:
                db.close()

        except Exception as e:
            logger.error(f"Query-entity chunk lookup failed: {e}")
            return []

    def _expand_via_entities(
        self,
        seed_chunks: List[Dict],
        max_extra: int = 20,
    ) -> List[Dict]:
        """
        Find additional chunks by walking the entity relationship graph.

        1. Get entities from seed chunks
        2. Find related entities (1 hop via entity_relationships)
        3. Find chunks containing those related entities
        4. Return chunks not already in seeds
        """
        from database.database import get_db
        from sqlalchemy import text

        seed_chunk_ids = {c["id"] for c in seed_chunks}
        if not seed_chunk_ids:
            return []

        db = next(get_db())
        try:
            # Step 1: Get entity IDs from seed chunks
            placeholders = ", ".join(f":cid{i}" for i in range(len(seed_chunk_ids)))
            params = {f"cid{i}": cid for i, cid in enumerate(seed_chunk_ids)}

            seed_entities = db.execute(text(f"""
                SELECT DISTINCT entity_id
                FROM entity_chunk_occurrences
                WHERE chunk_id IN ({placeholders})
            """), params).fetchall()

            if not seed_entities:
                logger.debug("No entities found in seed chunks")
                return []

            seed_entity_ids = [str(r[0]) for r in seed_entities]
            logger.debug(f"Found {len(seed_entity_ids)} entities in {len(seed_chunk_ids)} seed chunks")

            # Step 2: Find related entities via entity_relationships (1 hop)
            eid_placeholders = ", ".join(f":eid{i}" for i in range(len(seed_entity_ids)))
            eid_params = {f"eid{i}": eid for i, eid in enumerate(seed_entity_ids)}
            eid_params["min_strength"] = self.min_relationship_strength

            related_entities = db.execute(text(f"""
                SELECT DISTINCT
                    CASE
                        WHEN source_entity_id::text IN ({eid_placeholders}) THEN target_entity_id
                        ELSE source_entity_id
                    END as related_id,
                    relationship_strength
                FROM entity_relationships
                WHERE (source_entity_id::text IN ({eid_placeholders})
                    OR target_entity_id::text IN ({eid_placeholders}))
                AND relationship_strength >= :min_strength
                LIMIT 100
            """), eid_params).fetchall()

            if not related_entities:
                logger.debug("No related entities found")
                return []

            related_ids = [str(r[0]) for r in related_entities]
            logger.debug(f"Found {len(related_ids)} related entities")

            # Step 3: Find chunks containing related entities (excluding seeds)
            rid_placeholders = ", ".join(f":rid{i}" for i in range(len(related_ids)))
            rid_params = {f"rid{i}": rid for i, rid in enumerate(related_ids)}
            # Also exclude seed chunk IDs
            excl_placeholders = ", ".join(f":excl{i}" for i in range(len(seed_chunk_ids)))
            rid_params.update({f"excl{i}": cid for i, cid in enumerate(seed_chunk_ids)})
            rid_params["limit"] = max_extra

            graph_chunks_rows = db.execute(text(f"""
                SELECT DISTINCT ON (dc.chunk_id)
                    dc.chunk_id,
                    dc.chunk_text,
                    dc.chunk_metadata,
                    dc.doc_id::text
                FROM entity_chunk_occurrences eco
                JOIN document_chunks dc ON dc.chunk_id = eco.chunk_id
                WHERE eco.entity_id::text IN ({rid_placeholders})
                AND eco.chunk_id NOT IN ({excl_placeholders})
                LIMIT :limit
            """), rid_params).fetchall()

            chunks = []
            for row in graph_chunks_rows:
                meta = row[2] or {}
                # Ensure doc_id is in metadata for downstream enrichment
                meta["doc_id"] = row[3]
                chunks.append({
                    "id": row[0],
                    "text": row[1],
                    "metadata": meta,
                    "doc_id": row[3],
                    "score": 0.4,  # graph-discovered score (will be reranked)
                })

            logger.info(f"Entity graph expansion found {len(chunks)} additional chunks")
            return chunks

        except Exception as e:
            logger.error(f"Entity graph expansion failed: {e}")
            return []
        finally:
            db.close()
