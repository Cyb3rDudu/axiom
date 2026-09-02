"""Pure compute boundary (contract §5.5).

``compute(request, work_dir, set_stage=None) -> ProcessorResult`` is the
single entry point used by the HTTP layer. It performs no database,
OpenSearch, graph or Zotero writes — it only reads the supplied source,
converts, chunks and assembles a contract-shaped result plus temporary
artifact files. ``set_stage`` advances the live job stage (§9, issue #122)
while compute runs; the per-stage UTC timestamps land in
``manifest.stage_timings``.

Two backends are selectable via ``AXIOM_PROCESSOR_COMPUTE``:

* ``reference`` (default, hermetic for tests): converts a PDF via PyMuPDF /
  EPUB via zipfile, reuses the existing pure ``Chunker``, and produces
  contract-shaped results with honest provenance. No heavy models.
* ``real``: wires the existing Marker/pdf_worker, epub_worker, embedder,
  entity and relation extractors behind this same boundary. Heavy dependencies
  (torch, FlagEmbedding, gliner, mrebel) are imported lazily and only when the
  host can load them.
"""

from __future__ import annotations

import hashlib
import logging
import os
import re
import sys
import time
import zipfile
from collections.abc import Callable
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from . import CONTRACT_VERSION, DENSE_EMBEDDING_DIM, DENSE_EMBEDDING_MODEL
from .config import settings
from .validation import SourceError

log = logging.getLogger(__name__)



# ---------------------------------------------------------------------------
# Contract-shaped result assembly helpers
# ---------------------------------------------------------------------------


