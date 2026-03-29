"""
Entity Extractor for Knowledge Graph

Language-aware entity extraction with cross-language canonical forms:
- Detects document language and loads the correct spaCy model (de/en)
- When LLM extraction is enabled, uses LLM for entities + relationships + English canonicals
- Falls back to spaCy NER when LLM is disabled
- Canonical forms are always English-normalized for cross-language entity merging
"""

import os
import re
import json
import logging
import asyncio
import concurrent.futures
from typing import List, Dict, Tuple, Optional

logger = logging.getLogger(__name__)

try:
    import spacy
    SPACY_AVAILABLE = True
except ImportError:
    SPACY_AVAILABLE = False
    logger.warning("spaCy not available - entity extraction will be limited")


# ── Language detection ──────────────────────────────────────────────────

_DE_STOPWORDS = frozenset({
    "der", "die", "das", "und", "ist", "ein", "eine", "zu", "in", "den",
    "von", "für", "mit", "auf", "des", "dem", "nicht", "sich", "auch",
    "werden", "als", "dass", "oder", "wie", "wird", "bei", "nach", "nur",
    "über", "so", "aber", "vor", "noch", "kann", "durch", "sind", "wenn",
})
_EN_STOPWORDS = frozenset({
    "the", "and", "is", "a", "an", "to", "in", "of", "for", "with",
    "on", "not", "are", "was", "that", "this", "from", "by", "be",
    "have", "has", "it", "or", "which", "but", "as", "can", "they",
    "at", "were", "been", "would", "their", "will", "more", "about",
})

# ── spaCy model config ─────────────────────────────────────────────────

# language -> list of (version_dir, inner_dir) to try (lg first, then sm)
_SPACY_MODELS = {
    "de": [("de_core_news_lg-3.7.0", "de_core_news_lg"),
           ("de_core_news_sm-3.7.0", "de_core_news_sm")],
    "en": [("en_core_web_lg-3.7.1", "en_core_web_lg"),
           ("en_core_web_sm-3.7.1", "en_core_web_sm")],
}

# NER label mapping — covers English and German spaCy models
_LABEL_MAP = {
    # English model
    "PERSON": "PERSON", "ORG": "ORGANIZATION", "GPE": "LOCATION",
    "LOC": "LOCATION", "PRODUCT": "TECHNOLOGY", "WORK_OF_ART": "WORK",
    "EVENT": "CONCEPT", "LAW": "CONCEPT", "NORP": "CONCEPT",
    "MONEY": "METRIC", "PERCENT": "METRIC", "QUANTITY": "METRIC", "FAC": "LOCATION",
    # German model
    "PER": "PERSON", "MISC": "CONCEPT",
}


