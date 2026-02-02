"""
Centralized prompt loading service with aggressive caching.

Provides sub-millisecond prompt access via LRU cache for multilingual
AI agent prompts. Automatically falls back to English if a translation
is not available.

Usage:
    # Initialize once at application startup
    db = next(get_db())
    init_prompt_loader(db)

    # Load prompts in agent code
    loader = get_prompt_loader()
    prompt = loader.load_prompt('ResearchAgent', 'system_prompt', 'de')
"""

import logging
from functools import lru_cache
from typing import Optional, Dict, Any
from sqlalchemy.orm import Session

logger = logging.getLogger(__name__)


class PromptLoader:
    """
    Loads agent prompts from database with automatic fallback to English.
    Uses LRU cache for O(1) lookups after first load.

    Features:
    - Sub-millisecond cached lookups (>99% hit rate expected)
    - Automatic fallback to English if translation unavailable
    - Cache statistics for monitoring performance
    - Thread-safe through LRU cache implementation
    """

    def __init__(self, db: Session):
        self.db = db
        self._cache_stats = {"hits": 0, "misses": 0, "fallbacks": 0}
        logger.info("PromptLoader initialized")

    @lru_cache(maxsize=1000)
    def load_prompt(
        self,
        agent_name: str,
        prompt_key: str,
        language_code: str = 'en'
    ) -> str:
        """
        Load a prompt from database with caching and fallback.

        Args:
            agent_name: Name of agent (e.g., 'PlanningAgent', 'ResearchAgent')
            prompt_key: Prompt identifier (e.g., 'system_prompt', 'phase1')
            language_code: Target language (default: 'en')

        Returns:
            Prompt content as string

        Raises:
            ValueError: If prompt not found in any language
        """
        cache_key = f"{agent_name}:{prompt_key}:{language_code}"

        # Try to load in requested language
        prompt = self._query_prompt(agent_name, prompt_key, language_code)

        if prompt:
            self._cache_stats["hits"] += 1
            logger.debug(f"Loaded prompt {cache_key}")
            return prompt.content

        # Fallback to English
        if language_code != 'en':
            logger.warning(f"Prompt not found for {cache_key}, falling back to English")
            self._cache_stats["fallbacks"] += 1
            prompt = self._query_prompt(agent_name, prompt_key, 'en')

            if prompt:
                return prompt.content

        # No prompt found
        self._cache_stats["misses"] += 1
        raise ValueError(
            f"Prompt not found: agent={agent_name}, key={prompt_key}, "
            f"language={language_code} (and fallback 'en')"
        )

    def _query_prompt(
        self,
        agent_name: str,
        prompt_key: str,
        language_code: str
    ) -> Optional[Any]:
        """
        Query database for prompt.

        Returns the most recent active prompt version for the given
        agent_name, prompt_key, and language_code.
        """
        try:
            # Import here to avoid circular dependency
            from database.models import PromptTemplate

            return self.db.query(PromptTemplate).filter(
                PromptTemplate.agent_name == agent_name,
                PromptTemplate.prompt_key == prompt_key,
                PromptTemplate.language_code == language_code,
                PromptTemplate.is_active == True
            ).order_by(PromptTemplate.version.desc()).first()
        except Exception as e:
            logger.error(f"Error querying prompt: {e}")
            return None

    def get_cache_stats(self) -> Dict[str, Any]:
        """
        Return cache statistics for monitoring.

        Returns:
            Dictionary with hits, misses, fallbacks, hit_rate_percent, and cache_size
        """
        total = self._cache_stats["hits"] + self._cache_stats["misses"]
        hit_rate = (self._cache_stats["hits"] / total * 100) if total > 0 else 0
        return {
            **self._cache_stats,
            "hit_rate_percent": round(hit_rate, 2),
            "cache_size": self.load_prompt.cache_info().currsize
        }

    def clear_cache(self):
        """
        Clear LRU cache.

        Call this when prompts are updated via admin interface
        to ensure agents use the latest versions.
        """
        self.load_prompt.cache_clear()
        logger.info("PromptLoader cache cleared")


# Global instance (initialize with DB session in startup)
_prompt_loader: Optional[PromptLoader] = None


def init_prompt_loader(db: Session):
    """
    Initialize global PromptLoader instance.

    Should be called once during application startup with a database session.

    Args:
        db: SQLAlchemy database session
    """
    global _prompt_loader
    _prompt_loader = PromptLoader(db)
    logger.info("Global PromptLoader instance initialized")


def get_prompt_loader() -> PromptLoader:
    """
    Get global PromptLoader instance.

    Returns:
        The global PromptLoader instance

    Raises:
        RuntimeError: If PromptLoader not initialized (call init_prompt_loader first)
    """
    if _prompt_loader is None:
        raise RuntimeError(
            "PromptLoader not initialized. Call init_prompt_loader(db) first "
            "during application startup."
        )
    return _prompt_loader