def _sha256_hex(path: Path) -> str:
    h = hashlib.sha256()
    try:
        with open(path, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                h.update(block)
    except (OSError, ValueError) as err:
        raise RuntimeError(f"failed to hash {path}: {err}") from err
    return h.hexdigest()


_MD_ESCAPE_RE = re.compile(r"\\(.)")


def _drop_link_refs(refs: list[Any]) -> list[Any]:
    r"""Runner-Härtung Z2: http(s)-Refs sind LINKS, keine Bild-Artefakte.

    Bewiesene Klasse (Job 81d39c05, drei Chunks): die Marker-Ligatur-
    Fehllesung 'ft'->'!' macht aus Inline-Links Markdown-Bild-Syntax
    ('Wirtscha!spsychologie … ![spsychologie-aktuell.de\)](http://…)')
    und der Chunker-Regex übernimmt die externe URL als image_ref — der
    Persist-Gate blockiert zu Recht (CHUNK_IMAGE_REF_UNRESOLVED). Externe
    URLs können nie lokale Artefakte sein; sie fallen HIER aus den Refs.
    Lokale Refs bleiben unberührt — das Gate bleibt strikt."""
    return [r for r in (refs or [])
            if not str(r.get("path", r) if isinstance(r, dict) else r).startswith(("http://", "https://"))]


def _normalize_image_refs(refs: Any) -> list[str]:
    """Normalize the existing chunker's image_refs (list of dicts with
    'path'/'alt_text'/'position') to plain string refs (contract §11).

    #192: Marker emits markdown image paths with ESCAPED specials
    (``\\_page\\_6\\_Figure\\_5.jpeg``) while artifacts are stored
    UNESCAPED — the raw capture fails the artifact-ref persist gate
    (CHUNK_IMAGE_REF_UNRESOLVED). Markdown backslash escapes are
    collapsed to their literal character."""
    if not refs:
        return []
    out: list[str] = []
    for r in refs:
        if isinstance(r, str):
            raw = r
        elif isinstance(r, dict):
            # Use the path/filename as the ref string.
            raw = str(r.get("path", r.get("alt_text", "")))
        else:
            raw = str(r)
        out.append(_MD_ESCAPE_RE.sub(r"\1", raw))
    return out


def _merge_marker_pages(meta: dict, pagemap_max: int | None) -> None:
    """#226 F2 merge: when the anchor map is trusted, the chunker's {N}
    marker boundaries are the SAME anchor source and char-exact — they
    ride along on the epub_cfi locator (per-hit page resolution, #194
    shape) and override the fuzzy cfi-text-match envelope so envelope
    and boundaries can never disagree.

    W2 tail-restart guard: pagemap_max is the trusted map's max page WHEN
    the map was tail-trimmed (appendix/index numbering restart) — marker
    labels beyond it live in the restarted numbering space and are
    dropped (chunk keeps the cfi envelope, no paragraph_pages), never
    mixed under one trust stamp."""
    if meta.get("page_start") is None or not meta.get("paragraph_pages"):
        return
    bounds = meta["paragraph_pages"]
    if pagemap_max is not None and (
        int(str(bounds[0][1])) > pagemap_max
        or int(str(bounds[-1][1])) > pagemap_max
    ):
        return
    meta["epub_paragraph_pages"] = meta["paragraph_pages"]
    meta["page_start"] = int(str(bounds[0][1]))
    meta["page_end"] = int(str(bounds[-1][1]))


def _freeze_apa_fields(
    meta: dict, chapter_starts: list[tuple[int, str]]
) -> None:
    """#245 APA-7 citation fields, frozen at ingest — always together or
    not at all, never guessed.

    Chapter = level-1 markdown heading (the deterministic pandoc DOM
    structure; no reader/device state). paragraph_in_chapter counts
    paragraphs from the chapter heading to the chunk's START paragraph,
    1-based — the heading itself is not a paragraph. Books with no level-1
    structure after the front matter leave the fields empty."""
    if not chapter_starts:
        return
    spi = meta.get("start_paragraph_index")
    if not isinstance(spi, int):
        return
    chapter_no = 0
    chapter_para = -1
    for idx, _title in chapter_starts:
        if idx <= spi:
            chapter_no += 1
            chapter_para = idx
        else:
            break
    if chapter_no == 0:
        return  # before the first chapter (front matter) — no chapter context
    meta["chapter_number"] = chapter_no
    sections = meta.get("section_titles") or []
    if sections:
        meta["section_title"] = sections[-1]  # deepest section title
    meta["paragraph_in_chapter"] = max(1, spi - chapter_para)


def _adapt_embeddings(raw: Any) -> dict[str, Any]:
    """Pass through real embeddings from TextEmbedder.embed_chunks() into the
    Contract chunk shape. The real embedder writes
    chunk['embeddings'] = {'dense': [float...], 'sparse': {token_id: weight}}.
    We reformat sparse keys to strings (Contract §10: sparse values keys are
    strings). Returns {} if no real embeddings present (the reference stub
    fills in later)."""
    if not raw or not isinstance(raw, dict):
        return {}
    out: dict[str, Any] = {}
    dense = raw.get("dense")
    if dense and isinstance(dense, list):
        try:
            vals = [float(v) for v in dense]
        except (TypeError, ValueError):
            vals = []
        if vals:
            out["dense"] = {
                "model": DENSE_EMBEDDING_MODEL,
                "dimensions": len(vals),
                "values": vals,
            }
    sparse = raw.get("sparse")
    if sparse and isinstance(sparse, dict):
        sp: dict[str, str] = {}
        for k, v in sparse.items():
            try:
                sp[str(k)] = str(float(v))
            except (TypeError, ValueError):
                continue
        if sp:
            out["sparse"] = {"model": DENSE_EMBEDDING_MODEL, "values": sp}
    return out


def _stamp_page_source(
    locator: dict[str, Any], page_source_map: dict[int, str] | None
) -> None:
    """#173 trust stamp on every locator: never guess — every page reference
    carries its level. epub_cfi is 'none' (no stable pages); page_span takes
    the trust level of its START physical page, defaulting honestly to
    physical_only when no map was computed (reference mode)."""
    from axiom_ng_runner.compute_core import page_trust as pt

    if locator.get("type") == "epub_cfi":
        # #223/#226: the wire trust set — print_verified / derived_from_
        # sibling / print_unverified — set ONLY when the locator carries
        # pages; never silently upgraded.
        if locator.get("page_start") is not None:
            locator["page_source"] = locator.get("page_trust") or pt.PRINT_UNVERIFIED
        else:
            locator["page_source"] = pt.NONE
        # page_trust is a runner-internal handoff into this stamp — the
        # trust travels via page_source; the wire locator must not carry
        # an unknown field (the Go persist boundary re-marshals and drops
        # unknowns silently — W9 lesson, kept clean here).
        locator.pop("page_trust", None)
        return
    if locator.get("page_source"):
        return  # already stamped by a previous _stamp_page_source pass
    phys = locator.get("physical_page_start")
    if page_source_map and phys is not None:
        lvl = page_source_map.get(int(phys), pt.PHYSICAL_ONLY)
        locator["page_source"] = lvl
        pe = locator.get("physical_page_end")
        if pe is not None and page_source_map.get(int(pe)) != lvl:
            # #173: never mix numbering spaces — when the END page lies
            # outside the start page's trust level (e.g. a verified folio run
            # ending in unpaginated front matter), the end label belongs to a
            # different space and is dropped, not displayed as one span.
            locator.pop("page_label_end", None)
    else:
        locator["page_source"] = pt.PHYSICAL_ONLY


def _stamp_chapter(
    locator: dict[str, Any],
    page_chapter_map: dict[int, int] | None,
    chapter_meta: Any = None,
) -> None:
    """W12 chapter-ordinal stamp (W4 renders "Kap. N, S. X"). Only for
    corroborated chapter-relative books (page_chapter_map non-empty) —
    otherwise the locator stays exactly as before. The chunker's exact
    ordinal (chapter_meta, from the physical page of the chunk's first
    content) wins; pre-shaped reference locators fall back to their own
    physical_page_start lookup. Never overwrites an existing stamp."""
    if locator.get("type") != "page_span" or locator.get("chapter") is not None:
        return
    if chapter_meta is not None:
        locator["chapter"] = int(chapter_meta)
        return
    if page_chapter_map:
        phys = locator.get("physical_page_start")
        ch = page_chapter_map.get(int(phys)) if phys is not None else None
        if ch is not None:
            locator["chapter"] = ch


def _adapt_chunk(
    c: dict[str, Any],
    chunk_index: int,
    page_label_map: dict[int, str],
    page_source_map: dict[int, str] | None = None,
    page_chapter_map: dict[int, int] | None = None,
) -> dict[str, Any]:
    """Map the existing pipeline's chunk dict (chunker._create_chunk) to the
    contract chunk shape (contract §11 mapping table).

    A chunk that already carries contract shape (``ref`` + ``locator``), as
    produced by ``chunking.chunk_markdown``, passes through untouched. Old-style
    ``core_rag`` chunk dicts (logical page labels only) are adapted by
    reverse-mapping through ``page_label_map``.
    """
    if c.get("ref") and c.get("locator") and c.get("index") is not None:
        _stamp_page_source(c["locator"], page_source_map)
        _stamp_chapter(c["locator"], page_chapter_map)
        return c

    meta = c.get("metadata", {}) or {}
    section_titles = meta.get("section_titles", []) or []

    label_to_physical: dict[str, list[int]] = {}
    for phys, label in page_label_map.items():
        label_to_physical.setdefault(str(label), []).append(phys)

    def _physical(label: Any) -> int | None:
        if label is None or label == "":
            return None
        hits = label_to_physical.get(str(label), [])
        if hits:
            return min(hits)
        try:
            return int(str(label)) - 1  # fallback: numeric label N == page N-1
        except ValueError:
            return None

    locator_type = meta.get("locator_type")
    if locator_type == "epub_cfi":
        locator = {
            "type": "epub_cfi",
            "cfi_start": meta.get("cfi_start", ""),
            "cfi_end": meta.get("cfi_end", ""),
            "source": meta.get("locator_source", "epub"),
        }
        # #220/#223 citation parity: print pages from the anchor map (only
        # when monotone AND plausible AND TOC-corroborated-or-unproven —
        # divergent/implausible maps are refused upstream) and
        # chapter_number parity with PDF locators (1-based spine ordinal).
        # page_trust carries the #223/#226 verdict into the trust stamp.
        if meta.get("page_start") is not None:
            locator["page_start"] = int(meta["page_start"])
            locator["page_end"] = int(meta.get("page_end", meta["page_start"]))
            if meta.get("page_trust"):
                locator["page_trust"] = meta["page_trust"]
            if meta.get("epub_paragraph_pages"):
                # #194 wire shape: [[charOffset, label], ...] as STRINGS —
                # per-paragraph page boundaries so a hit position resolves
                # to its exact print page, not the span envelope.
                locator["paragraph_pages"] = [
                    [str(o), str(l)] for o, l in meta["epub_paragraph_pages"]
                ]
        if meta.get("chapter") is not None:
            locator["chapter"] = int(meta["chapter"])
        # #245 APA-7: frozen citation fields ride on the stored locator
        # (additive; absent when the book has no chapter structure).
        if meta.get("chapter_number") is not None:
            locator["chapter_number"] = int(meta["chapter_number"])
        if meta.get("section_title"):
            locator["section_title"] = str(meta["section_title"])
        if meta.get("paragraph_in_chapter") is not None:
            locator["paragraph_in_chapter"] = int(meta["paragraph_in_chapter"])
    else:
        page_start = meta.get("page_start")
        page_end = meta.get("page_end")
        # W12 review C1: prefer the chunker's exact Marker-page anchors —
        # chapter-relative books carry duplicate labels across chapters and
        # the label reverse-mapping resolves to the EARLIEST chapter's page.
        locator = {
            "type": "page_span",
            "physical_page_start": meta.get("physical_page_start", _physical(page_start)),
            "physical_page_end": meta.get("physical_page_end", _physical(page_end)),
            "page_label_start": str(page_start) if page_start is not None else "",
            "page_label_end": str(page_end) if page_end is not None else "",
            "source": meta.get("locator_source", "marker_paginate"),
        }
        # #194: per-paragraph page map — char-offset boundaries where the
        # print page changes. Additive contract field; absent on pre-#194
        # generations (consumers fall back to the span envelope).
        paragraph_pages = meta.get("paragraph_pages")
        if paragraph_pages:
            locator["paragraph_pages"] = paragraph_pages

    out = {
        "ref": f"chunk-{chunk_index:04d}",
        "index": chunk_index,
        "text": c.get("text", ""),
        "locator": locator,
        "structure": {
            "section_titles": section_titles,
            "start_paragraph_index": meta.get("start_paragraph_index", 0),
            "end_paragraph_index": meta.get("end_paragraph_index", 0),
        },
        "token_count": meta.get("token_count", 0),
        "image_refs": _normalize_image_refs(meta.get("image_refs", [])),
        # Durchreichen echter Embeddings aus dem Original-Chunk (z.B. von
        # TextEmbedder.embed_chunks), damit _build_reference_result sie nicht
        # mit dem Reference-Stub überschreibt.
        "embeddings": _adapt_embeddings(c.get("embeddings")),
        "metadata": {},
    }
    # #173: EVERY locator leaves stamped — the adapted branch is the REAL
    # production path (compute_core.Chunker old-style dicts); an unstamped
    # locator here would trip the §11 gate and terminal-fail the job.
    _stamp_page_source(out["locator"], page_source_map)
    _stamp_chapter(out["locator"], page_chapter_map, meta.get("chapter"))
    return out


def _dense_embedding(chunk: dict[str, Any]) -> dict[str, Any]:
    dims = DENSE_EMBEDDING_DIM
    digest = hashlib.sha256(chunk["text"].encode("utf-8")).digest()
    values = [round((digest[i % len(digest)] / 255.0) * 2 - 1, 6) for i in range(dims)]
    return {
        "model": DENSE_EMBEDDING_MODEL,
        "dimensions": dims,
        "values": values,
    }


def _sparse_embedding(text: str) -> dict[str, Any]:
    tokens = text.lower().split()
    counts: dict[str, int] = {}
    for t in tokens:
        counts[t] = counts.get(t, 0) + 1
    values: dict[str, float] = {}
    for tok, n in counts.items():
        # Deterministic bucket via SHA-256 (stable, non-cryptographic use).
        digest = hashlib.sha256(tok.encode("utf-8")).hexdigest()
        key = str(int(digest, 16) % 1000)
        values[key] = round(n * 0.5, 6)
    return {"model": "reference-bge-m3", "values": values}


def _reference_entities(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Deterministic entity extraction: proper-noun-ish tokens as spans."""
    import re

    entities: list[dict[str, Any]] = []
    seen: dict[str, str] = {}
    for chunk in chunks:
        text = chunk["text"]
        for m in re.finditer(r"\b[A-Z][a-zA-Z0-9_]{2,}\b", text):
            token = m.group(0)
            eid = seen.get(token)
            if eid is None:
                eid = f"entity-{len(entities) + 1:04d}"
                seen[token] = eid
                entities.append(
                    {
                        "ref": eid,
                        "text": token,
                        "canonical_form": token.lower(),
                        "type": "METHOD",
                        "description": None,
                        "mentions": [],
                    }
                )
            entities[-1]["mentions"].append(
                {
                    "chunk_ref": chunk["ref"],
                    "start_char": m.start(),
                    "end_char": m.end(),
                    "confidence": 0.9,
                }
            )
    return entities


def _reference_relationships(
    entities: list[dict[str, Any]], chunks: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    # Evidence for a relationship is the chunk where the target entity actually
    # occurs (from its first mention), never an unsafe positional index that can
    # overflow when entity count exceeds chunk count.
    chunk_by_entity: dict[str, str] = {}
    for e in entities:
        mentions = e.get("mentions") or []
        if mentions:
            chunk_by_entity[e["ref"]] = mentions[0]["chunk_ref"]

    rels: list[dict[str, Any]] = []
    for i in range(1, len(entities)):
        src, tgt = entities[i - 1]["ref"], entities[i]["ref"]
        if src == tgt or tgt not in chunk_by_entity:
            continue
        rels.append(
            {
                "source_entity_ref": src,
                "target_entity_ref": tgt,
                "type": "co_occurs",
                "strength": 0.7,
                "evidence_chunk_refs": [chunk_by_entity[tgt]],
                "extractor": "reference-mrebel",
                "metadata": {},
            }
        )
    return rels


# ── L6: real entity/relationship extraction (GLiNER + mREBEL) ────────────

_GLINER_MODEL: Any = None


def _get_gliner() -> Any:
    """Load GLiNER once per process (module-level cache, mirroring
    relation_extractor.load_mrebel's pattern). Deliberately in-process — NOT
    via gpu_worker/model_cache: the runner is already the heavy process (same
    reasoning as TextEmbedder). Device placement follows the repo's hardware
    detector; if the detector is unavailable the model stays wherever
    from_pretrained put it."""
    global _GLINER_MODEL
    if _GLINER_MODEL is not None:
        return _GLINER_MODEL
    from gliner import GLiNER

    _GLINER_MODEL = GLiNER.from_pretrained("urchade/gliner_multi-v2.1")
    try:
        from axiom_ng_runner.compute_core.devices import hardware_detector

        _GLINER_MODEL = _GLINER_MODEL.to(
            hardware_detector.get_model_device("gliner")
        )
    except Exception as err:  # noqa: BLE001 — detector is optional here
        log.warning("GLiNER device placement skipped: %s", err)
    return _GLINER_MODEL


def _extract_real_entities(
    chunk_items: list[tuple[str, str]],
    on_progress: Callable[[int, int], None] | None = None,
) -> list[dict[str, Any]]:
    """GLiNER zero-shot NER → contract entities.

    chunk_items: [(chunk_ref, text)]. Entities are grouped by
    (whitespace-normalized text.lower(), type); every occurrence becomes a
    mention with chunk-local char offsets. Reuses the established
    labels/type-map/filters from compute_core.entity_extractor.

    #236: on_progress (done, total) is reported per chunk — pure reporting,
    no effect on extraction. GLiNER predicts per chunk anyway, so the
    chunk granularity is the natural batch; the app-level callback
    throttles the store writes exactly like relationships.
    """
    from axiom_ng_runner.compute_core.entity_extractor import (
        _GENERIC_WORDS,
        _GLINER_TYPE_MAP,
        _NOISE_RE,
        GLINER_LABELS,
    )

    model = _get_gliner()
    entities: list[dict[str, Any]] = []
    seen: dict[tuple[str, str], int] = {}

    total = len(chunk_items)
    for i, (chunk_ref, text) in enumerate(chunk_items, start=1):
        if on_progress is not None:
            on_progress(i, total)
        if not text.strip():
            continue
        try:
            spans = model.predict_entities(
                text, GLINER_LABELS, threshold=0.45, multi_label=True
            )
        except Exception as err:  # noqa: BLE001 — one bad chunk must not kill the job
            log.warning("GLiNER prediction failed for %s: %s", chunk_ref, err)
            continue
        for e in spans:
            # Trust boundary: GLiNER spans are model output — skip malformed
            # entries BEFORE creating any entity: missing keys or unparseable
            # offsets must neither kill the job nor leave a mention-less
            # entity.
            try:
                ent_text = e["text"].strip()
                ent_type = _GLINER_TYPE_MAP.get(e["label"])
                start = int(e.get("start", 0))
                confidence = round(float(e.get("score", 0.5)), 3)
            except (AttributeError, KeyError, TypeError, ValueError):
                continue
            if not ent_type or len(ent_text) < 2 or len(ent_text) > 100:
                continue
            if _NOISE_RE.search(ent_text):
                continue
            if len(ent_text.split()) == 1 and ent_text.lower() in _GENERIC_WORDS:
                continue
            key = (" ".join(ent_text.lower().split()), ent_type)
            idx = seen.get(key)
            if idx is None:
                idx = len(entities)
                seen[key] = idx
                entities.append(
                    {
                        "ref": f"entity-{idx + 1:04d}",
                        "text": ent_text,
                        "canonical_form": ent_text.lower(),
                        "type": ent_type,
                        "description": None,
                        "mentions": [],
                    }
                )
            entities[idx]["mentions"].append(
                {
                    "chunk_ref": chunk_ref,
                    "start_char": start,
                    "end_char": start + len(e["text"]),
                    "confidence": confidence,
                }
            )
    return entities


def _extract_real_relationships(
    entities: list[dict[str, Any]],
    chunk_dicts: list[dict[str, Any]],
    chunk_texts: dict[str, str],
    deadline: float | None = None,
    on_progress: Callable[[int, int], None] | None = None,
) -> tuple[list[dict[str, Any]], bool]:
    """mREBEL triples → contract relationships.

    #225: extraction runs in batches with per-batch progress (done/total
    chunks — the stage counter never shows 0/0 again) and an optional
    wall-clock deadline; on expiry the loop stops BETWEEN batches and the
    caller completes the job honestly with the committed partial result
    (STAGE_BUDGET_EXCEEDED), never an eternal lease.

    Triple endpoints are matched against the GLiNER entities (exact, then
    substring); unmatched endpoints become new entities — mREBEL finds
    entities GLiNER misses, and the validator only needs refs to resolve.
    Mutates `entities` (may append). Relationship dedup merges evidence.
    Returns (relationships, budget_exceeded).
    """
    import re

    from axiom_ng_runner.compute_core.relation_extractor import (
        extract_relations_from_chunks,
    )

    triples: list[dict[str, Any]] = []
    budget_exceeded = False
    BATCH = 20
    if not chunk_dicts:  # pinned behavior: one extractor call even when empty
        triples = extract_relations_from_chunks([])
    for i in range(0, len(chunk_dicts), BATCH):
        if deadline is not None and time.monotonic() > deadline:
            budget_exceeded = True
            log.warning("relationships deadline hit before batch at %d/%d", i, len(chunk_dicts))
            break
        triples.extend(extract_relations_from_chunks(chunk_dicts[i:i + BATCH]))
        if on_progress is not None:
            on_progress(min(i + BATCH, len(chunk_dicts)), len(chunk_dicts))

    by_text: dict[str, str] = {}
    for e in entities:
        by_text.setdefault(" ".join(e["text"].lower().split()), e["ref"])

    def _resolve(name: str, ent_type: str, chunk_ref: str) -> str:
        key = " ".join(name.lower().split())
        if key in by_text:
            return by_text[key]
        # Substring matches only for names long enough to be distinctive on
        # BOTH sides — "UN" must not substring-match "united nations" (and
        # an "ESG" entity must not swallow "ESG frameworks"). Shorter names
        # require an exact match or become their own entity.
        if len(key) >= 4:
            for t, ref in by_text.items():
                if len(t) >= 4 and (key in t or t in key):
                    return ref
        eid = f"entity-{len(entities) + 1:04d}"
        by_text[key] = eid
        text = chunk_texts.get(chunk_ref, "")
        start = text.find(name)
        entities.append(
            {
                "ref": eid,
                "text": name.strip(),
                "canonical_form": key,
                "type": ent_type,
                "description": None,
                "mentions": (
                    [
                        {
                            "chunk_ref": chunk_ref,
                            "start_char": start,
                            "end_char": start + len(name),
                            "confidence": 0.6,
                        }
                    ]
                    if start >= 0
                    else []
                ),
            }
        )
        return eid

    rels: list[dict[str, Any]] = []
    rel_index: dict[tuple[str, str, str], int] = {}
    for tr in triples:
        chunk_ref = tr.get("chunk_id", "")
        if not chunk_ref:
            continue
        src = _resolve(tr["head"], tr.get("head_type", "CONCEPT"), chunk_ref)
        tgt = _resolve(tr["tail"], tr.get("tail_type", "CONCEPT"), chunk_ref)
        if src == tgt:
            continue
        rtype = re.sub(r"\s+", "_", tr["relation"].lower())[:64]
        dkey = (src, tgt, rtype)
        idx = rel_index.get(dkey)
        if idx is not None:
            if chunk_ref not in rels[idx]["evidence_chunk_refs"]:
                rels[idx]["evidence_chunk_refs"].append(chunk_ref)
            continue
        rel_index[dkey] = len(rels)
        rels.append(
            {
                "source_entity_ref": src,
                "target_entity_ref": tgt,
                "type": rtype,
                "strength": 0.7,
                "evidence_chunk_refs": [chunk_ref],
                "extractor": "mrebel-large",
                "metadata": {},
            }
        )
    return rels, budget_exceeded


def _assign_contract_chunk_ids(chunk_dicts: list[dict[str, Any]]) -> None:
    """Overwrite the Chunker's doc-local chunk_id ("{doc_id}_chunk_0000")
    with the deterministic contract ref (chunk-{i:04d}, same scheme as
    _adapt_chunk) so mREBEL evidence chunk_refs resolve without a second
    mapping. In-place; other metadata keys survive, missing/None metadata
    is created without dropping sibling keys (lost-update-safe)."""
    for i, c in enumerate(chunk_dicts):
        meta = c.setdefault("metadata", {})
        if not isinstance(meta, dict):
            meta = {}
            c["metadata"] = meta
        meta["chunk_id"] = f"chunk-{i:04d}"


def _build_reference_result(
    request: dict[str, Any],
    work_dir: Path,
    chunk_dicts: list[dict[str, Any]],
    page_label_map: dict[int, str],
    markdown_path: Path,
    attachment_id: str,
    content_hash: str,
    source_page_count: int,
    image_artifacts: list[dict[str, Any]] | None = None,
    real_entities: list[dict[str, Any]] | None = None,
    real_relationships: list[dict[str, Any]] | None = None,
    stage_timings: dict[str, str] | None = None,
    page_source_map: dict[int, str] | None = None,
    page_chapter_map: dict[int, int] | None = None,
) -> dict[str, Any]:
    proc = request.get("processing", {}) or {}
    processor_name = "axiom-python-marker"
    processor_version = "0.1.0"

    md_sha = _sha256_hex(markdown_path)
    md_size = markdown_path.stat().st_size
    artifacts: list[dict[str, Any]] = [
        {
            "ref": "markdown",
            "kind": "markdown",
            "media_type": "text/markdown; charset=utf-8",
            "sha256": md_sha,
            "size_bytes": md_size,
            "retention": "durable",
        }
    ]
    if image_artifacts:
        artifacts.extend(image_artifacts)

    chunks: list[dict[str, Any]] = []
    for idx, c in enumerate(chunk_dicts):
        ch = _adapt_chunk(c, idx, page_label_map, page_source_map, page_chapter_map)
        # Bedingte Fill-Logik (b): nur mit Reference-Stub füllen, wenn keine
        # echten Embeddings aus _adapt_embeddings durchgereicht wurden. Der
        # Real-Backend (TextEmbedder.embed_chunks) setzt echte BGE-M3-Vektoren
        # in c['embeddings']; der Reference-Backend liefert keine echten.
        if proc.get("compute_dense_embeddings") and not ch["embeddings"].get("dense"):
            ch["embeddings"]["dense"] = _dense_embedding(ch)
        if proc.get("compute_sparse_embeddings") and not ch["embeddings"].get("sparse"):
            ch["embeddings"]["sparse"] = _sparse_embedding(ch["text"])
        chunks.append(ch)

    # Dense-Modellname ehrlich (Known Gap Carrier-POC): echte Vektoren, die
    # TextEmbedder in die chunk_dicts gelegt hat, bedeuten BAAI/bge-m3 —
    # gleiche datengetriebene Variante-b-Erkennung wie beim Fill oben.
    dense_model = (
        "BAAI/bge-m3"
        if any((c.get("embeddings") or {}).get("dense") for c in chunk_dicts)
        else "reference-bge-m3"
    )

    # L6 bedingte Füllung (wie embeddings, Variante b): echte GLiNER/mREBEL-
    # Ergebnisse aus _real_pipeline werden durchgereicht; nur wenn KEINE
    # geliefert wurden (Reference-Backend), greifen die Stubs.
    entities = real_entities if real_entities is not None else []
    if real_entities is None and proc.get("extract_entities"):
        entities = _reference_entities(chunks)

    entity_relationships = (
        real_relationships if real_relationships is not None else []
    )
    if (
        real_relationships is None
        and proc.get("extract_relationships")
        and entities
    ):
        entity_relationships = _reference_relationships(entities, chunks)

    stats = {
        "pages": source_page_count,
        "chunks": len(chunks),
        "artifacts": len(artifacts),
        "entities": len(entities),
        "entity_relationships": len(entity_relationships),
        "chunk_relationships": 0,
    }

    manifest = {
        "source_page_count": source_page_count,
        "page_label_map": {str(k): v for k, v in page_label_map.items()},
    }
    if stage_timings:
        # Per-stage UTC start timestamps (§9): post-hoc reconstruction of
        # where a book spent its time, no live observation needed.
        manifest["stage_timings"] = stage_timings

    return {
        "contract_version": CONTRACT_VERSION,
        "job_id": request["job_id"],
        "status": "completed",
        "source": {
            "attachment_id": attachment_id,
            "content_hash": content_hash,
            "verified": True,
        },
        "processor": {
            "name": processor_name,
            "version": processor_version,
            "profile": proc.get("profile", "full-rag-v1"),
            "profile_hash": None,  # Go records the profile hash from the request.
            "models": {
                "marker": "marker-1.0",
                "dense_embedding": dense_model,
                "entity_extraction": (
                    "urchade/gliner_multi-v2.1"
                    if real_entities is not None
                    else "reference-gliner"
                ),
                "relationship_extraction": (
                    "Babelscape/mrebel-large"
                    if real_relationships is not None
                    else "reference-mrebel"
                ),
            },
        },
        "artifacts": artifacts,
        "manifest": manifest,
        "chunks": chunks,
        "entities": entities,
        "chunk_relationships": [],
        "entity_relationships": entity_relationships,
        "stats": stats,
        "warnings": [],
    }


# ---------------------------------------------------------------------------
# Reference conversion (PDF via PyMuPDF, EPUB via zipfile)
# ---------------------------------------------------------------------------


def _convert_reference(
    request: dict[str, Any], source_path: Path, work_dir: Path
) -> tuple[str, dict[int, str], int, Path]:
    """Convert source to markdown + page label map. Pure, hermetic.

    Returns (markdown, page_label_map, page_count, markdown_path).
    """
    md_path = work_dir / "markdown.md"
    content_type = request["attachment"]["content_type"]

    if content_type == "application/pdf":
        markdown, page_label_map, page_count = _convert_pdf_reference(
            source_path, md_path
        )
    else:  # application/epub+zip
        markdown, page_label_map, page_count = _convert_epub_reference(
            source_path, md_path
        )

    md_path.write_text(markdown, encoding="utf-8")
    return markdown, page_label_map, page_count, md_path


def _convert_pdf_reference(pdf_path: Path, md_path: Path):
    import pymupdf  # PyMuPDF

    doc = pymupdf.open(str(pdf_path))
    pages: list[str] = []
    for i in range(doc.page_count):
        page = doc.load_page(i)
        text = page.get_text("text").strip()
        if text:
            # Marker-style page marker that the Chunker understands.
            pages.append(f"{{{i}}}{'-' * 10}\n\n{text}")
    doc.close()

    from .chunking import extract_page_labels

    page_label_map = extract_page_labels(str(pdf_path))
    markdown = "\n\n".join(pages) if pages else "<!-- empty pdf -->"
    return markdown, page_label_map, len(pages) if pages else 0


def _convert_epub_reference(epub_path: Path, md_path: Path):
    """Minimal EPUB -> markdown for the reference backend (zipfile only)."""
    import re as _re

    chunks_text: list[str] = []
    with zipfile.ZipFile(epub_path) as z:
        names = z.namelist()
        html_files = [
            n
            for n in names
            if n.lower().endswith((".xhtml", ".html"))
            and not n.startswith("META-INF/")
            and "/nav" not in n.lower()
        ]
        html_files.sort()
        for name in html_files:
            raw = z.read(name).decode("utf-8", errors="replace")
            # Very light HTML-to-text: headings and paragraphs.
            raw = _re.sub(r"(?i)<br\s*/?>", "\n", raw)
            raw = _re.sub(r"(?i)<(?:h[1-6])[^>]*>", "\n", raw)
            raw = _re.sub(r"(?i)</(?:h[1-6])[^>]*>", "\n", raw)
            raw = _re.sub(r"(?i)</p>", "\n", raw)
            raw = _re.sub(r"<[^>]+>", " ", raw)
            raw = _re.sub(r"[ \t]+", " ", raw)
            text = "\n".join(line.strip() for line in raw.splitlines() if line.strip())
            if text:
                chunks_text.append(text)
    markdown = "\n\n".join(chunks_text) if chunks_text else "<!-- empty epub -->"
    return markdown, {}, 0


# ---------------------------------------------------------------------------
# Public boundary
# ---------------------------------------------------------------------------


class ComputeEnvironmentError(RuntimeError):
    """compute=real but the heavy compute environment is broken (missing
    deps/imports). This is an environment/config problem: the job must FAIL
    RETRYABLY — never silently degrade to the reference stub (#240)."""


def compute(
    request: dict[str, Any],
    work_dir: Path,
    set_stage: Callable[[str], None] | None = None,
    commit: Callable[[dict[str, Any]], None] | None = None,
    set_progress: Callable[[int, int, str], None] | None = None,
    runtime: Any | None = None,
) -> dict[str, Any]:
    """Run the pure compute pipeline for a validated source. Returns a
    contract processor-result dict (contract §10). Work dir is per-job and
    already validated by the caller. ``set_stage`` advances the live job
    stage (GET /v1/jobs) while compute runs (§9). ``commit`` (#225
    early-commit) lets the pipeline persist the so-far-complete result
    before the relationships stage runs; ``set_progress`` reports §9
    progress for long-running stage loops. ``runtime`` (#242) is the per-job
    runtime used to register the real backend's conversion subprocess so
    cancel can terminate it."""
    backend = settings.get().compute_backend
    if backend == "real":
        try:
            result = _compute_real(
                request, work_dir, set_stage, commit, set_progress, runtime
            )
        except ComputeEnvironmentError:
            # #240: a broken real-mode environment is a FAILED, RETRYABLE job —
            # never a silent reference run (batch compute on a stub is index
            # poisoning with no signal). Explicit opt-out only, loud + marked.
            if os.environ.get("ALLOW_REFERENCE_FALLBACK") != "1":
                raise
            # Ordering property of the opt-out: a real run that dies AFTER
            # the #225 early-commit (e.g. a relationships-stage import) still
            # falls back — the reference result then REPLACES the retrievable
            # real partial via set_result. Loud + marked, but a fallback run
            # can overwrite a real partial; callers must know this.
            log.warning(
                "ALLOW_REFERENCE_FALLBACK=1: compute=real failed; DEGRADING TO "
                "THE REFERENCE STUB — the result is NOT real compute output"
            )
            result = _compute_reference(request, work_dir, set_stage)
            proc = result.get("processor") or {}
            proc["reference_fallback"] = True
            result["processor"] = proc
            return result
        if result is not None:
            return result
        # _compute_real returning None means the real pipeline declined
        # (e.g. cancel-before-start) — the caller settles the job; do NOT
        # silently fall through to the reference stub here (#240 discipline).
        return None
    return _compute_reference(request, work_dir, set_stage)


def _stage_tracker(
    set_stage: Callable[[str], None] | None,
) -> tuple[Callable[[str], None], dict[str, str]]:
    """Live-stage callback + per-stage UTC timestamps for the manifest.

    enter(stage) records when the stage began and mirrors it into the job
    store so /v1/jobs shows where a book is spending its time; the timings
    land in manifest.stage_timings for post-hoc benchmark analysis."""
    timings: dict[str, str] = {}

    def enter(stage: str) -> None:
        timings[stage] = datetime.now(timezone.utc).isoformat()
        if set_stage is not None:
            set_stage(stage)

    return enter, timings


def _int_keys(mapping: dict[Any, Any]) -> dict[int, Any]:
    """Coerce mapping keys to int where possible (page label maps)."""
    out: dict[int, Any] = {}
    for k, v in mapping.items():
        try:
            out[int(k)] = v
        except (TypeError, ValueError):
            continue
    return out


def _compute_reference(
    request: dict[str, Any], work_dir: Path, set_stage: Callable[[str], None] | None = None
) -> dict[str, Any]:
    enter, timings = _stage_tracker(set_stage)
    attach = request["attachment"]
    source_path = Path(attach["local_path"])

    enter("convert")
    markdown, page_label_map, page_count, md_path = _convert_reference(
        request, source_path, work_dir
    )
    page_label_map = _int_keys(page_label_map)

    # Pure, hermetic deterministic chunker (stdlib-only). Produces chunks in
    # final contract shape directly (see chunking.chunk_markdown).
    enter("chunk")
    from .chunking import chunk_markdown

    chunk_dicts = chunk_markdown(markdown, page_label_map)

    # Same stage shape as the real backend (stubs are instant, but
    # /v1/jobs must not look backend-dependent).
    enter("embed")
    enter("entities")
    enter("relationships")
    enter("assemble")
    return _build_reference_result(
        request=request,
        work_dir=work_dir,
        chunk_dicts=chunk_dicts or [],
        page_label_map=page_label_map,
        markdown_path=md_path,
        attachment_id=attach["attachment_id"],
        content_hash="sha256:" + _sha256_hex(source_path),
        source_page_count=page_count,
        stage_timings=timings,
    )


def _compute_real(
    request: dict[str, Any],
    work_dir: Path,
    set_stage: Callable[[str], None] | None = None,
    commit: Callable[[dict[str, Any]], None] | None = None,
    set_progress: Callable[[int, int, str], None] | None = None,
    runtime: Any | None = None,
) -> dict[str, Any] | None:
    """Wire the existing heavy pipeline (Marker/pdf_worker, epub_worker,
    embedder, extractors). Raises ComputeEnvironmentError if the heavy deps
    are unavailable — the caller must fail the job (#240), never silently
    fall back. The contract black-box suite runs against ``reference``."""
    try:
        return _real_pipeline(
            request, work_dir, set_stage, commit, set_progress, runtime
        )
    except ImportError as err:
        raise ComputeEnvironmentError(
            f"compute=real but the real compute pipeline is unavailable: {err}"
        ) from err


def _enrich_epub_cfi_locators(
    chunk_dicts: list[dict[str, Any]],
    cfi_entries: list[dict[str, Any]],
    marker_pagemap_max: int | None = None,
) -> None:
    """§11 Weg A: epub_cfi locators for EPUB chunks, with #220/#223/#226
    anchor-map pages and #234 interior interpolation. In place.

    #234: a publisher chapter wrapped in ONE top-level element yields a
    single chapter-wide cfi entry — every print anchor of that chapter
    lands on it. ``annotate_cfi_entries`` exposes the interior anchor
    run (``anchor_offsets``); here each chunk's position inside the entry
    resolves its print page from that run. Refused maps (non-monotone,
    implausible, divergent) never annotate entries at all — no run, no
    interpolation, page_source stays none (#226 discipline unchanged)."""
    if not cfi_entries:
        return
    from .compute_core import epub_pagelist
    from .epub_cfi import match_text_to_cfi

    pos_by_cfi = {
        e["cfi"]: (e.get("page"), e.get("spine"), e.get("page_trust"))
        for e in cfi_entries
    }
    entry_by_cfi = {e["cfi"]: e for e in cfi_entries}
    for c in chunk_dicts:
        # setdefault (not `or {}`): an EMPTY metadata dict is falsy and the
        # old idiom silently detached it, dropping every enrichment
        meta = c.setdefault("metadata", {})
        meta["locator_type"] = "epub_cfi"
        text = c.get("text", "")
        cfi_start, cfi_end = match_text_to_cfi(text, cfi_entries)
        meta["cfi_start"] = cfi_start
        meta["cfi_end"] = cfi_end
        # #220: carry anchor-map pages + spine ordinal through to
        # _adapt_chunk's locator (absent without a monotone map).
        ps, sp, ptrust = pos_by_cfi.get(cfi_start, (None, None, None))
        if ps is not None:
            # #234: interior interpolation within the verified anchor run
            entry = entry_by_cfi.get(cfi_start)
            interp = bool(ptrust and entry)
            ip_start = (epub_pagelist.interior_page(entry, text)
                        if interp else None)
            meta["page_start"] = ip_start or ps
            ip_end = (epub_pagelist.interior_page(entry, text, tail=True)
                      if interp else None)
            meta["page_end"] = max(
                meta["page_start"],
                ip_end or pos_by_cfi.get(cfi_end, (ps, None, None))[0] or ps)
            meta["page_trust"] = ptrust
        else:
            # #226 F2: refused map — the chunker's {N} marker labels are
            # the same refused anchors; never let them leak as pages.
            meta.pop("page_start", None)
            meta.pop("page_end", None)
            meta.pop("page_trust", None)
        if sp is not None:
            meta["chapter"] = sp + 1  # 1-based spine ordinal (PDF parity)
        # #226 F2 merge: marker page boundaries (same anchor source,
        # char-exact) ride along and sharpen the envelope — unless the
        # map was tail-trimmed and the labels leave the trusted range.
        _merge_marker_pages(meta, marker_pagemap_max)


def _normalize_epub_image_paths(markdown: str) -> str:
    """#124: strip machine-specific temp paths from EPUB image references.

    The epub worker converts into a temp dir whose name carries a random
    suffix (``/tmp/epub_media_<random>/images/…``). Pandoc emits that
    absolute path both as markdown image links ``![alt](path)`` and as raw
    HTML ``<img src="path" …>`` — the latter survived into chunk texts
    (TC2: Sonko/Demystifying, 27 chunks) and made every re-run
    byte-different. Both forms are rewritten to the file basename, which is
    stable across runs; the artifact/ref mapping later resolves the actual
    image via basename keys.
    """
    import re as _re

    markdown = _re.sub(
        r"!\[([^\]]*)\]\(([^)]+)\)",
        lambda m: f"![{m.group(1)}]({Path(m.group(2)).name})",
        markdown,
    )
    markdown = _re.sub(
        r'(<img\b[^>]*\bsrc=")([^"]+)(")',
        lambda m: m.group(1) + Path(m.group(2)).name + m.group(3),
        markdown,
    )
    return markdown


def _real_pipeline(
    request: dict[str, Any],
    work_dir: Path,
    set_stage: Callable[[str], None] | None = None,
    commit: Callable[[dict[str, Any]], None] | None = None,
    set_progress: Callable[[int, int, str], None] | None = None,
    runtime: Any | None = None,
) -> dict[str, Any]:
    import json as _json
    import subprocess
    from contextlib import suppress

    enter, stage_timings = _stage_tracker(set_stage)
    attach = request["attachment"]
    source_path = Path(attach["local_path"])
    content_type = attach["content_type"]
    out_md = work_dir / "markdown.md"
    out_images = work_dir / "images"

    enter("convert")
    convert = (
        "axiom_ng_runner.compute_core.pdf_worker"
        if content_type == "application/pdf"
        else "axiom_ng_runner.compute_core.epub_worker"
    )
    cmd = [
        sys.executable,
        "-m",
        convert,
        str(source_path),
        str(out_md),
        str(out_images),
    ]
    # #242: launch the conversion via Popen so the handle is registered with
    # the job runtime; cancel can then terminate the real process tree
    # (start_new_session -> own process group -> SIGTERM/SIGKILL the group)
    # instead of only flipping the status while the worker keeps burning.
    # ``communicate()`` replaces subprocess.run's collect-and-wait.
    proc = subprocess.Popen(
        cmd,
        cwd=str(Path(__file__).resolve().parent.parent),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    if runtime is not None:
        runtime.register_child(proc)
    out_text = ""
    err_text = ""
    try:
        with suppress(Exception):  # a cancelled child may break communicate()
            out_text, err_text = proc.communicate()
    finally:
        if runtime is not None:
            runtime.unregister_child(proc)
    if proc.returncode != 0:
        from axiom_ng_runner.compute_core.pdf_worker.__main__ import (
            _classify_child_failure,
        )
        klass = _classify_child_failure(proc.returncode)
        if not klass and proc.returncode == 1 and content_type == "application/epub+zip":
            # #237: the EPUB worker classified a DOCUMENT defect (corrupt
            # zip/OPF, pandoc parse failure, DRM) — exit 1 with a JSON error
            # on stderr. That is a structured terminal FAIL (repair track),
            # not an infrastructure retry storm. EPUB-only by design: the
            # pdf_worker's catch-all exit 1 also covers infra faults (model
            # load, disk) — the PDF side keeps the preflight/repair gate and
            # this retryable path. Exit 2/3 and signals stay infra.
            # NOTE: worker exit codes don't separate doc-vs-infra yet;
            # dedicated exit codes when misroutes actually show up
            raise SourceError(
                "SOURCE_UNREADABLE",
                f"{convert} failed: {err_text[-300:]}",
            )
        detail = (klass + " | ") if klass else ""
        raise RuntimeError(f"{convert} failed: {detail}{err_text[-500:]}")

    # Parse the pdf_worker JSON result for the image_mapping
    # ({original_marker_filename → saved_filename}).
    image_mapping: dict[str, str] = {}
    for line in reversed((out_text or "").splitlines()):
        line = line.strip()
        if line.startswith("{") and line.endswith("}"):
            try:
                wresult = _json.loads(line)
                image_mapping = wresult.get("image_mapping") or {}
            except _json.JSONDecodeError:
                continue
            break

    markdown = out_md.read_text(encoding="utf-8")
    page_label_map: dict[int, str] = {}
    page_source_map: dict[int, str] = {}
    page_chapter_map: dict[int, int] = {}
    # #226 W2: refusable tail-trimmed marker-page ceiling; None for PDFs and
    # untrimmed EPUBS (declared here so it is unconditionally bound).
    marker_pagemap_max: int | None = None
    cfi_entries: list[dict[str, Any]] = []
    if content_type == "application/pdf":
        # #173: the trust pipeline replaces the bare 3-tier extract — labels
        # AND per-page trust levels (folio_verified / pdf_label_sane /
        # physical_only) in one pass. Never guess: a page reference without
        # its level never leaves the runner.
        from axiom_ng_runner.compute_core.page_trust import build_page_trust

        page_label_map, page_source_map, page_chapter_map = build_page_trust(str(source_path))
    elif content_type == "application/epub+zip":
        # EPUB CFI extraction (§11 Weg A): build text→CFI mapping from the
        # original XHTML DOM so chunks carry real epub_cfi locators instead
        # of fabricated page_span labels.
        from .epub_cfi import build_cfi_map

        cfi_entries = build_cfi_map(str(source_path))
        # #220 citation parity: publisher print-page anchors → entry pages.
        # Monotone maps only (never guess); a non-monotone map is logged
        # and dropped — locators stay chapter+CFI with page_source=none.
        from axiom_ng_runner.compute_core import epub_pagelist

        pagemap = epub_pagelist.parse_page_map(str(source_path))
        # #226 W2: when the map was tail-trimmed (appendix/index numbering
        # restart), the worker still emits {N} markers for the restarted
        # tail — a DIFFERENT numbering space. Gate the marker-bounds
        # override on the trusted map's max page so spaces never mix.
        marker_pagemap_max = (
            pagemap["anchors"][-1]["page"]
            if pagemap.get("restart_tail_trimmed") and pagemap["anchors"]
            else None
        )
        # #226 provenance: the #222 injector declares itself in the OPF
        # (the anchor shape mimics native format — shape is not evidence).
        from axiom_ng_runner.compute_core import page_trust as pt

        derived = epub_pagelist.detect_derived_from_sibling(str(source_path))
        page_level: str | None = None  # None = refuse enrichment entirely
        if pagemap["anchors"] and pagemap["monotone"]:
            sanity = epub_pagelist.sanity_check(pagemap["anchors"])
            toc = epub_pagelist.verify_print_folios(
                str(source_path), pagemap["anchors"]
            )
            if not sanity["ok"]:
                log.warning("epub_pagelist: implausible map (%s) — refusing "
                            "enrichment (page_source stays none)",
                            "; ".join(sanity["reasons"]))
            elif toc["verdict"] == "divergent" and not derived:
                # #223: the book's own TOC contradicts the markers (offset
                # %s) — likely reader pagination, NOT print folios. Refuse.
                log.warning("epub_pagelist: printed TOC diverges from markers "
                            "(%d/%d joins, offset %s) — refusing enrichment",
                            toc["joins"], toc["matched"], toc["offset"])
            else:
                # #226 trust ordering: TOC proof > declared sibling
                # derivation > no proof. A declared derived map overrides a
                # TOC divergence (the derivation was round-trip-verified
                # against the PDF sibling's folios; a constant TOC offset
                # is a join artifact at chapter starts, not marker error).
                if toc["verdict"] == "verified":
                    page_level = pt.PRINT_VERIFIED
                elif derived:
                    page_level = pt.DERIVED_FROM_SIBLING
                    if toc["verdict"] == "divergent":
                        log.info("epub_pagelist: TOC diverges (offset %s) but "
                                 "the sibling declaration wins: derived map, "
                                 "round-trip-verified", toc["offset"])
                else:
                    page_level = pt.PRINT_UNVERIFIED
                annotated = epub_pagelist.annotate_cfi_entries(
                    cfi_entries, pagemap["anchors"]
                )
                log.info("epub_pagelist: %d anchors (%s), TOC verdict %s, "
                         "%d/%d entries annotated, page_source=%s",
                         pagemap["count"], ",".join(pagemap["dialects"]),
                         toc["verdict"], annotated, len(cfi_entries), page_level)
                if page_level is not None:
                    for e in cfi_entries:
                        if e.get("page") is not None:
                            e["page_trust"] = page_level
        elif pagemap["anchors"]:
            log.warning("epub_pagelist: %d anchors NOT monotone — refusing "
                        "enrichment (page_source stays none)", pagemap["count"])
        # Rewrite machine-specific image paths to stable basenames BEFORE
        # chunking (#124): the epub worker's temp dir carries a random
        # suffix that would otherwise land verbatim in chunk texts and
        # break byte-determinism across runs. Covers both markdown image
        # links and raw HTML <img src> attributes (pandoc emits both).
        markdown = _normalize_epub_image_paths(markdown)
        out_md.write_text(markdown, encoding="utf-8")

    enter("chunk")
    from axiom_ng_runner.compute_core.chunker import Chunker

    chunker = Chunker(max_chunk_tokens=1200)
    chunk_dicts = chunker.chunk(
        markdown,
        doc_metadata={"doc_id": request["job_id"], "page_label_map": page_label_map,
                     "page_chapter_map": page_chapter_map},
    )

    # #245 APA-7: freeze chapter/section/paragraph citation fields at
    # ingest from the chunker's heading structure (deterministic pandoc
    # DOM output — stable like the CFI itself; see _freeze_apa_fields).
    if content_type == "application/epub+zip":
        chapters = chunker.chapter_starts()
        for c in chunk_dicts:
            _freeze_apa_fields(c.setdefault("metadata", {}), chapters)

    # Dense embeddings via the existing heavy core if requested.
    proc_opt = request.get("processing", {}) or {}
    if proc_opt.get("compute_dense_embeddings"):
        enter("embed")
        from axiom_ng_runner.compute_core.embedder import TextEmbedder

        TextEmbedder().embed_chunks(chunk_dicts)

    # Collect Marker images and declare them as Contract artifacts (§13).
    # Build a ref-mapping: original_marker_name → image-XXXX (Contract-konform).
    # Both _adapt_chunk's _normalize_image_refs and the artifact declarations use
    # this mapping so chunk.image_refs ⊆ {artifact.ref} (round-trip consistency).
    image_artifacts, orig_to_ref = _collect_image_artifacts(out_images, image_mapping, work_dir)

    # Apply the ref-mapping to chunk image_refs so they carry the Contract refs.
    # Use basename for the lookup key: the Chunker captures the full markdown
    # path (e.g. 'media/cover.png'), but image_mapping keys are basenames.
    for c in chunk_dicts:
        meta = c.get("metadata", {}) or {}
        raw_refs = _drop_link_refs(meta.get("image_refs", []) or [])
        normalized: list[str] = []
        for r in raw_refs:
            if isinstance(r, dict):
                orig = str(r.get("path", r.get("alt_text", "")))
            else:
                orig = str(r)
            # Basename lookup (C1 fix): matches the old processor's
            # Path(old_path).name defense.
            orig_base = Path(orig).name if orig else orig
            normalized.append(orig_to_ref.get(orig_base, orig_to_ref.get(orig, orig)))
        meta["image_refs"] = normalized

    # EPUB CFI: override page_span locators with epub_cfi for EPUB sources.
    # The real Chunker emits page_span with fabricated page labels (it doesn't
    # know about EPUB structure). We replace them with real CFI locators from
    # the original XHTML DOM (§11 Weg A).
    _enrich_epub_cfi_locators(chunk_dicts, cfi_entries, marker_pagemap_max)

    # L6: real entity (GLiNER) and relationship (mREBEL) extraction.
    # Contract chunk refs are deterministic (chunk-{i:04d} from enumerate),
    # so extraction can run before _build_reference_result assigns them.
    real_entities: list[dict[str, Any]] | None = None
    real_relationships: list[dict[str, Any]] | None = None
    chunk_items = [(f"chunk-{i:04d}", c.get("text", ""))
                   for i, c in enumerate(chunk_dicts)]
    if proc_opt.get("extract_entities") or proc_opt.get("extract_relationships"):
        enter("entities")
        real_entities = _extract_real_entities(
            chunk_items,
            on_progress=(lambda d, t: set_progress(d, t, "chunks"))
            if set_progress is not None else None,
        )
    # #225 early-commit: everything except relationships is done — build
    # and commit the result NOW so a late-stage abort (budget, hang, crash)
    # cannot discard chunks/embeddings/entities. The relationships stage
    # then patches the same dict; the caller's final set_result overwrites
    # the stored snapshot. stage_completion documents what is present.
    want_relationships = bool(
        proc_opt.get("extract_relationships") and real_entities is not None
    )
    stage_completion = {
        "chunks": True,
        "embeddings": bool(proc_opt.get("compute_dense_embeddings")),
        "entities": real_entities is not None,
        "relationships": False,
        "relationships_reason": None,
    }

    def _finish(rels: list[dict[str, Any]] | None) -> dict[str, Any]:
        result = _build_reference_result(
            request=request,
            work_dir=work_dir,
            chunk_dicts=chunk_dicts or [],
            page_label_map=page_label_map,
            markdown_path=out_md,
            attachment_id=attach["attachment_id"],
            content_hash="sha256:" + _sha256_hex(source_path),
            source_page_count=len(page_label_map) if page_label_map else 0,
            image_artifacts=image_artifacts,
            real_entities=real_entities,
            real_relationships=rels,
            stage_timings=stage_timings,
            page_source_map=page_source_map,
            page_chapter_map=page_chapter_map,
        )
        if "stage_completion" in (result.get("manifest") or {}):
            result["manifest"]["stage_completion"] = stage_completion
        else:
            result.setdefault("manifest", {})["stage_completion"] = stage_completion
        return result

    # [] (not None) suppresses _build_reference_result's heuristic fallback
    # relationships — the real backend never presents reference stubs, and
    # a crash-retrieved partial must not carry them either.
    result = _finish([])
    if commit is not None:
        commit(result)

    if want_relationships:
        # mREBEL reads metadata['chunk_id'] as evidence — set the contract
        # ref so evidence_chunk_refs resolve without a second mapping.
        enter("relationships")
        _assign_contract_chunk_ids(chunk_dicts)
        chunk_texts = dict(chunk_items)
        # want_relationships implies real_entities is not None (the guard above
        # required it); a bare ``if real_entities is None`` would violate the
        # invariant, so narrow explicitly for the extractor's non-Optional arg.
        assert real_entities is not None
        budget = settings.get().relationships_budget_seconds
        deadline = (time.monotonic() + budget) if budget > 0 else None
        real_relationships, budget_exceeded = _extract_real_relationships(
            real_entities, chunk_dicts, chunk_texts,
            deadline=deadline,
            on_progress=(lambda d, t: set_progress(d, t, "chunks"))
            if set_progress is not None else None,
        )
        if budget_exceeded:
            # #225: honest partial completion, not an eternal lease — the
            # committed result stays fetchable; force_rebuild (§19) reruns
            # the whole job (the runner holds no corpus state to resume
            # from; the early commit protects retrievability, not resume).
            stage_completion["relationships_reason"] = "STAGE_BUDGET_EXCEEDED"
            log.warning("relationships stage budget (%ss) exceeded (%d chunks) — "
                        "completing without relationships",
                        budget, len(chunk_dicts))
        stage_completion["relationships"] = not budget_exceeded
        result = _finish(real_relationships)

    enter("assemble")
    return result


def _collect_image_artifacts(
    images_dir: Path, image_mapping: dict[str, str], work_dir: Path
) -> tuple[list[dict[str, Any]], dict[str, str]]:
    """Collect Marker images and build Contract artifact declarations.

    Returns (artifacts, orig_to_ref) where:
    - artifacts: list of §13-conformant artifact dicts (ref/kind/media_type/
      sha256/size_bytes/retention) for each image
    - orig_to_ref: mapping {original_marker_filename → image-XXXX ref} used
      to normalize chunk.image_refs for round-trip consistency.

    Images are copied to work_dir/artifacts/<ref> so the artifact endpoint can
    serve them for Go to fetch+verify+commit.
    """
    if not image_mapping:
        return [], {}

    artifacts: list[dict[str, Any]] = []
    orig_to_ref: dict[str, str] = {}
    art_dir = work_dir / "artifacts"
    art_dir.mkdir(parents=True, exist_ok=True)

    ext_to_media: dict[str, str] = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
    }

    for idx, (orig_name, saved_name) in enumerate(sorted(image_mapping.items())):
        img_path = images_dir / saved_name
        if not img_path.exists():
            log.warning("image not found: %s", img_path)
            continue
        ext = img_path.suffix.lower()
        ref = f"image-{idx:04d}"

        # Copy to artifacts dir under the Contract ref name.
        dest = art_dir / ref
        try:
            dest.write_bytes(img_path.read_bytes())
        except OSError as err:
            log.warning("failed to copy image %s: %s", img_path, err)
            continue

        # W1 fix: assign orig_to_ref ONLY after successful copy + artifact
        # declaration, so a failed image is dropped from both (no dangling ref).
        orig_to_ref[orig_name] = ref
        sha = _sha256_hex(dest)
        size = dest.stat().st_size
        artifacts.append({
            "ref": ref,
            "kind": "extracted_image",
            "media_type": ext_to_media.get(ext, "application/octet-stream"),
            "sha256": sha,
            "size_bytes": size,
            "retention": "durable_if_referenced",
        })

    return artifacts, orig_to_ref
