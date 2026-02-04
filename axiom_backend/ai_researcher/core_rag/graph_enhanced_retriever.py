"""
Graph-Enhanced Retriever

Enhanced retrieval combining vector search with graph traversal.
"""

from typing import List, Dict, Any, Optional, Set, Tuple
import asyncio
import logging
from .graph_store import GraphStore, Relationship

logger = logging.getLogger(__name__)


class GraphEnhancedRetriever:
    """
    Enhanced retrieval combining vector search with graph traversal.
    """

    def __init__(
        self,
        base_retriever,
        graph_store: GraphStore,
        max_depth: int = 2,
        vector_weight: float = 0.6,
        graph_weight: float = 0.3,
        diversity_weight: float = 0.1,
        decay_factor: float = 0.6
    ):
        self.base_retriever = base_retriever
        self.graph_store = graph_store
        self.max_depth = max_depth
        self.vector_weight = vector_weight
        self.graph_weight = graph_weight
        self.diversity_weight = diversity_weight
        self.decay_factor = decay_factor

    async def retrieve(
        self,
        query_text: str,
        n_results: int = 5,
        filter_metadata: Optional[Dict[str, Any]] = None,
        use_graph: bool = True,
        **kwargs
    ) -> List[Dict[str, Any]]:
        """
        Retrieve with optional graph enhancement.
        """
        # Phase 1: Vector search for seeds
        seed_size = min(n_results, 15)
        seed_results = await self.base_retriever.retrieve(
            query_text=query_text,
            n_results=seed_size,
            filter_metadata=filter_metadata,
            use_reranker=False,  # Skip reranking at this stage
            use_graph=False,  # Prevent recursion
            **kwargs
        )

        # If graph disabled or no seeds, return base results
        if not use_graph or not seed_results:
            return seed_results[:n_results]

        # Phase 2: Graph expansion
        expanded_results = await self._expand_with_graph(
            seeds=seed_results,
            query_text=query_text,
            max_candidates=50
        )

        # Phase 3: Final reranking
        if self.base_retriever.reranker and len(expanded_results) > n_results:
            try:
                reranked = await asyncio.to_thread(
                    self.base_retriever.reranker.rerank,
                    query_text,
                    expanded_results[:n_results * 2],
                    top_n=n_results
                )
                return [item for _, item in reranked]
            except Exception as e:
                logger.error(f"Reranking failed: {e}")
                return expanded_results[:n_results]

        return expanded_results[:n_results]

    async def _expand_with_graph(
        self,
        seeds: List[Dict],
        query_text: str,
        max_candidates: int = 50
    ) -> List[Dict]:
        """Expand seed results using graph traversal."""
        visited: Set[str] = set()
        candidates: Dict[str, Tuple[Dict, float]] = {}

        # Initialize with seeds
        for seed in seeds:
            chunk_id = seed['id']
            candidates[chunk_id] = (seed, seed['score'])
            visited.add(chunk_id)

        # BFS traversal from each seed
        for seed in seeds:
            queue = [(seed['id'], 0, seed['score'])]

            while queue and len(candidates) < max_candidates:
                current_id, depth, parent_score = queue.pop(0)

                if depth >= self.max_depth:
                    continue

                # Get neighbors
                neighbors = self.graph_store.get_chunk_neighbors(
                    current_id,
                    min_strength=0.3
                )

                for neighbor_id, relationship in neighbors:
                    if neighbor_id in visited:
                        continue

                    visited.add(neighbor_id)

                    # Calculate graph score with decay
                    graph_score = relationship.strength * (
                        self.decay_factor ** (depth + 1)
                    )

                    # Fetch chunk and compute combined score
                    neighbor_chunk = await self._fetch_chunk(neighbor_id)
                    if not neighbor_chunk:
                        continue

                    # Combined scoring
                    vector_sim = neighbor_chunk.get('score', 0.5)
                    combined_score = (
                        self.vector_weight * vector_sim +
                        self.graph_weight * graph_score
                    )

                    candidates[neighbor_id] = (neighbor_chunk, combined_score)
                    queue.append((neighbor_id, depth + 1, combined_score))

        # Sort by combined score
        ranked = sorted(
            candidates.values(),
            key=lambda x: x[1],
            reverse=True
        )

        return [chunk for chunk, score in ranked]

    async def _fetch_chunk(self, chunk_id: str) -> Optional[Dict]:
        """Fetch chunk data from vector store."""
        from database.database import get_db
        from sqlalchemy import text

        db = next(get_db())
        try:
            query = text("""
                SELECT chunk_id, chunk_text, chunk_metadata, dense_embedding
                FROM document_chunks
                WHERE chunk_id = :chunk_id
            """)
            result = db.execute(query, {'chunk_id': chunk_id}).fetchone()

            if result:
                return {
                    'id': result[0],
                    'text': result[1],
                    'metadata': result[2],
                    'score': 0.5  # Default score for graph-discovered chunks
                }
            return None
        except Exception as e:
            logger.error(f"Failed to fetch chunk {chunk_id}: {e}")
            return None
        finally:
            db.close()
