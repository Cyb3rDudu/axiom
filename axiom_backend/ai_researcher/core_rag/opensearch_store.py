"""
OpenSearch Store for Fulltext/Keyword Search

Provides BM25-based fulltext search to complement vector similarity search.
Enables hybrid retrieval: vector + fulltext + graph for improved recall.
"""

import logging
from typing import List, Dict, Any, Optional

logger = logging.getLogger(__name__)

# Lazy import to avoid startup errors if OpenSearch is unavailable
_opensearch_client = None


def get_opensearch_client():
    """Lazy-load OpenSearch client."""
    global _opensearch_client
    if _opensearch_client is not None:
        return _opensearch_client

    try:
        from opensearchpy import OpenSearch
        from ai_researcher import config

        hosts = [{"host": config.OPENSEARCH_HOST, "port": config.OPENSEARCH_PORT}]

        client_kwargs = {
            "hosts": hosts,
            "use_ssl": config.OPENSEARCH_USE_SSL,
            "verify_certs": config.OPENSEARCH_USE_SSL,
        }

        if config.OPENSEARCH_USERNAME and config.OPENSEARCH_PASSWORD:
            client_kwargs["http_auth"] = (config.OPENSEARCH_USERNAME, config.OPENSEARCH_PASSWORD)

        _opensearch_client = OpenSearch(**client_kwargs)
        return _opensearch_client

    except ImportError:
        logger.warning("opensearch-py not installed. Fulltext search disabled.")
        return None
    except Exception as e:
        logger.warning(f"Failed to create OpenSearch client: {e}")
        return None


