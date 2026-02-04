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
        embedding: Optional[list] = None
    ) -> str:
        """Add or update entity, return entity_id."""
        db = next(get_db())
        try:
            query = text("""
                INSERT INTO document_entities
                    (entity_text, entity_type, canonical_form, description, embedding)
                VALUES (:text, :type, :canonical, :desc, :emb)
                ON CONFLICT (canonical_form, entity_type)
                DO UPDATE SET
                    entity_text = EXCLUDED.entity_text,
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
            db.commit()

            return str(result.fetchone()[0])
        except Exception as e:
            db.rollback()
            logger.error(f"Failed to add entity: {e}")
            raise
        finally:
            db.close()

    def link_entity_to_chunk(
        self,
        entity_id: str,
        chunk_id: str,
        doc_id: str,
        occurrence_count: int = 1,
        context_snippet: Optional[str] = None,
        relevance_score: float = 0.5
    ):
        """Link entity to chunk where it appears."""
        db = next(get_db())
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
            db.commit()
        except Exception as e:
            db.rollback()
            logger.error(f"Failed to link entity to chunk: {e}")
            raise
        finally:
            db.close()

    def add_chunk_relationship(
        self,
        source_chunk_id: str,
        target_chunk_id: str,
        relationship_type: str,
        strength: float,
        metadata: Optional[Dict] = None
    ):
        """Add relationship between chunks."""
        db = next(get_db())
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
            db.commit()
        except Exception as e:
            db.rollback()
            logger.error(f"Failed to add chunk relationship: {e}")
            raise
        finally:
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

    def build_sequential_relationships(self, doc_id: str, chunk_count: int):
        """Build next/previous relationships for document chunks."""
        relationships = []
        for i in range(chunk_count - 1):
            relationships.append({
                'source': f"{doc_id}_chunk_{i:04d}",
                'target': f"{doc_id}_chunk_{i+1:04d}",
                'type': 'sequential_next',
                'strength': 0.85,
                'metadata': {}
            })

        # Batch insert
        db = next(get_db())
        try:
            for rel in relationships:
                self.add_chunk_relationship(
                    rel['source'], rel['target'],
                    rel['type'], rel['strength'], rel['metadata']
                )
        except Exception as e:
            logger.error(f"Failed to build sequential relationships: {e}")
        finally:
            db.close()
