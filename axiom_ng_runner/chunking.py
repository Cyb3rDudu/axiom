"""Pure, hermetic chunking for the *reference* compute backend.

The real backend's compute_core still pulls GPU/store dependencies
(numpy/pgvector/torch), so a truly hermetic reference backend cannot
safely import them. This module re-implements the *small* pure bit we
need (deterministic struct-aware chunking + PDF page label extraction)
with stdlib + PyMuPDF only, so the reference backend and the contract
black-box suite stay dependency-light. The ``real`` backend wraps the
genuine compute_core.
"""

from __future__ import annotations

import re

# Marker page marker pattern used across the pipeline: "{0}-----".
_PAGE_MARKER_RE = re.compile(r"^\{(\d+)\}-{10,}$")
_HEADING_RE = re.compile(r"^(#{1,6})\s+(.+)$", re.MULTILINE)
_PARAGRAPH_SPLIT = re.compile(r"(\n\s*\n+)")


def extract_page_labels(pdf_path: str) -> dict[int, str]:
    """Logical PDF page labels with a 2-tier fallback (contract §11).

    Tier 1: PDF embedded page labels (publisher metadata).
    Tier 2: physical page index + 1 (as string).
    Returns a mapping {physical_index(0-based): label_string}.
    """
    import pymupdf

    doc = pymupdf.open(str(pdf_path))
    labels: dict[int, str] = {}
    n = doc.page_count
    for i in range(n):
        label = doc[i].get_label()
        if label and label.strip():
            labels[i] = label.strip()
    doc.close()
    if labels:
        return labels
    return {i: str(i + 1) for i in range(n)}


def _split_paragraphs(markdown: str) -> list[str]:
    parts = _PARAGRAPH_SPLIT.split(markdown)
    out: list[str] = []
    for part in parts:
        if part and not _PARAGRAPH_SPLIT.match(part):
            out.append(part.strip())
    return out


def _headings_map(paragraphs: list[str]) -> dict[int, list[str]]:
    mapping: dict[int, list[str]] = {}
    stack: list[str] = []
    for i, para in enumerate(paragraphs):
        first = para.strip().split("\n")[0]
        m = _HEADING_RE.match(first)
        if m:
            level = len(m.group(1))
            title = m.group(2).strip()
            stack = stack[: level - 1] + [title]
        mapping[i] = list(stack)
    return mapping


def chunk_markdown(
    markdown: str, page_label_map: dict[int, str], max_tokens: int = 512
) -> list[dict[str, object]]:
    """Deterministic struct-aware chunker producing contract-shaped chunks.

    Each output dict is the contract chunk shape (contract §11), including a
    page_span locator reconstructed from Marker-style page markers and the
    logical page label map.
    """
    paragraphs = _split_paragraphs(markdown)
    if not paragraphs:
        return []

    # Assign a logical page label to each paragraph from Marker markers.
    para_page_label: list[str] = []
    current_label = "1"
    clean_paras: list[str] = []
    had_page_markers = False
    for para in paragraphs:
        pm = _PAGE_MARKER_RE.match(para.strip())
        if pm:
            had_page_markers = True
            try:
                physical = int(pm.group(1))
            except (TypeError, ValueError):
                continue
            current_label = page_label_map.get(physical, str(physical + 1))
            continue
        para_page_label.append(current_label)
        clean_paras.append(para)
    if not clean_paras:
        return []

    headings = _headings_map(clean_paras)

    # Group paragraphs into chunks by token budget, reopening on headings.
    tokens_per_para = [_count_tokens(p) for p in clean_paras]
    chunks: list[dict[str, object]] = []
    current: list[int] = []  # indices into clean_paras
    acc = 0
    for i in range(len(clean_paras)):
        is_heading = bool(_HEADING_RE.match(clean_paras[i].strip().split("\n")[0]))
        if current and (is_heading or (acc + tokens_per_para[i] > max_tokens)):
            chunks.append(
                _emit(
                    clean_paras,
                    current,
                    para_page_label,
                    headings,
                    page_label_map,
                    had_page_markers,
                    len(chunks),
                )
            )
            acc = 0
            current = []
        current.append(i)
        acc += tokens_per_para[i]
    if current:
        chunks.append(
            _emit(
                clean_paras,
                current,
                para_page_label,
                headings,
                page_label_map,
                had_page_markers,
                len(chunks),
            )
        )
    return chunks


def _count_tokens(text: str) -> int:
    # Word-count * 1.3 approximation, matching the existing chunker's cost model.
    try:
        return int(len(text.split()) * 1.3)
    except (TypeError, ValueError):
        return 0


def _physical(label: str, page_label_map: dict[int, str]) -> int | None:
    reverse: dict[str, int] = {}
    for phys, lbl in page_label_map.items():
        reverse.setdefault(lbl, phys)
    if label in reverse:
        return reverse[label]
    try:
        return int(label) - 1
    except (TypeError, ValueError):
        return None


def _emit(
    clean_paras: list[str],
    indices: list[int],
    para_page_label: list[str],
    headings: dict[int, list[str]],
    page_label_map: dict[int, str],
    had_page_markers: bool,
    chunk_index: int,
) -> dict[str, object]:
    text = "\n\n".join(clean_paras[i] for i in indices)
    page_start = para_page_label[indices[0]]
    page_end = para_page_label[indices[-1]]

    # Contract §11: page labels MUST NOT be fabricated for sources without
    # stable pages. Only emit a page_span when the source carried real Marker
    # page markers; otherwise use an honest epub_cfi locator.
    if had_page_markers:
        labels = sorted({para_page_label[i] for i in indices})
        physes = [
            p for p in (_physical(label, page_label_map) for label in labels) if p is not None
        ]
        locator: dict[str, object] = {
            "type": "page_span",
            "physical_page_start": min(physes) if physes else None,
            "physical_page_end": max(physes) if physes else None,
            "page_label_start": page_start,
            "page_label_end": page_end,
            "source": "marker_paginate",
        }
        # #194 per-paragraph page map: char-offset boundaries where the
        # chunk's print page changes (first entry always (0, first page)) —
        # consumers derive the exact page of a hit position instead of
        # citing the whole span envelope.
        # offsets as STRINGS: uniform [[str, str], ...] arrays keep the
        # OpenSearch dynamic mapping stable (mixed int/str arrays conflict:
        # "cannot be changed from type [long] to [text]" — live pilot lesson)
        bounds: list[list[str]] = [["0", str(page_start)]]
        off = 0
        prev = page_start
        for i in indices:
            if para_page_label[i] != prev:
                bounds.append([str(off), str(para_page_label[i])])
                prev = para_page_label[i]
            off += len(clean_paras[i]) + 2  # "\n\n" join
        locator["paragraph_pages"] = bounds
    else:
        locator = {
            "type": "epub_cfi",
            "cfi_start": "",
            "cfi_end": "",
            "source": "epub",
        }

    return {
        "ref": f"chunk-{chunk_index:04d}",
        "index": chunk_index,
        "text": text,
        "locator": locator,
        "structure": {
            "section_titles": headings.get(indices[0], []),
            "start_paragraph_index": indices[0],
            "end_paragraph_index": indices[-1],
        },
        "token_count": _count_tokens(text),
        "image_refs": [],
        "embeddings": {},
        "metadata": {},
    }
