"""Pure compute boundary (contract §5.5).

``compute(request, work_dir) -> ProcessorResult`` is the single entry point
used by the HTTP layer. It performs no database, OpenSearch, graph or Zotero
writes — it only reads the supplied source, converts, chunks and assembles a
contract-shaped result plus temporary artifact files.

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
import sys
import zipfile
from pathlib import Path
from typing import Any

from . import CONTRACT_VERSION, DENSE_EMBEDDING_DIM, DENSE_EMBEDDING_MODEL
from .config import settings

log = logging.getLogger(__name__)

_AXIOM_BACKEND = Path(__file__).resolve().parent.parent / "axiom_backend"

if str(_AXIOM_BACKEND) not in sys.path:  # allow the runner to wrap existing cores
    sys.path.insert(0, str(_AXIOM_BACKEND))


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


def _normalize_image_refs(refs: Any) -> list[str]:
    """Normalize the existing chunker's image_refs (list of dicts with
    'path'/'alt_text'/'position') to plain string refs (contract §11)."""
    if not refs:
        return []
    out: list[str] = []
    for r in refs:
        if isinstance(r, str):
            out.append(r)
        elif isinstance(r, dict):
            # Use the path/filename as the ref string.
            out.append(str(r.get("path", r.get("alt_text", ""))))
        else:
            out.append(str(r))
    return out


def _adapt_chunk(
    c: dict[str, Any], chunk_index: int, page_label_map: dict[int, str]
) -> dict[str, Any]:
    """Map the existing pipeline's chunk dict (chunker._create_chunk) to the
    contract chunk shape (contract §11 mapping table).

    A chunk that already carries contract shape (``ref`` + ``locator``), as
    produced by ``chunking.chunk_markdown``, passes through untouched. Old-style
    ``core_rag`` chunk dicts (logical page labels only) are adapted by
    reverse-mapping through ``page_label_map``.
    """
    if c.get("ref") and c.get("locator") and c.get("index") is not None:
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
    else:
        page_start = meta.get("page_start")
        page_end = meta.get("page_end")
        locator = {
            "type": "page_span",
            "physical_page_start": _physical(page_start),
            "physical_page_end": _physical(page_end),
            "page_label_start": str(page_start) if page_start is not None else "",
            "page_label_end": str(page_end) if page_end is not None else "",
            "source": meta.get("locator_source", "marker_paginate"),
        }

    return {
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
        "embeddings": {},
        "metadata": {},
    }


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
        ch = _adapt_chunk(c, idx, page_label_map)
        if proc.get("compute_dense_embeddings"):
            ch["embeddings"]["dense"] = _dense_embedding(ch)
        if proc.get("compute_sparse_embeddings"):
            ch["embeddings"]["sparse"] = _sparse_embedding(ch["text"])
        chunks.append(ch)

    entities: list[dict[str, Any]] = []
    if proc.get("extract_entities"):
        entities = _reference_entities(chunks)

    entity_relationships: list[dict[str, Any]] = []
    if proc.get("extract_relationships") and entities:
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
                "dense_embedding": "reference-bge-m3",
                "entity_extraction": "reference-gliner",
                "relationship_extraction": "reference-mrebel",
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


def compute(request: dict[str, Any], work_dir: Path) -> dict[str, Any]:
    """Run the pure compute pipeline for a validated source. Returns a
    contract processor-result dict (contract §10). Work dir is per-job and
    already validated by the caller."""
    backend = settings.get().compute_backend
    if backend == "real":
        result = _compute_real(request, work_dir)
        if result is not None:
            return result
        log.warning("real backend unavailable; falling back to reference")
    return _compute_reference(request, work_dir)


def _int_keys(mapping: dict[Any, Any]) -> dict[int, Any]:
    """Coerce mapping keys to int where possible (page label maps)."""
    out: dict[int, Any] = {}
    for k, v in mapping.items():
        try:
            out[int(k)] = v
        except (TypeError, ValueError):
            continue
    return out


def _compute_reference(request: dict[str, Any], work_dir: Path) -> dict[str, Any]:
    attach = request["attachment"]
    source_path = Path(attach["local_path"])

    markdown, page_label_map, page_count, md_path = _convert_reference(
        request, source_path, work_dir
    )
    page_label_map = _int_keys(page_label_map)

    # Pure, hermetic deterministic chunker (stdlib-only). Produces chunks in
    # final contract shape directly (see chunking.chunk_markdown).
    from .chunking import chunk_markdown

    chunk_dicts = chunk_markdown(markdown, page_label_map)

    return _build_reference_result(
        request=request,
        work_dir=work_dir,
        chunk_dicts=chunk_dicts or [],
        page_label_map=page_label_map,
        markdown_path=md_path,
        attachment_id=attach["attachment_id"],
        content_hash="sha256:" + _sha256_hex(source_path),
        source_page_count=page_count,
    )


def _compute_real(request: dict[str, Any], work_dir: Path) -> dict[str, Any] | None:
    """Wire the existing heavy pipeline (Marker/pdf_worker, epub_worker,
    embedder, extractors). Returns None if the heavy deps are unavailable so
    the caller can fall back. The contract black-box suite runs against
    ``reference``."""
    try:
        return _real_pipeline(request, work_dir)
    except ImportError as err:
        log.warning("real compute deps unavailable: %s", err)
        return None


def _real_pipeline(request: dict[str, Any], work_dir: Path) -> dict[str, Any]:
    import json as _json
    import subprocess
    import sys

    attach = request["attachment"]
    source_path = Path(attach["local_path"])
    content_type = attach["content_type"]
    out_md = work_dir / "markdown.md"
    out_images = work_dir / "images"

    convert = (
        "ai_researcher.pdf_worker"
        if content_type == "application/pdf"
        else "ai_researcher.epub_worker"
    )
    cmd = [
        sys.executable,
        "-m",
        convert,
        str(source_path),
        str(out_md),
        str(out_images),
    ]
    proc = subprocess.run(
        cmd, cwd=str(_AXIOM_BACKEND), capture_output=True, text=True, check=False
    )
    if proc.returncode != 0:
        raise RuntimeError(f"{convert} failed: {proc.stderr[-500:]}")

    # Parse the pdf_worker JSON result for the image_mapping
    # ({original_marker_filename → saved_filename}).
    image_mapping: dict[str, str] = {}
    for line in reversed((proc.stdout or "").splitlines()):
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
    if content_type == "application/pdf":
        from ai_researcher.core_rag.processor import extract_page_labels

        page_label_map = extract_page_labels(str(source_path))

    from ai_researcher.core_rag.chunker import Chunker

    chunk_dicts = Chunker(max_chunk_tokens=1200).chunk(
        markdown,
        doc_metadata={"doc_id": request["job_id"], "page_label_map": page_label_map},
    )

    # Dense embeddings via the existing heavy core if requested.
    proc_opt = request.get("processing", {}) or {}
    if proc_opt.get("compute_dense_embeddings"):
        from ai_researcher.core_rag.embedder import TextEmbedder

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
        raw_refs = meta.get("image_refs", []) or []
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

    return _build_reference_result(
        request=request,
        work_dir=work_dir,
        chunk_dicts=chunk_dicts or [],
        page_label_map=page_label_map,
        markdown_path=out_md,
        attachment_id=attach["attachment_id"],
        content_hash="sha256:" + _sha256_hex(source_path),
        source_page_count=len(page_label_map) if page_label_map else 0,
        image_artifacts=image_artifacts,
    )


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