class EntityExtractor:
    """Language-aware entity extraction with cross-language canonical forms."""

    ENTITY_TYPES = [
        "PERSON", "ORGANIZATION", "LOCATION", "CONCEPT",
        "METHOD", "TECHNOLOGY", "METRIC", "WORK", "DATASET"
    ]

    def __init__(
        self,
        embedder=None,  # kept for API compat
        llm_client=None,
        llm_model: str = None,
        enable_llm_refinement: bool = False,
        language: str = "en",
    ):
        self.llm_client = llm_client
        self.llm_model = llm_model
        self.enable_llm_refinement = enable_llm_refinement
        self.language = language
        self.nlp = None

        # Eagerly load the spaCy model for the given language
        if SPACY_AVAILABLE:
            self._load_spacy_model(language)

    # ── Language detection (static, call before constructing) ───────────

    @staticmethod
    def detect_language(text: str) -> str:
        """Detect language from text using stopword frequency. Returns 'de' or 'en'."""
        words = text.lower().split()[:500]
        word_set = set(words)
        de_hits = len(word_set & _DE_STOPWORDS)
        en_hits = len(word_set & _EN_STOPWORDS)
        lang = "de" if de_hits > en_hits else "en"
        logger.info(f"Language detection: de={de_hits} en={en_hits} → {lang}")
        return lang

    # ── spaCy model loading ─────────────────────────────────────────────

    def _load_spacy_model(self, language: str) -> None:
        """Load the best available spaCy model for the given language."""
        if not SPACY_AVAILABLE:
            return

        spacy_data = os.getenv("SPACY_DATA", "/root/.local/share/spacy")
        models = _SPACY_MODELS.get(language, _SPACY_MODELS["en"])

        for version_dir, inner_dir in models:
            model_path = os.path.join(spacy_data, version_dir, inner_dir, version_dir)
            if os.path.exists(model_path):
                self.nlp = spacy.load(model_path)
                logger.info(f"Loaded spaCy model: {version_dir} for language '{language}'")
                return

        # Fallback: try loading by short name
        for _, inner_dir in models:
            try:
                self.nlp = spacy.load(inner_dir)
                logger.info(f"Loaded spaCy model by name: {inner_dir}")
                return
            except OSError:
                continue

        logger.warning(f"No spaCy model found for language '{language}'")

    # ── spaCy NER extraction ────────────────────────────────────────────

    def _extract_with_spacy(self, text: str) -> List[Dict]:
        """Extract entities using spaCy NER. Canonical form is lowercased original."""
        if not self.nlp:
            return []

        doc = self.nlp(text)
        entities = []

        for ent in doc.ents:
            entity_type = _LABEL_MAP.get(ent.label_)
            if not entity_type:
                continue
            ent_text = ent.text.strip()
            if len(ent_text) < 2:
                continue

            context_start = max(0, ent.start_char - 100)
            context_end = min(len(text), ent.end_char + 100)

            entities.append({
                "text": ent_text,
                "type": entity_type,
                "canonical_form": self._normalize(ent_text),
                "position": ent.start_char,
                "confidence": 0.8,
                "context_snippet": text[context_start:context_end],
            })

        return entities

    @staticmethod
    def _normalize(text: str) -> str:
        """Lowercase and strip punctuation for canonical form."""
        return re.sub(r'[^\w\s]', '', text.lower().strip())

    # ── Batch canonicalization for spaCy entities (cross-language) ──────

    async def canonicalize_entities_batch(self, entities: List[Dict]) -> List[Dict]:
        """
        Post-process spaCy entities: ask the LLM to provide English canonical
        forms for non-English entity texts. One LLM call per document.
        Mutates entities in place and returns them.
        """
        if not self.llm_client or self.language == "en":
            return entities

        # Collect unique non-English entity texts
        unique_texts = list({e["text"] for e in entities if e.get("text")})
        if not unique_texts or len(unique_texts) > 200:
            return entities  # skip if too many

        prompt = f"""Translate these named entities to their standard English form.
Return a JSON object mapping original text to English canonical form.
If an entity is already in English or is a proper name that doesn't change, keep it as-is.

Entities:
{json.dumps(unique_texts, ensure_ascii=False)}

Return JSON:
{{"Europäische Zentralbank": "european central bank", "Adam Smith": "adam smith", "Marktwirtschaft": "market economy"}}"""

        try:
            response = self.llm_client.chat.completions.create(
                messages=[{"role": "user", "content": prompt}],
                model=self.llm_model,
                response_format={"type": "json_object"},
            )
            mapping = json.loads(response.choices[0].message.content)

            # Apply mapping
            for entity in entities:
                canonical = mapping.get(entity["text"])
                if canonical:
                    entity["canonical_form"] = canonical.lower().strip()

            logger.info(f"Canonicalized {len(mapping)} entity texts to English")
        except Exception as e:
            logger.warning(f"Batch canonicalization failed (non-fatal): {e}")

        return entities

    # ── LLM entity + relationship extraction ────────────────────────────

    async def _extract_entities_llm(self, chunk_text: str) -> Tuple[List[Dict], List[Dict]]:
        """Extract entities and relationships via LLM. Returns English canonical forms."""
        if not self.llm_client:
            return [], []

        prompt = f"""Extract named entities and relationships from this academic text.

Entity types (use ONLY these):
- PERSON: real people (authors, historical figures, philosophers)
- ORGANIZATION: institutions, companies, journals, universities
- LOCATION: countries, cities, regions
- CONCEPT: key theories, methods, frameworks, important technical terms
- WORK: books, papers, laws, publications referenced

For each entity provide:
- "text": the entity as it appears in the text
- "type": one of the types above
- "canonical": the standard English form, lowercased (e.g. "Europäische Zentralbank" → "european central bank")

Relationship types: CITES, USES, EXTENDS, CONTRADICTS, SUPPORTS, DEVELOPS, AFFILIATED_WITH

Rules:
- Extract only meaningful, specific entities — not common words or sentence fragments
- Proper names (people, places) stay as-is in canonical form, just lowercased
- Concepts and organizations should be translated to English in canonical form
- Limit to the 15 most important entities per chunk

Return JSON:
{{
  "entities": [
    {{"text": "Adam Smith", "type": "PERSON", "canonical": "adam smith"}},
    {{"text": "Der Wohlstand der Nationen", "type": "WORK", "canonical": "the wealth of nations"}}
  ],
  "relationships": [
    {{"source": "Adam Smith", "target": "The Wealth of Nations", "type": "CITES", "confidence": 0.9}}
  ]
}}

Text:
{chunk_text[:2000]}"""

        messages = [{"role": "user", "content": prompt}]

        # 3-level JSON fallback
        attempts = [
            ("json_object", {"type": "json_object"}, messages),
            ("json_enhanced", {"type": "json_object"},
             [{"role": "user", "content": prompt + "\n\nCRITICAL: Return ONLY valid JSON."}]),
            ("no_format", None,
             [{"role": "user", "content": prompt + "\n\nReturn ONLY a valid JSON object, nothing else."}]),
        ]

        for attempt_name, response_format, attempt_messages in attempts:
            try:
                kwargs = {"messages": attempt_messages, "model": self.llm_model}
                if response_format:
                    kwargs["response_format"] = response_format

                response = self.llm_client.chat.completions.create(**kwargs)
                content = response.choices[0].message.content
                data = json.loads(content)

                # Parse entities
                raw_entities = data.get("entities", [])
                entities = []
                for e in raw_entities:
                    ent_text = (e.get("text") or "").strip()
                    etype = (e.get("type") or "").upper()
                    canonical = (e.get("canonical") or ent_text).lower().strip()
                    if ent_text and etype in self.ENTITY_TYPES and len(ent_text) >= 2:
                        entities.append({
                            "text": ent_text,
                            "type": etype,
                            "canonical_form": re.sub(r'[^\w\s]', '', canonical),
                            "position": 0,
                            "confidence": 0.9,
                            "context_snippet": "",
                        })

                relationships = data.get("relationships", [])
                logger.debug(f"LLM extracted {len(entities)} entities, {len(relationships)} relationships ({attempt_name})")
                return entities, relationships

            except json.JSONDecodeError as e:
                logger.warning(f"JSON parse error ({attempt_name}): {str(e)[:100]}")
                if attempt_name == "no_format":
                    logger.error("All JSON attempts failed for LLM entity extraction")
                    return [], []
            except Exception as e:
                logger.error(f"LLM entity extraction failed ({attempt_name}): {str(e)[:100]}")
                if attempt_name == "no_format":
                    return [], []

        return [], []

    # ── Main extraction entry points ────────────────────────────────────

    async def extract_from_chunk(
        self,
        chunk_text: str,
        chunk_metadata: Dict,
    ) -> Tuple[List[Dict], List[Dict]]:
        """
        Extract entities and relationships from a chunk.
        LLM path: entities + relationships + English canonical forms.
        spaCy path: entities only, canonical = lowercased original.
        """
        if self.enable_llm_refinement and self.llm_client:
            return await self._extract_entities_llm(chunk_text)

        entities = self._extract_with_spacy(chunk_text)
        return entities, []

    def extract_from_chunk_sync(
        self,
        chunk_text: str,
        chunk_metadata: Dict,
    ) -> Tuple[List[Dict], List[Dict]]:
        """
        Synchronous wrapper. Runs LLM extraction in a thread pool when enabled.
        """
        if self.enable_llm_refinement and self.llm_client:
            try:
                with concurrent.futures.ThreadPoolExecutor() as pool:
                    return pool.submit(
                        lambda: asyncio.run(self._extract_entities_llm(chunk_text))
                    ).result(timeout=30)
            except Exception as e:
                logger.error(f"LLM entity extraction failed (sync): {e}")
                # Fall through to spaCy

        entities = self._extract_with_spacy(chunk_text)
        return entities, []
