"""
Entity Extractor for Knowledge Graph

Uses GLiNER (zero-shot NER) for high-quality, multilingual entity extraction
with custom academic entity types. Falls back to spaCy if GLiNER unavailable.
"""

import os
import re
import logging
from typing import List, Dict, Tuple, Optional

logger = logging.getLogger(__name__)

# ── GLiNER setup ────────────────────────────────────────────────────────

GLINER_AVAILABLE = False
_gliner_model = None

try:
    from gliner import GLiNER
    GLINER_AVAILABLE = True
except ImportError:
    logger.warning("GLiNER not available - falling back to spaCy NER")

SPACY_AVAILABLE = False
try:
    import spacy
    SPACY_AVAILABLE = True
except ImportError:
    pass

# Entity labels for GLiNER (plain text, defined at inference time)
GLINER_LABELS = [
    "person",
    "organization",
    "location",
    "concept",
    "book or journal",
    "research method",
]

# Map GLiNER labels to our internal types
_GLINER_TYPE_MAP = {
    "person": "PERSON",
    "organization": "ORGANIZATION",
    "location": "LOCATION",
    "concept": "CONCEPT",
    "book or journal": "WORK",
    "research method": "METHOD",
}

# Patterns to filter from entity text
_NOISE_RE = re.compile(r'\bet\s+al\.?$', re.IGNORECASE)

# spaCy fallback label mapping (covers English and German models)
_SPACY_LABEL_MAP = {
    "PERSON": "PERSON", "PER": "PERSON",
    "ORG": "ORGANIZATION",
    "GPE": "LOCATION", "LOC": "LOCATION",
    "WORK_OF_ART": "WORK",
    "MISC": "CONCEPT",
}

# spaCy model config
_SPACY_MODELS = {
    "de": [("de_core_news_lg-3.7.0", "de_core_news_lg"),
           ("de_core_news_sm-3.7.0", "de_core_news_sm")],
    "en": [("en_core_web_lg-3.7.1", "en_core_web_lg"),
           ("en_core_web_sm-3.7.1", "en_core_web_sm")],
}

# Language detection stopwords
_DE_STOPWORDS = frozenset({
    "der", "die", "das", "und", "ist", "ein", "eine", "zu", "in", "den",
    "von", "für", "mit", "auf", "des", "dem", "nicht", "sich", "auch",
    "werden", "als", "dass", "oder", "wie", "wird", "bei", "nach",
})
_EN_STOPWORDS = frozenset({
    "the", "and", "is", "a", "an", "to", "in", "of", "for", "with",
    "on", "not", "are", "was", "that", "this", "from", "by", "be",
    "have", "has", "it", "or", "which", "but", "as", "can", "they",
})


def _get_gliner_model():
    """Lazy-load singleton GLiNER model on GPU if available."""
    global _gliner_model
    if _gliner_model is None and GLINER_AVAILABLE:
        import torch
        cache_dir = os.getenv("HF_HOME", "/root/.cache/huggingface/hub")
        device = "cuda" if torch.cuda.is_available() else "cpu"
        logger.info(f"Loading GLiNER model (urchade/gliner_multi-v2.1) on {device}...")
        _gliner_model = GLiNER.from_pretrained(
            "urchade/gliner_multi-v2.1",
            cache_dir=cache_dir,
        ).to(device)
        logger.info(f"GLiNER model loaded on {device}.")
    return _gliner_model


