"""
Graph Store for Knowledge Graph

Manages entity and chunk relationship graphs in PostgreSQL.
"""

from typing import List, Tuple, Dict, Optional
from sqlalchemy import text
from database.database import get_db
from dataclasses import dataclass
import logging

logger = logging.getLogger(__name__)


@dataclass
class Relationship:
    type: str
    strength: float
    metadata: Dict


class GraphStore:
    """
    Manages entity and chunk relationship graphs in PostgreSQL.
    """

    def add_entity(
        self,
        entity_text: str,
        entity_type: str,
        canonical_form: str,
        description: Optional[str] = None,
        embedding: Optional[list] = None,
        session=None,
    ) -> str:
        """Add or update entity with description accumulation, return entity_id.

        If ``session`` is provided (an existing SQLAlchemy session/connection), it
        is reused and NO commit is issued — the caller owns the transaction so a
        multi-step rebuild can be committed/rolled back atomically. Otherwise a
        fresh session is opened and committed as before.
        """
        # Truncate text fields to fit database column constraints
        entity_text = entity_text[:255] if entity_text else entity_text
        canonical_form = canonical_form[:255] if canonical_form else canonical_form
        description = description[:2000] if description else description

        own_session = session is None
        db = session if session is not None else next(get_db())
        try:
            # Updated SQL to accumulate descriptions on conflict (capped at 2000 chars)
            query = text("""
                INSERT INTO document_entities
                    (entity_text, entity_type, canonical_form, description, embedding)
                VALUES (:text, :type, :canonical, :desc, :emb)
                ON CONFLICT (canonical_form, entity_type)
                DO UPDATE SET
                    entity_text = EXCLUDED.entity_text,
                    description = CASE
                        WHEN document_entities.description IS NULL THEN EXCLUDED.description
                        WHEN EXCLUDED.description IS NULL THEN document_entities.description
                        ELSE SUBSTR(document_entities.description || ' | ' || EXCLUDED.description, 1, 2000)
                    END,
                    updated_at = CURRENT_TIMESTAMP
                RETURNING id
            """)

            result = db.execute(query, {
                'text': entity_text,
                'type': entity_type,
                'canonical': canonical_form,
                'desc': description,
                'emb': embedding
            })
            if own_session:
                db.commit()

            return str(result.fetchone()[0])
        except Exception as e:
            # When the caller supplied its own session, the transaction belongs
            # to the caller — let the exception propagate so they can roll back
            # the whole unit of work themselves.
            if own_session:
                db.rollback()
            logger.error(f"Failed to add entity: {e}")
            raise
        finally:
            if own_session:
                db.close()

    def summarize_entity_descriptions(self, llm_client=None) -> int:
        """
        Consolidate accumulated entity descriptions using LLM.
        Query entities with 3+ description fragments and summarize into single description.

        Args:
            llm_client: Optional LLM client for summarization. If None, uses heuristic.

        Returns:
            Number of entities updated
        """
        db = next(get_db())
        try:
            # Find entities with multiple description fragments (split by ' | ')
            query = text("""
                SELECT id, entity_text, entity_type, canonical_form, description
                FROM document_entities
                WHERE description IS NOT NULL
                  AND description LIKE '% | %'
                  AND (LENGTH(description) - LENGTH(REPLACE(description, ' | ', ''))) >= 4
            """)

            results = db.execute(query).fetchall()
            updated_count = 0

            for row in results:
                entity_id = row[0]
                entity_text = row[1]
                entity_type = row[2]
                description = row[4]

                # Split fragments
                fragments = description.split(' | ')

                if len(fragments) < 3:
                    continue

                if llm_client:
                    # Use LLM to consolidate
                    try:
                        consolidated = self._llm_consolidate(
                            llm_client, entity_text, entity_type, fragments
                        )
                    except Exception as e:
                        logger.warning(f"LLM consolidation failed for entity {entity_id}: {e}")
                        # Fallback to heuristic
                        consolidated = self._heuristic_consolidate(fragments)
                else:
                    # Use heuristic: keep first 3 unique fragments
                    consolidated = self._heuristic_consolidate(fragments)

                # Update entity
                update_query = text("""
                    UPDATE document_entities
                    SET description = :description, updated_at = CURRENT_TIMESTAMP
                    WHERE id = :id
                """)
                db.execute(update_query, {'id': entity_id, 'description': consolidated})
                updated_count += 1

            db.commit()
            logger.info(f"Summarized descriptions for {updated_count} entities")
            return updated_count

        except Exception as e:
            db.rollback()
            logger.error(f"Failed to summarize entity descriptions: {e}")
            return 0
        finally:
            db.close()

    def _heuristic_consolidate(self, fragments: List[str], max_length: int = 500) -> str:
        """Consolidate fragments using heuristic: keep unique, truncate."""
        seen = set()
        unique = []
        for frag in fragments:
            frag_lower = frag.lower().strip()
            if frag_lower not in seen and len(frag.strip()) > 10:
                seen.add(frag_lower)
                unique.append(frag.strip())

        result = ' | '.join(unique[:5])  # Keep max 5 unique fragments
        return result[:max_length] if len(result) > max_length else result

    def _llm_consolidate(
        self,
        llm_client,
        entity_text: str,
        entity_type: str,
        fragments: List[str]
    ) -> str:
        """Use LLM to consolidate description fragments."""
        prompt = f"""Consolidate the following description fragments about the entity "{entity_text}" (type: {entity_type}) into a single, coherent description. Remove redundancy and keep key information.

Fragments:
{chr(10).join(f'- {f}' for f in fragments[:10])}

Return only the consolidated description (max 500 chars):"""

        # This is a placeholder - actual implementation depends on LLM client interface
        # Adjust based on your LLM client's API
        try:
            if hasattr(llm_client, 'chat'):
                response = llm_client.chat([{"role": "user", "content": prompt}])
                return response.choices[0].message.content[:500]
            else:
                # Fallback to heuristic
                return self._heuristic_consolidate(fragments)
        except Exception:
            return self._heuristic_consolidate(fragments)

    def link_entity_to_chunk(
        self,
        entity_id: str,
        chunk_id: str,
        doc_id: str,
        occurrence_count: int = 1,
        context_snippet: Optional[str] = None,
        relevance_score: float = 0.5,
        session=None,
    ):
        """Link entity to chunk where it appears.

        Reuses ``session`` (no commit) when provided so callers can run a
        rebuild atomically; otherwise opens/commits/closes its own session.
        """
        own_session = session is None
        db = session if session is not None else next(get_db())
        try:
            query = text("""
                INSERT INTO entity_chunk_occurrences
                    (entity_id, chunk_id, doc_id, occurrence_count,
                     context_snippet, relevance_score)
                VALUES (:entity_id, :chunk_id, :doc_id, :count, :context, :relevance)
                ON CONFLICT (entity_id, chunk_id)
                DO UPDATE SET
                    occurrence_count = entity_chunk_occurrences.occurrence_count + 1
            """)

            db.execute(query, {
                'entity_id': entity_id,
                'chunk_id': chunk_id,
                'doc_id': doc_id,
                'count': occurrence_count,
                'context': context_snippet,
                'relevance': relevance_score
            })
            if own_session:
                db.commit()
        except Exception as e:
            if own_session:
                db.rollback()
            logger.error(f"Failed to link entity to chunk: {e}")
            raise
        finally:
            if own_session:
                db.close()

    def add_chunk_relationship(
        self,
        source_chunk_id: str,
        target_chunk_id: str,
        relationship_type: str,
        strength: float,
        metadata: Optional[Dict] = None,
        session=None,
    ):
        """Add relationship between chunks.

        Reuses ``session`` (no commit) when provided so callers can run a
        rebuild atomically; otherwise opens/commits/closes its own session.
        """
        own_session = session is None
        db = session if session is not None else next(get_db())
        try:
            import json
            query = text("""
                INSERT INTO chunk_relationships
                    (source_chunk_id, target_chunk_id, relationship_type, strength, metadata)
                VALUES (:source, :target, :type, :strength, CAST(:metadata AS jsonb))
                ON CONFLICT (source_chunk_id, target_chunk_id, relationship_type)
                DO UPDATE SET strength = EXCLUDED.strength
            """)

            db.execute(query, {
                'source': source_chunk_id,
                'target': target_chunk_id,
                'type': relationship_type,
                'strength': strength,
                'metadata': json.dumps(metadata or {})
            })
            if own_session:
                db.commit()
        except Exception as e:
            if own_session:
                db.rollback()
            logger.error(f"Failed to add chunk relationship: {e}")
            raise
        finally:
            if own_session:
                db.close()

    def get_chunk_neighbors(
        self,
        chunk_id: str,
        relationship_types: Optional[List[str]] = None,
        min_strength: float = 0.3,
        max_neighbors: int = 20
    ) -> List[Tuple[str, Relationship]]:
        """Get neighboring chunks via relationships."""
        db = next(get_db())
        try:
            query_str = """
                SELECT target_chunk_id, relationship_type, strength, metadata
                FROM chunk_relationships
                WHERE source_chunk_id = :chunk_id
                  AND strength >= :min_strength
            """

            params = {
                'chunk_id': chunk_id,
                'min_strength': min_strength,
                'max_neighbors': max_neighbors
            }

            if relationship_types:
                query_str += " AND relationship_type = ANY(:types)"
                params['types'] = relationship_types

            query_str += " ORDER BY strength DESC LIMIT :max_neighbors"

            results = db.execute(text(query_str), params).fetchall()

            return [
                (row[0], Relationship(
                    type=row[1],
                    strength=row[2],
                    metadata=row[3]
                ))
                for row in results
            ]
        except Exception as e:
            logger.error(f"Failed to get chunk neighbors: {e}")
            return []
        finally:
            db.close()

    def build_sequential_relationships(self, doc_id: str, chunk_count: int, session=None):
        """Build next/previous relationships for document chunks.

        When ``session`` is provided it is forwarded to add_chunk_relationship so
        everything lands in the same transaction as the caller.
        """
        relationships = []
        for i in range(chunk_count - 1):
            relationships.append({
                'source': f"{doc_id}_chunk_{i:04d}",
                'target': f"{doc_id}_chunk_{i+1:04d}",
                'type': 'sequential_next',
                'strength': 0.85,
                'metadata': {}
            })

        try:
            for rel in relationships:
                self.add_chunk_relationship(
                    rel['source'], rel['target'],
                    rel['type'], rel['strength'], rel['metadata'],
                    session=session,
                )
        except Exception as e:
            logger.error(f"Failed to build sequential relationships: {e}")
            raise

    def build_cooccurrence_relationships(self, doc_id: Optional[str] = None, min_cooccurrence: int = 2, session=None):
        """
        Build entity relationships based on co-occurrence in chunks.
        Creates relationships between entities that appear together frequently.

        Args:
            doc_id: Optional document ID to limit to specific document
            min_cooccurrence: Minimum number of co-occurrences to create relationship
            session: Optional SQLAlchemy session to reuse (no commit) for atomic
                multi-step operations.
        """
        own_session = session is None
        db = session if session is not None else next(get_db())
        try:
            # Find entity pairs that co-occur in chunks
            query = text("""
                WITH entity_pairs AS (
                    SELECT
                        e1.entity_id as source_entity_id,
                        e2.entity_id as target_entity_id,
                        COUNT(DISTINCT e1.chunk_id) as cooccurrence_count,
                        ARRAY_AGG(DISTINCT e1.chunk_id) as chunk_ids
                    FROM entity_chunk_occurrences e1
                    JOIN entity_chunk_occurrences e2
                        ON e1.chunk_id = e2.chunk_id
                        AND e1.entity_id < e2.entity_id
                    WHERE (:doc_id IS NULL OR e1.doc_id = CAST(:doc_id AS uuid))
                    GROUP BY e1.entity_id, e2.entity_id
                    HAVING COUNT(DISTINCT e1.chunk_id) >= :min_cooccurrence
                )
                INSERT INTO entity_relationships
                    (source_entity_id, target_entity_id, relationship_type, relationship_strength, evidence_count, relationship_metadata)
                SELECT
                    source_entity_id,
                    target_entity_id,
                    'CO_OCCURS',
                    LEAST(1.0, CAST(cooccurrence_count AS FLOAT) / 10.0) as strength,
                    cooccurrence_count,
                    jsonb_build_object('chunk_ids', chunk_ids, 'source', 'cooccurrence')
                FROM entity_pairs
                ON CONFLICT (source_entity_id, target_entity_id, relationship_type)
                DO UPDATE SET
                    relationship_strength = EXCLUDED.relationship_strength,
                    evidence_count = EXCLUDED.evidence_count,
                    relationship_metadata = EXCLUDED.relationship_metadata
                RETURNING source_entity_id, target_entity_id, relationship_strength
            """)

            result = db.execute(query, {
                'doc_id': doc_id,
                'min_cooccurrence': min_cooccurrence
            })
            rows = result.fetchall()
            if own_session:
                db.commit()

            logger.info(f"Created {len(rows)} co-occurrence relationships for doc_id={doc_id}")
            return len(rows)

        except Exception as e:
            if own_session:
                db.rollback()
            logger.error(f"Failed to build co-occurrence relationships: {e}")
            raise
        finally:
            if own_session:
                db.close()

    def add_entity_relationship(
        self,
        source_entity_id: str,
        target_entity_id: str,
        relationship_type: str,
        strength: float,
        evidence_chunks: Optional[List[str]] = None,
        source: str = "llm",
        session=None,
    ):
        """
        Add relationship between entities (from LLM extraction).

        Args:
            source_entity_id: Source entity UUID
            target_entity_id: Target entity UUID
            relationship_type: Type of relationship (CITES, USES, etc.)
            strength: Relationship strength (0.0-1.0)
            evidence_chunks: List of chunk IDs that support this relationship
            source: Source of relationship ('llm' or 'cooccurrence')
            session: Optional SQLAlchemy session to reuse (no commit) for
                atomic multi-step operations.
        """
        own_session = session is None
        db = session if session is not None else next(get_db())
        try:
            import json
            query = text("""
                INSERT INTO entity_relationships
                    (source_entity_id, target_entity_id, relationship_type,
                     relationship_strength, evidence_count, relationship_metadata)
                VALUES (
                    CAST(:source AS uuid),
                    CAST(:target AS uuid),
                    :type,
                    :strength,
                    :evidence_count,
                    CAST(:metadata AS jsonb)
                )
                ON CONFLICT (source_entity_id, target_entity_id, relationship_type)
                DO UPDATE SET
                    relationship_strength = GREATEST(
                        entity_relationships.relationship_strength,
                        EXCLUDED.relationship_strength
                    ),
                    evidence_count = entity_relationships.evidence_count + EXCLUDED.evidence_count
                RETURNING id
            """)

            result = db.execute(query, {
                'source': source_entity_id,
                'target': target_entity_id,
                'type': relationship_type,
                'strength': strength,
                'evidence_count': len(evidence_chunks or []),
                'metadata': json.dumps({
                    'source': source,
                    'chunk_ids': evidence_chunks or []
                })
            })
            if own_session:
                db.commit()

            return str(result.fetchone()[0])

        except Exception as e:
            if own_session:
                db.rollback()
            logger.error(f"Failed to add entity relationship: {e}")
            raise
        finally:
            if own_session:
                db.close()
