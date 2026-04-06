"""
Relation Extractor for Knowledge Graph

Uses mREBEL-large (Babelscape) for multilingual relation extraction.
Extracts (subject, predicate, object) triples from text with entity types.
Supports 18 languages including German and English.

VRAM management: loads on demand (~2.4GB), should be unloaded after use
to free GPU memory for embedder/reranker.
"""

import re
import logging
import torch
from typing import List, Dict, Tuple, Optional

logger = logging.getLogger(__name__)

_mrebel_model = None
_mrebel_tokenizer = None


def load_mrebel():
    """Load mREBEL model onto GPU. Call unload_mrebel() when done."""
    global _mrebel_model, _mrebel_tokenizer
    if _mrebel_model is not None:
        return _mrebel_model, _mrebel_tokenizer

    from transformers import AutoModelForSeq2SeqLM, AutoTokenizer

    logger.info("Loading mREBEL-large for relation extraction...")
    cache_dir = None  # uses default HF cache
    _mrebel_tokenizer = AutoTokenizer.from_pretrained(
        "Babelscape/mrebel-large", cache_dir=cache_dir
    )
    _mrebel_model = AutoModelForSeq2SeqLM.from_pretrained(
        "Babelscape/mrebel-large", cache_dir=cache_dir
    )
    from hardware_detection import hardware_detector
    device = hardware_detector.get_model_device("mrebel")
    _mrebel_model = _mrebel_model.to(device).eval()
    vram_info = f" ({torch.cuda.memory_allocated()/1e9:.1f}GB VRAM)" if device.startswith("cuda") else ""
    logger.info(f"mREBEL loaded on {device}{vram_info}")
    return _mrebel_model, _mrebel_tokenizer


def unload_mrebel():
    """Unload mREBEL from GPU to free VRAM."""
    global _mrebel_model, _mrebel_tokenizer
    if _mrebel_model is not None:
        del _mrebel_model
        _mrebel_model = None
    if _mrebel_tokenizer is not None:
        del _mrebel_tokenizer
        _mrebel_tokenizer = None
    if torch.cuda.is_available():
        torch.cuda.empty_cache()
    import gc
    gc.collect()
    logger.info("mREBEL unloaded, GPU memory freed")


# Map mREBEL entity types to our internal types
_MREBEL_TYPE_MAP = {
    "per": "PERSON",
    "org": "ORGANIZATION",
    "loc": "LOCATION",
    "concept": "CONCEPT",
    "media": "WORK",
    "event": "CONCEPT",
    "misc": "CONCEPT",
}


def _parse_mrebel_output(decoded: str) -> List[Dict]:
    """
    Parse mREBEL output into structured triples.

    mREBEL output format:
        tp_XX<triplet> HEAD <head_type> TAIL <tail_type> RELATION <triplet> ...

    Returns list of dicts with keys:
        head, head_type, tail, tail_type, relation
    """
    triples = []
    # Clean up output
    decoded = decoded.replace("</s>", "").replace("<pad>", "")

    # Extract all triples using findall -- handles cascading triples in one beam
    # Pattern: <triplet> HEAD <type> TAIL <type> RELATION
    # RELATION ends at the next <triplet>, <type>, or end of string
    for match in re.finditer(
        r'<triplet>\s*(.+?)\s*<(\w+)>\s*(.+?)\s*<(\w+)>\s*([^<]+)',
        decoded
    ):
        head, head_type, tail, tail_type, relation = match.groups()
        head = head.strip()
        tail = tail.strip()
        relation = relation.strip()

        if head and tail and relation and len(head) >= 2 and len(tail) >= 2:
            triples.append({
                "head": head,
                "head_type": _MREBEL_TYPE_MAP.get(head_type.lower(), "CONCEPT"),
                "tail": tail,
                "tail_type": _MREBEL_TYPE_MAP.get(tail_type.lower(), "CONCEPT"),
                "relation": relation,
            })

    return triples


def extract_relations_from_chunks(
    chunks: List[Dict],
    num_beams: int = 3,
    max_length: int = 256,
) -> List[Dict]:
    """
    Extract relation triples from a list of chunks using mREBEL.

    Args:
        chunks: List of chunk dicts with 'text' key
        num_beams: Beam search width (more beams = more triples but slower)
        max_length: Max output sequence length

    Returns:
        List of triple dicts with: head, head_type, tail, tail_type, relation, chunk_id
    """
    model, tokenizer = load_mrebel()
    device = next(model.parameters()).device
    tp_token_id = tokenizer.convert_tokens_to_ids("tp_XX")

    all_triples = []
    seen = set()

    for chunk in chunks:
        text = chunk.get("text", "")
        chunk_id = chunk.get("metadata", {}).get("chunk_id", "")

        if not text or len(text.strip()) < 50:
            continue

        # Truncate long chunks for mREBEL (max ~512 tokens input)
        input_text = text[:1500]

        try:
            input_ids = tokenizer(
                input_text,
                max_length=512,
                padding=True,
                truncation=True,
                return_tensors="pt",
            )

            with torch.no_grad():
                tokens = model.generate(
                    **input_ids.to(device),
                    max_length=max_length,
                    length_penalty=0,
                    num_beams=num_beams,
                    num_return_sequences=num_beams,
                    decoder_start_token_id=tp_token_id,
                )

            for seq in tokens:
                decoded = tokenizer.decode(seq, skip_special_tokens=False)
                triples = _parse_mrebel_output(decoded)

                for triple in triples:
                    # Deduplicate across beams
                    key = (triple["head"].lower(), triple["relation"], triple["tail"].lower())
                    if key in seen:
                        continue
                    seen.add(key)

                    triple["chunk_id"] = chunk_id
                    all_triples.append(triple)

        except Exception as e:
            logger.warning(f"mREBEL extraction failed for chunk {chunk_id}: {e}")
            continue

    logger.info(f"mREBEL extracted {len(all_triples)} unique triples from {len(chunks)} chunks")
    return all_triples