class EntityExtractor:
    """
    Entity extraction using GLiNER (zero-shot, multilingual, custom types).
    Falls back to language-aware spaCy NER if GLiNER is unavailable.
    """

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
        gliner_threshold: float = 0.45,
    ):
        self.llm_client = llm_client
        self.llm_model = llm_model
        self.enable_llm_refinement = enable_llm_refinement
        self.language = language
        self.gliner_threshold = gliner_threshold
        self.nlp = None  # spaCy fallback

        # Try GLiNER first, fall back to spaCy
        if GLINER_AVAILABLE:
            self._gliner = _get_gliner_model()
        else:
            self._gliner = None
            if SPACY_AVAILABLE:
                self._load_spacy_model(language)

    # ── Language detection ──────────────────────────────────────────────

    @staticmethod
    def detect_language(text: str) -> str:
        """Detect language from text using stopword frequency."""
        words = text.lower().split()[:500]
        word_set = set(words)
        de_hits = len(word_set & _DE_STOPWORDS)
        en_hits = len(word_set & _EN_STOPWORDS)
        lang = "de" if de_hits > en_hits else "en"
        logger.info(f"Language detection: de={de_hits} en={en_hits} → {lang}")
        return lang

    # ── GLiNER extraction ───────────────────────────────────────────────

    def _extract_with_gliner(self, text: str) -> List[Dict]:
        """Extract entities using GLiNER zero-shot NER."""
        if not self._gliner:
            return []

        try:
            raw_entities = self._gliner.predict_entities(
                text, GLINER_LABELS,
                threshold=self.gliner_threshold,
                multi_label=True,
            )
        except Exception as e:
            logger.error(f"GLiNER prediction failed: {e}")
            return []

        entities = []
        seen = set()  # deduplicate by (text, type)

        for e in raw_entities:
            ent_text = e["text"].strip()
            ent_type = _GLINER_TYPE_MAP.get(e["label"])
            if not ent_type:
                continue
            if len(ent_text) < 2 or len(ent_text) > 100:
                continue
            # Filter "et al." citation patterns
            if _NOISE_RE.search(ent_text):
                continue
            # Skip generic single common words
            if len(ent_text.split()) == 1 and ent_text.lower() in {
                "firm", "firms", "workers", "government", "governments",
                "countries", "borrowers", "savers", "lenders", "households",
            }:
                continue

            key = (ent_text.lower(), ent_type)
            if key in seen:
                continue
            seen.add(key)

            entities.append({
                "text": ent_text,
                "type": ent_type,
                "canonical_form": self._normalize(ent_text),
                "position": e.get("start", 0),
                "confidence": round(e["score"], 3),
                "context_snippet": "",
            })

        return entities

    # ── spaCy fallback ──────────────────────────────────────────────────

    def _load_spacy_model(self, language: str) -> None:
        """Load spaCy model for the given language."""
        if not SPACY_AVAILABLE:
            return
        spacy_data = os.getenv("SPACY_DATA", "/root/.local/share/spacy")
        models = _SPACY_MODELS.get(language, _SPACY_MODELS["en"])
        for version_dir, inner_dir in models:
            model_path = os.path.join(spacy_data, version_dir, inner_dir, version_dir)
            if os.path.exists(model_path):
                self.nlp = spacy.load(model_path)
                logger.info(f"Loaded spaCy model: {version_dir}")
                return
        logger.warning(f"No spaCy model found for language '{language}'")

    def _extract_with_spacy(self, text: str) -> List[Dict]:
        """Fallback: extract entities using spaCy NER."""
        if not self.nlp:
            return []
        doc = self.nlp(text)
        entities = []
        for ent in doc.ents:
            ent_type = _SPACY_LABEL_MAP.get(ent.label_)
            if not ent_type:
                continue
            ent_text = ent.text.strip()
            if len(ent_text) < 3 or len(ent_text) > 80:
                continue
            entities.append({
                "text": ent_text,
                "type": ent_type,
                "canonical_form": self._normalize(ent_text),
                "position": ent.start_char,
                "confidence": 0.8,
                "context_snippet": "",
            })
        return entities

    # ── Shared utils ────────────────────────────────────────────────────

    @staticmethod
    def _normalize(text: str) -> str:
        """Lowercase and strip punctuation for canonical form."""
        return re.sub(r'[^\w\s]', '', text.lower().strip())

    # ── Main entry points ───────────────────────────────────────────────

    def extract_from_chunk(
        self,
        chunk_text: str,
        chunk_metadata: Dict,
    ) -> Tuple[List[Dict], List[Dict]]:
        """Extract entities from a chunk. Returns (entities, relationships)."""
        if self._gliner:
            entities = self._extract_with_gliner(chunk_text)
        else:
            entities = self._extract_with_spacy(chunk_text)
        return entities, []

    def extract_from_chunk_sync(
        self,
        chunk_text: str,
        chunk_metadata: Dict,
    ) -> Tuple[List[Dict], List[Dict]]:
        """Synchronous entry point (same as extract_from_chunk)."""
        return self.extract_from_chunk(chunk_text, chunk_metadata)
