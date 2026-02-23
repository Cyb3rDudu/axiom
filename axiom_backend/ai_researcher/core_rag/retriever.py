import asyncio # <-- Import asyncio
from typing import List, Dict, Any, Optional
import logging

from .embedder import TextEmbedder
from .pgvector_store import PGVectorStore as VectorStore
from .reranker import TextReranker # Optional reranker

logger = logging.getLogger(__name__)

class Retriever:
    """
    Handles the retrieval process: embedding queries, querying the vector store,
    and optionally reranking results.
    Supports hybrid retrieval: vector + OpenSearch fulltext + graph.
    """
    def __init__(
        self,
        embedder: TextEmbedder,
        vector_store: VectorStore,
        reranker: Optional[TextReranker] = None # Allow optional reranker
    ):
        self.embedder = embedder
        self.vector_store = vector_store
        self.reranker = reranker
        self.graph_retriever = None
        self.opensearch_available = False
        self.os_store = None

        # Initialize OpenSearch for fulltext search
        from ai_researcher import config
        if config.ENABLE_OPENSEARCH:
            try:
                from .opensearch_store import get_opensearch_store
                self.os_store = get_opensearch_store()
                if self.os_store and self.os_store.health_check():
                    self.opensearch_available = True
                    logger.info("Retriever: OpenSearch fulltext search enabled.")
                else:
                    logger.warning("Retriever: OpenSearch unavailable, falling back to vector-only search.")
            except Exception as e:
                logger.warning(f"Retriever: OpenSearch init failed: {e}")
        else:
            logger.info("Retriever: OpenSearch disabled by config.")

        # Initialize graph-enhanced retrieval if enabled
        if config.ENABLE_GRAPH_RETRIEVAL:
            try:
                from .graph_store import GraphStore
                from .graph_enhanced_retriever import GraphEnhancedRetriever
                graph_store = GraphStore()
                self.graph_retriever = GraphEnhancedRetriever(
                    base_retriever=self,
                    graph_store=graph_store,
                    **config.GRAPH_RETRIEVAL_CONFIG
                )
                print("Retriever initialized with graph enhancement.")
            except Exception as e:
                print(f"Warning: Failed to initialize graph retriever: {e}")
                print("Falling back to standard vector retrieval.")
        else:
            print("Retriever initialized.")

        if self.reranker:
             print("Retriever: Reranker is enabled.")
        else:
              print("Retriever: Reranker is disabled.")


    async def retrieve( # <-- Make async
        self,
        query_text: str,
        n_results: int = 5,
        filter_metadata: Optional[Dict[str, Any]] = None,
        use_reranker: bool = True, # Flag to control reranking per query
        dense_weight: float = 0.5, # Weight for initial vector store query
        sparse_weight: float = 0.5,  # Weight for initial vector store query
        use_graph: bool = True  # NEW: Enable graph-enhanced retrieval
    ) -> List[Dict[str, Any]]:
        """
        Retrieves relevant chunks for a given query.

        Args:
            query_text: The user's query string.
            n_results: The final number of results desired.
            filter_metadata: Optional metadata filter for the vector store query.
            use_reranker: Whether to use the reranker if available and enabled.
            dense_weight: Weight for dense embeddings in the initial hybrid search.
            sparse_weight: Weight for sparse embeddings in the initial hybrid search.
            use_graph: Whether to use graph-enhanced retrieval if available.

        Returns:
            A list of retrieved chunk dictionaries, sorted by relevance.
        """
        import logging as _logging
        _ret_logger = _logging.getLogger(__name__)
        # Use graph retriever if enabled and requested
        if use_graph and self.graph_retriever:
            _ret_logger.info(f"Retriever: Delegating to graph retriever for query '{query_text[:50]}'")
            return await self.graph_retriever.retrieve(
                query_text=query_text,
                n_results=n_results,
                filter_metadata=filter_metadata,
                use_graph=True,
                dense_weight=dense_weight,
                sparse_weight=sparse_weight
            )

        # Otherwise use standard retrieval
        _ret_logger.info(f"Retriever: Standard path for query '{query_text[:50]}', use_graph={use_graph}")
        print(f"\n--- Retrieving documents for query: '{query_text}' ---")

        # 1. Embed the query (using async method with semaphore)
        _ret_logger.info("Retriever: Embedding query...")
        try:
            # Use the new async embedding method that includes semaphore control
            query_embeddings = await self.embedder.embed_query_async(query_text)
            if not query_embeddings:
                _ret_logger.error(f"Retriever: Failed to embed query (returned None) for '{query_text[:50]}'")
                return []
        except Exception as e:
            _ret_logger.error(f"Retriever: Error during query embedding: {e}", exc_info=True)
            return []

        query_dense = query_embeddings.get("dense")
        query_sparse = query_embeddings.get("sparse") # This is the dict

        if not query_dense or query_sparse is None:
             _ret_logger.error(f"Retriever: Query embedding returned unexpected format. dense={query_dense is not None}, sparse={query_sparse is not None}")
             return []

        # 2. Query the Vector Store (and OpenSearch in parallel if available)
        # Fetch potentially more results initially if reranking is enabled
        initial_fetch_n = n_results * 3 if (use_reranker and self.reranker) else n_results
        print(f"Querying vector store (in thread, fetching up to {initial_fetch_n} results)...")

        # Prepare filter for OpenSearch
        filter_doc_ids = None
        if filter_metadata and "doc_id" in filter_metadata:
            if isinstance(filter_metadata["doc_id"], dict) and "$in" in filter_metadata["doc_id"]:
                filter_doc_ids = filter_metadata["doc_id"]["$in"]
            else:
                filter_doc_ids = [filter_metadata["doc_id"]]

        # Parallel search: vector + OpenSearch
        async def vector_search_task():
            try:
                return await asyncio.to_thread(
                    self.vector_store.query,
                    query_dense_embedding=query_dense,
                    query_sparse_embedding_dict=query_sparse,
                    n_results=initial_fetch_n,
                    filter_metadata=filter_metadata,
                    dense_weight=dense_weight,
                    sparse_weight=sparse_weight
                )
            except Exception as e:
                print(f"Error during vector store query thread execution: {e}")
                return []

        async def opensearch_search_task():
            if not self.opensearch_available or not self.os_store:
                return []
            try:
                return await asyncio.to_thread(
                    self.os_store.search,
                    query_text,
                    n_results=initial_fetch_n,
                    filter_doc_ids=filter_doc_ids
                )
            except Exception as e:
                print(f"Error during OpenSearch query: {e}")
                return []

        # Execute searches in parallel
        if self.opensearch_available:
            vector_results, opensearch_results = await asyncio.gather(
                vector_search_task(),
                opensearch_search_task()
            )
            print(f"Retrieved {len(vector_results)} vector results, {len(opensearch_results)} OpenSearch results.")

            # Merge results with configurable weights
            initial_results = self._merge_results(
                vector_results, opensearch_results,
                vector_weight=0.7, fulltext_weight=0.3
            )
        else:
            initial_results = await vector_search_task()
            print(f"Retrieved {len(initial_results)} results from vector store.")

        if not initial_results:
            print("No results found in vector store. Attempting to refresh client and retry...")

            # Try retry once without refresh_client (method doesn't exist)
            try:
                initial_results = await asyncio.to_thread(
                    self.vector_store.query,
                    query_dense_embedding=query_dense,
                    query_sparse_embedding_dict=query_sparse,
                    n_results=initial_fetch_n,
                    filter_metadata=filter_metadata,
                    dense_weight=dense_weight,
                    sparse_weight=sparse_weight
                )

                if initial_results:
                    print(f"After refresh: Retrieved {len(initial_results)} results from vector store.")
                else:
                    print("No results found in vector store even after refresh.")
                    return []
            except Exception as e:
                print(f"Error during vector store retry after refresh: {e}")
                return []

        print(f"Retrieved {len(initial_results)} initial results from vector store.")

        # 3. Optionally Rerank (run sync reranker in thread)
        if use_reranker and self.reranker:
            print("Applying reranker (in thread)...")
            try:
                # Run the synchronous rerank method in a separate thread
                # The reranker now returns a list of tuples (score, item)
                reranked_tuples = await asyncio.to_thread(
                    self.reranker.rerank, query_text, initial_results, top_n=n_results
                )
                # Extract just the items from the tuples
                final_results = [item for _, item in reranked_tuples]
                print(f"Returning {len(final_results)} reranked results.")
            except Exception as e:
                 print(f"Error during reranker thread execution: {e}. Falling back to initial results.")
                 # Fallback to initial results if reranking fails
                 final_results = initial_results[:n_results]
        else:
            # If not reranking, just take the top N from the initial results
            final_results = initial_results[:n_results]
            print(f"Returning {len(final_results)} results (reranker disabled or skipped).")


        # Return the list of dictionaries as retrieved/reranked

        return final_results

    def _merge_results(
        self,
        vector_results: List[Dict[str, Any]],
        opensearch_results: List[Dict[str, Any]],
        vector_weight: float = 0.7,
        fulltext_weight: float = 0.3
    ) -> List[Dict[str, Any]]:
        """
        Merge vector and OpenSearch results using reciprocal rank fusion.

        Args:
            vector_results: Results from vector similarity search
            opensearch_results: Results from BM25 fulltext search
            vector_weight: Weight for vector search scores
            fulltext_weight: Weight for fulltext search scores

        Returns:
            Merged and deduplicated results sorted by combined score
        """
        if not opensearch_results:
            return vector_results
        if not vector_results:
            return opensearch_results

        # Normalize weights
        total_weight = vector_weight + fulltext_weight
        if total_weight > 0:
            vector_weight /= total_weight
            fulltext_weight /= total_weight

        # Combine results by chunk_id
        merged = {}

        # Add vector results
        for i, result in enumerate(vector_results):
            chunk_id = result.get('id') or result.get('chunk_id')
            if chunk_id:
                merged[chunk_id] = {
                    **result,
                    'id': chunk_id,
                    'vector_score': result.get('score', 0),
                    'fulltext_score': 0,
                    'vector_rank': i + 1
                }

        # Add/update with OpenSearch results
        for i, result in enumerate(opensearch_results):
            chunk_id = result.get('chunk_id') or result.get('id')
            if chunk_id:
                if chunk_id in merged:
                    merged[chunk_id]['fulltext_score'] = result.get('score', 0)
                    merged[chunk_id]['fulltext_rank'] = i + 1
                else:
                    merged[chunk_id] = {
                        **result,
                        'id': chunk_id,
                        'vector_score': 0,
                        'fulltext_score': result.get('score', 0),
                        'fulltext_rank': i + 1
                    }

        # Calculate combined score using weighted combination
        for chunk_id, result in merged.items():
            v_score = result.get('vector_score', 0)
            f_score = result.get('fulltext_score', 0)

            # Normalize scores (simple normalization)
            # Use reciprocal rank fusion as fallback if scores are missing
            v_rank = result.get('vector_rank', len(vector_results) + 1)
            f_rank = result.get('fulltext_rank', len(opensearch_results) + 1)

            # RRF score: sum of 1/(rank + k)
            k = 60  # RRF constant
            rrf_vector = 1 / (v_rank + k) if v_rank else 0
            rrf_fulltext = 1 / (f_rank + k) if f_rank else 0

            # Combine using weights
            result['score'] = (vector_weight * rrf_vector + fulltext_weight * rrf_fulltext)
            result['combined_score'] = result['score']

        # Sort by combined score
        sorted_results = sorted(merged.values(), key=lambda x: x.get('score', 0), reverse=True)
        return sorted_results