class OpenSearchStore:
    """
    OpenSearch-based fulltext search store using BM25.
    Complements vector similarity search for hybrid retrieval.
    """

    def __init__(self):
        """Initialize the OpenSearch store."""
        self.client = get_opensearch_client()
        self._index_name = None
        from ai_researcher import config
        self._config_index_name = config.OPENSEARCH_INDEX

    @property
    def index_name(self) -> str:
        """Get the configured index name."""
        return self._config_index_name

    def health_check(self) -> bool:
        """
        Check if OpenSearch cluster is reachable.

        Returns:
            True if cluster is healthy, False otherwise
        """
        if not self.client:
            logger.warning("OpenSearch client not initialized")
            return False

        try:
            health = self.client.cluster.health()
            status = health.get("status", "red")

            if status in ("green", "yellow"):
                logger.info(f"OpenSearch cluster health: {status}")
                return True
            else:
                logger.warning(f"OpenSearch cluster unhealthy: {status}")
                return False

        except Exception as e:
            logger.warning(f"OpenSearch health check failed: {e}")
            return False

    def ensure_index(self) -> bool:
        """
        Ensure the chunks index exists with proper mappings.

        Returns:
            True if index exists or was created successfully
        """
        if not self.client:
            return False

        try:
            if self.client.indices.exists(index=self.index_name):
                logger.debug(f"OpenSearch index '{self.index_name}' already exists")
                return True

            # Create index with mappings
            mappings = {
                "mappings": {
                    "properties": {
                        "chunk_id": {"type": "keyword"},
                        "doc_id": {"type": "keyword"},
                        "chunk_text": {
                            "type": "text",
                            "analyzer": "standard",
                            "search_analyzer": "standard"
                        },
                        "section_titles": {
                            "type": "text",
                            "analyzer": "standard"
                        },
                        "chunk_index": {"type": "integer"},
                        "token_count": {"type": "integer"},
                        "metadata": {"type": "object", "enabled": True}
                    }
                },
                "settings": {
                    "number_of_shards": 1,
                    "number_of_replicas": 0
                }
            }

            self.client.indices.create(index=self.index_name, body=mappings)
            logger.info(f"Created OpenSearch index '{self.index_name}'")
            return True

        except Exception as e:
            logger.error(f"Failed to ensure OpenSearch index: {e}")
            return False

    def add_chunks(self, doc_id: str, chunks: List[Dict]) -> int:
        """
        Index document chunks for fulltext search.

        Args:
            doc_id: Document ID
            chunks: List of chunk dictionaries with 'text' and 'metadata'

        Returns:
            Number of chunks indexed
        """
        if not self.client:
            logger.warning("OpenSearch client not available, skipping indexing")
            return 0

        # Ensure index exists
        if not self.ensure_index():
            logger.error("Failed to ensure OpenSearch index")
            return 0

        try:
            indexed_count = 0

            for chunk in chunks:
                text = chunk.get("text", "")
                metadata = chunk.get("metadata", {})
                chunk_id = metadata.get("chunk_id", "")

                if not chunk_id:
                    continue

                doc = {
                    "chunk_id": chunk_id,
                    "doc_id": doc_id,
                    "chunk_text": text,
                    "section_titles": " ".join(metadata.get("section_titles", [])),
                    "chunk_index": metadata.get("chunk_index", 0),
                    "token_count": metadata.get("token_count", len(text.split())),
                    "metadata": metadata
                }

                # Index document (upsert by chunk_id)
                self.client.index(
                    index=self.index_name,
                    id=chunk_id,
                    body=doc
                )
                indexed_count += 1

            # Refresh index to make changes visible
            self.client.indices.refresh(index=self.index_name)
            logger.info(f"Indexed {indexed_count} chunks in OpenSearch for doc {doc_id}")
            return indexed_count

        except Exception as e:
            logger.error(f"Failed to index chunks in OpenSearch: {e}")
            return 0

    def search(
        self,
        query: str,
        n_results: int = 10,
        filter_doc_ids: Optional[List[str]] = None
    ) -> List[Dict]:
        """
        Perform BM25 fulltext search.

        Args:
            query: Search query string
            n_results: Maximum number of results to return
            filter_doc_ids: Optional list of doc_ids to filter results

        Returns:
            List of search results with chunk_id, doc_id, text, score, metadata
        """
        if not self.client:
            logger.warning("OpenSearch client not available")
            return []

        try:
            # Build query
            must_conditions = [
                {
                    "match": {
                        "chunk_text": {
                            "query": query,
                            "operator": "or"
                        }
                    }
                }
            ]

            # Add doc_id filter if specified
            filter_conditions = []
            if filter_doc_ids:
                filter_conditions.append({
                    "terms": {
                        "doc_id": filter_doc_ids
                    }
                })

            search_body = {
                "size": n_results,
                "query": {
                    "bool": {
                        "must": must_conditions,
                        "filter": filter_conditions
                    }
                },
                "_source": ["chunk_id", "doc_id", "chunk_text", "metadata", "section_titles"]
            }

            response = self.client.search(
                index=self.index_name,
                body=search_body
            )

            # Format results
            results = []
            for hit in response.get("hits", {}).get("hits", []):
                source = hit.get("_source", {})
                results.append({
                    "chunk_id": source.get("chunk_id"),
                    "doc_id": source.get("doc_id"),
                    "text": source.get("chunk_text", ""),
                    "score": hit.get("_score", 0.0),
                    "metadata": source.get("metadata", {}),
                    "section_titles": source.get("section_titles", "")
                })

            logger.debug(f"OpenSearch BM25 search returned {len(results)} results for query: {query[:50]}...")
            return results

        except Exception as e:
            logger.error(f"OpenSearch search failed: {e}")
            return []

    def delete_document(self, doc_id: str) -> int:
        """
        Delete all chunks for a document.

        Args:
            doc_id: Document ID to delete

        Returns:
            Number of documents deleted
        """
        if not self.client:
            logger.warning("OpenSearch client not available")
            return 0

        try:
            query = {
                "query": {
                    "term": {
                        "doc_id": doc_id
                    }
                }
            }

            response = self.client.delete_by_query(
                index=self.index_name,
                body=query
            )

            deleted = response.get("deleted", 0)
            logger.info(f"Deleted {deleted} chunks from OpenSearch for doc {doc_id}")
            return deleted

        except Exception as e:
            logger.error(f"Failed to delete document from OpenSearch: {e}")
            return 0


# Singleton instance
_opensearch_store = None


def get_opensearch_store() -> Optional[OpenSearchStore]:
    """Get or create the OpenSearch store singleton."""
    global _opensearch_store
    if _opensearch_store is None:
        try:
            from ai_researcher import config
            if config.ENABLE_OPENSEARCH:
                _opensearch_store = OpenSearchStore()
                if not _opensearch_store.health_check():
                    logger.warning("OpenSearch health check failed, fulltext search disabled")
                    _opensearch_store = None
        except Exception as e:
            logger.warning(f"Failed to initialize OpenSearch store: {e}")
            _opensearch_store = None
    return _opensearch_store
