"""
Entity Extractor for Knowledge Graph

Hybrid entity extraction using spaCy NER + optional LLM refinement.
"""

from typing import List, Dict, Tuple, Optional
import re
import json
import logging

logger = logging.getLogger(__name__)

try:
    import spacy
    SPACY_AVAILABLE = True
except ImportError:
    SPACY_AVAILABLE = False
    logger.warning("spaCy not available - entity extraction will be limited")


class EntityExtractor:
    """
    Hybrid entity extraction using spaCy NER + optional LLM refinement.
    """

    ENTITY_TYPES = [
        "PERSON", "ORGANIZATION", "LOCATION", "CONCEPT",
        "METHOD", "TECHNOLOGY", "METRIC", "WORK", "DATASET"
    ]

    def __init__(
        self,
        embedder=None,
        llm_client=None,
        enable_llm_refinement: bool = False
    ):
        self.embedder = embedder
        self.llm_client = llm_client
        self.enable_llm_refinement = enable_llm_refinement
        self.nlp = None

        # Load spaCy model if available
        if SPACY_AVAILABLE:
            try:
                self.nlp = spacy.load("en_core_web_sm")
                logger.info("Loaded spaCy model: en_core_web_sm")
            except OSError:
                logger.warning("spaCy model not found. Run: python -m spacy download en_core_web_sm")

    async def extract_from_chunk(
        self,
        chunk_text: str,
        chunk_metadata: Dict
    ) -> Tuple[List[Dict], List[Dict]]:
        """
        Extract entities and relationships from a chunk.
        Returns (entities, relationships).
        """
        # Fast spaCy extraction
        entities = self._extract_with_spacy(chunk_text) if self.nlp else []

        # Optional LLM refinement for relationships
        relationships = []
        if self.enable_llm_refinement and self.llm_client and len(entities) > 1:
            try:
                relationships = await self._extract_relationships_llm(
                    chunk_text, entities
                )
            except Exception as e:
                logger.error(f"LLM relationship extraction failed: {e}")

        return entities, relationships

    def _extract_with_spacy(self, text: str) -> List[Dict]:
        """Fast entity extraction with spaCy."""
        if not self.nlp:
            return []

        doc = self.nlp(text)
        entities = []

        for ent in doc.ents:
            # Map spaCy labels to our types
            entity_type = self._map_spacy_label(ent.label_)
            if entity_type:
                entities.append({
                    "text": ent.text,
                    "type": entity_type,
                    "canonical_form": self._normalize_entity(ent.text),
                    "position": ent.start_char,
                    "confidence": 0.8  # Default for spaCy
                })

        return entities

    def _map_spacy_label(self, label: str) -> Optional[str]:
        """Map spaCy NER labels to our entity types."""
        mapping = {
            "PERSON": "PERSON",
            "ORG": "ORGANIZATION",
            "GPE": "LOCATION",
            "LOC": "LOCATION",
            "PRODUCT": "TECHNOLOGY",
            "EVENT": "CONCEPT"
        }
        return mapping.get(label)

    def _normalize_entity(self, text: str) -> str:
        """Normalize entity text to canonical form."""
        canonical = text.lower().strip()
        canonical = re.sub(r'[^\w\s]', '', canonical)
        return canonical

    async def _extract_relationships_llm(
        self,
        chunk_text: str,
        entities: List[Dict]
    ) -> List[Dict]:
        """Use LLM to detect relationships between entities."""
        if not self.llm_client:
            return []

        entity_list = [e["text"] for e in entities]
        prompt = self._build_relationship_prompt(chunk_text, entity_list)

        try:
            response = await self.llm_client.chat(
                messages=[{"role": "user", "content": prompt}],
                response_format={"type": "json_object"}
            )

            data = json.loads(response.choices[0].message.content)
            return data.get("relationships", [])
        except Exception as e:
            logger.error(f"LLM relationship extraction failed: {e}")
            return []

    def _build_relationship_prompt(
        self,
        text: str,
        entities: List[str]
    ) -> str:
        """Build prompt for LLM relationship extraction."""
        return f"""Extract relationships between the following entities in the text.

Entities: {', '.join(entities)}

Relationship Types: CITES, USES, EXTENDS, CONTRADICTS, SUPPORTS, DEVELOPS, AFFILIATED_WITH

Text:
{text[:2000]}

Return JSON:
{{
  "relationships": [
    {{"source": "entity1", "target": "entity2", "type": "USES", "confidence": 0.9}}
  ]
}}
"""

    def extract_from_chunk_sync(
        self,
        chunk_text: str,
        chunk_metadata: Dict
    ) -> Tuple[List[Dict], List[Dict]]:
        

    def extract_from_chunk_sync(
        self,
        chunk_text: str,
        chunk_metadata: Dict
    ) -> Tuple[List[Dict], List[Dict]]:
        """
        Synchronous wrapper for extract_from_chunk.
        Extract entities and relationships from a chunk.
        Returns (entities, relationships).
        """
        # Fast spaCy extraction (synchronous)
        entities = self._extract_with_spacy(chunk_text) if self.nlp else []

        # Note: LLM refinement requires async, so we skip it in sync mode
        relationships = []

        return entities, relationships
