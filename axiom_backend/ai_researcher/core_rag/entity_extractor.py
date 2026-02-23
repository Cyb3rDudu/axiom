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
                # Try loading from mounted models directory first
                import os
                spacy_data = os.getenv("SPACY_DATA", "/root/.local/share/spacy")

                # Try large model first (better accuracy), fallback to small
                for model_name in ["en_core_web_lg-3.7.1", "en_core_web_sm-3.7.1"]:
                    model_path = os.path.join(spacy_data, model_name, "en_core_web_lg" if "lg" in model_name else "en_core_web_sm", model_name)

                    if os.path.exists(model_path):
                        self.nlp = spacy.load(model_path)
                        logger.info(f"Loaded spaCy model from: {model_path}")
                        break
                else:
                    # Fallback to default loading
                    self.nlp = spacy.load("en_core_web_lg")
                    logger.info("Loaded spaCy model: en_core_web_lg")
            except OSError as e:
                logger.warning(f"spaCy model not found: {e}. Entity extraction will use 0 entities.")

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
                # Extract ~200 char context window around entity
                context_start = max(0, ent.start_char - 100)
                context_end = min(len(text), ent.end_char + 100)
                context_snippet = text[context_start:context_end]

                entities.append({
                    "text": ent.text,
                    "type": entity_type,
                    "canonical_form": self._normalize_entity(ent.text),
                    "position": ent.start_char,
                    "confidence": 0.8,  # Default for spaCy
                    "context_snippet": context_snippet
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
            "EVENT": "CONCEPT",
            "WORK_OF_ART": "WORK",
            "LAW": "CONCEPT",
            "NORP": "CONCEPT",
            "MONEY": "METRIC",
            "PERCENT": "METRIC",
            "QUANTITY": "METRIC",
            "FAC": "LOCATION"
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
        """Use LLM to detect relationships between entities with 3-level JSON fallback."""
        if not self.llm_client:
            return []

        entity_list = [e["text"] for e in entities]
        base_prompt = self._build_relationship_prompt(chunk_text, entity_list)
        messages = [{"role": "user", "content": base_prompt}]

        # 3-level fallback: json_object -> enhanced prompt -> no format
        attempts = [
            ("json_object", {"type": "json_object"}, messages),
            ("json_object_enhanced", {"type": "json_object"},
             [{"role": "user", "content": base_prompt + "\n\nCRITICAL: Return ONLY valid JSON. Ensure all strings are properly quoted and escaped."}]),
            ("no_format", None,
             [{"role": "user", "content": base_prompt + "\n\nReturn ONLY a valid JSON object, nothing else."}])
        ]

        for attempt_name, response_format, attempt_messages in attempts:
            try:
                kwargs = {"messages": attempt_messages}
                if response_format:
                    kwargs["response_format"] = response_format

                response = await self.llm_client.chat(**kwargs)
                content = response.choices[0].message.content

                # Try to parse JSON
                data = json.loads(content)
                logger.debug(f"Successfully extracted relationships using {attempt_name}")
                return data.get("relationships", [])

            except json.JSONDecodeError as e:
                logger.warning(f"JSON parse error with {attempt_name}: {str(e)[:100]}")
                if attempt_name == "no_format":
                    # Last attempt failed, give up
                    logger.error(f"All JSON extraction attempts failed for entity relationships")
                    return []
                # Try next attempt
                continue

            except Exception as e:
                logger.error(f"LLM relationship extraction failed with {attempt_name}: {str(e)[:100]}")
                if attempt_name == "no_format":
                    return []
                # Try next attempt
                continue

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
        """
        Synchronous wrapper for extract_from_chunk.
        Extract entities and relationships from a chunk.
        Returns (entities, relationships).
        """
        # Fast spaCy extraction (synchronous)
        entities = self._extract_with_spacy(chunk_text) if self.nlp else []
        relationships = []

        return entities, relationships
