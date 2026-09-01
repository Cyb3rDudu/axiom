"""#233 — locator backfill: enrich existing chunks' folios from a derived EPUB.

The folio is metadata on the chunk, not the chunk itself. This module builds
an ALIGNMENT between an active snapshot's chunks and an enriched EPUB sibling
(an injected page map, declared ``derived_from_sibling`` by the injector), so
the chunks' print-page locators can be upgraded in place — no conversion, no
re-chunking, no new snapshot, no re-embedding.

Trust semantics (#222/#226/#233): backfilled folios are ALWAYS
``derived_from_sibling`` — never ``print_verified``. A chunk whose alignment
falls below the confidence threshold keeps ``none`` (refuse, never guess).
Chapter numbers/titles remain authoritative from the active snapshot; only
pages are added.

Two alignment paths, selected by the active snapshot's source kind:

* PDF   (source_kind="pdf")    — four-point text-window alignment (#221): hard
  anchors fix a monotone physical↔print map, print pages interpolate between.
* EPUB  (source_kind="epub")   — CFI-based: the candidate's page map is carried
  onto its CFI entries and each chunk's text is matched to a CFI entry to read
  the print page.

Both paths refuse the WHOLE backfill on a non-monotone page map (#226) and
refuse per-chunk when confidence is too low. Idempotent by construction:
re-running on the same input yields the same result.
"""
from __future__ import annotations

import logging
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

# pi-lens-ignore: reportMissingImports
from axiom_ng_runner.compute_core import epub_pagelist
from axiom_ng_runner.epub_cfi import build_cfi_map
from axiom_ng_runner.scripts.four_point_pilot import (
    BOILERPLATE,
    containment,
    harvest_epub,
    norm_tokens,
)

logger = logging.getLogger(__name__)

# A marker must match a PDF physical page at least this well to become a hard
# anchor (mirrors four_point_pilot._SIM_OK).
SIM_ANCHOR = 0.4
# Fewer hard anchors than this and the whole backfill refuses (#226 discipline).
MIN_ANCHORS = 4
# Enrichment trust is always derived_from_sibling.
SOURCE = "derived_from_sibling"

# Confidence blend weights: anchor text-similarity + chunk text-overlap +
# anchor spread (distance between the bracketing anchors).
_WEIGHT_ANCHOR = 0.5
_WEIGHT_TEXT = 0.3
_WEIGHT_SPREAD = 0.2
# A chunk below this blended confidence keeps `none`.
MIN_CONFIDENCE = 0.4
# Below this raw text-overlap there is no evidence the chunk text belongs to
# the predicted page — refuse regardless of the anchor score.
MIN_TEXT_OVERLAP = 0.05


# ---------------------------------------------------------------------------
# result structures
# ---------------------------------------------------------------------------

@dataclass
class ChunkAlignment:
    """Per-chunk alignment outcome.

    ``enrich`` is True only when the chunk currently carries no derived-or-
    higher folio (checked by the caller via ``wants_enrichment``) and the
    alignment is confident. ``refused`` documents the refusal reason.
    """
    chunk_id: str
    enrich: bool = False
    page_start: int | None = None
    page_end: int | None = None
    source: str = SOURCE
    confidence: float = 0.0
    refused: bool = False
    reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class BackfillResult:
    """Outcome for one document: per-chunk alignments + an honest gate."""
    chunk_results: list[ChunkAlignment] = field(default_factory=list)
    aligned: bool = True              # False => whole backfill refused
    refused_reason: str = ""          # when aligned is False
    enrichment_targets: int = 0
    pages_enriched: int = 0
    pages_refused: int = 0
    anchor_count: int = 0

    def summary(self) -> dict[str, Any]:
        return {
            "aligned": self.aligned,
            "refused_reason": self.refused_reason,
            "enrichment_targets": self.enrichment_targets,
            "pages_enriched": self.pages_enriched,
            "pages_refused": self.pages_refused,
            "anchor_count": self.anchor_count,
        }


# ---------------------------------------------------------------------------
# input contract
# ---------------------------------------------------------------------------

def is_aligned_chunk(chunk: dict[str, Any]) -> bool:
    """A chunk is a backfill candidate only when it carries no print-page
    trust of its own. Derived-from-sibling and stronger (folio_verified,
    print_verified, pdf_label_sane) are left untouched — backfill never
    downgrades or re-stamps a chunk that already has a folio."""
    src = (chunk.get("locator") or {}).get("page_source") or "none"
    return src in ("none", "physical_only", "blind", "")


def _chunk_after_target(chunk: dict[str, Any], page_start: int | None,
                        page_end: int | None) -> ChunkAlignment:
    """A chunk that already carries a folio (or is not a target) is reported
    as aligned (no change), NOT enriched."""
    return ChunkAlignment(
        chunk_id=chunk["id"],
        enrich=False,
        page_start=page_start,
        page_end=page_end,
        confidence=1.0,
        refused=False,
        reason="unchanged: already has a folio or not a backfill target",
    )


def _chunk_refused(chunk: dict[str, Any], reason: str) -> ChunkAlignment:
    return ChunkAlignment(
        chunk_id=chunk["id"], refused=True, reason=reason, confidence=0.0,
    )


def _physical(chunk: dict[str, Any], key: str) -> int | None:
    v = (chunk.get("locator") or {}).get(key)
    if v is None:
        v = chunk.get(key)  # tolerate top-level physical fields
    if isinstance(v, int):
        return v
    try:
        return int(v) if v is not None else None
    except (TypeError, ValueError):
        return None


# ---------------------------------------------------------------------------
# PDF path — four-point text-window alignment
# ---------------------------------------------------------------------------

def _pdf_physical_tokens(pdf_path: str) -> list[list[str]]:
    """Normalized per-physical-page tokens, 0-BASED index (tokens[p] is
    pymupdf page p) — the same base the stored locator's
    physical_page_start uses (contract §11; search renders it +1)."""
    import pymupdf

    doc = pymupdf.open(pdf_path)
    try:
        texts = [doc[i].get_text("text") for i in range(doc.page_count)]
        return [norm_tokens(str(t), 1200) for t in texts]
    finally:
        doc.close()


def build_pdf_anchors(candidate_epub: str, phys_tokens: list[list[str]],
                      stop: set[str]) -> list[tuple[int, int, float]]:
    """Hard anchors ``(physical_page_0based, print_page, similarity)``.

    Harvest the EPUB's page markers, match each ARABIC marker's text window
    against the PDF physical pages by containment, and keep a monotone
    non-decreasing physical sequence (dropping the rare marker that regresses
    — the #221 folio-assigner tolerance). Returns [] when the gate fails:
    too few strong anchors survive, which the caller reports as a whole-
    backfill refusal. The page-map monotonicity gate itself (arabic maps;
    a restarted index tail is trimmed by epub_pagelist) is enforced once by
    the caller via parse_page_map before this runs (#226)."""
    markers, _ = harvest_epub(Path(candidate_epub), stop)
    arabic = [m for m in markers if m["marker_page"] > 0]
    if not arabic:
        return []
    anchors: list[tuple[int, int, float]] = []
    prev_phys = -1
    for m in sorted(arabic, key=lambda x: x["marker_page"]):
        best_s, best_p = 0.0, None
        window = m["window"]
        for p, tokens in enumerate(phys_tokens):
            s = containment(window, tokens, stop)
            if s > best_s:
                best_s, best_p = s, p
        if best_p is None or best_s < SIM_ANCHOR:
            continue
        ph = best_p  # 0-based, matching the stored locator convention
        if ph < prev_phys:
            # a stray regression (marker text matching an earlier page) is
            # dropped, not fatal — same tolerance the #221 assigner uses.
            continue
        anchors.append((ph, m["marker_page"], best_s))
        prev_phys = ph
    if len(anchors) < MIN_ANCHORS:
        return []
    return anchors


def _interpolate(anchors: list[tuple[int, int, float]], phys: int
                 ) -> tuple[int | None, float, float]:
    """Linear interpolation of print page at a 0-based physical page.

    Returns (print_page, confidence, spread). ``spread`` is the physical
    distance between the bracketing anchors (0 when not bracketed)."""
    prev = None
    for a in anchors:
        if a[0] <= phys:
            prev = a
        else:
            nxt = a
            if prev is None:
                # before the first anchor: NO bracketing evidence — refuse
                # rather than extrapolate (#226 discipline)
                return None, 0.0, 0.0
            gap = nxt[0] - prev[0]
            if gap == 0:
                pr = prev[1]
            else:
                pr = round(prev[1] + (phys - prev[0]) * (nxt[1] - prev[1]) / gap)
            spread = 1.0 - (gap - 1) / 100.0
            spread = max(0.0, min(1.0, spread))
            conf = min(prev[2], nxt[2])
            return pr, conf, spread
    # past the last anchor: NO bracketing evidence — refuse (see above)
    return None, 0.0, 0.0


def align_pdf_chunks(candidate_epub: str, pdf_path: str,
                     chunks: list[dict[str, Any]]) -> BackfillResult:
    """PDF-active path: physical_page_start -> derived print page via anchors."""
    result = BackfillResult()
    # #226 monotonicity gate: the candidate's own page map must be monotone
    # (arabic; a restarted index tail is trimmed by parse_page_map). A
    # non-monotone map refuses the whole backfill before any anchoring.
    pm = epub_pagelist.parse_page_map(candidate_epub)
    if not pm["monotone"] or pm["count"] < MIN_ANCHORS:
        result.aligned = False
        result.refused_reason = (
            "non-monotone or too few page anchors in candidate EPUB "
            "(#226 refuse)"
        )
        logger.warning("locator_backfill: refused PDF alignment (page map %s)",
                       "non-monotone" if not pm["monotone"] else "too few anchors")
        return result
    stop = set(norm_tokens(BOILERPLATE))
    phys_tokens = _pdf_physical_tokens(pdf_path)
    anchors = build_pdf_anchors(candidate_epub, phys_tokens, stop)
    result.anchor_count = len(anchors)
    if not anchors:
        result.aligned = False
        result.refused_reason = (
            "too few strong text anchors for PDF alignment (#226 refuse)"
        )
        logger.warning("locator_backfill: refused PDF alignment (no anchors)")
        return result

    for chunk in chunks:
        if not is_aligned_chunk(chunk):
            result.chunk_results.append(_chunk_after_target(
                chunk,
                _physical(chunk, "physical_page_start"),
                _physical(chunk, "physical_page_end"),
            ))
            continue
        result.enrichment_targets += 1
        pstart = _physical(chunk, "physical_page_start")
        pend = _physical(chunk, "physical_page_end")
        if pstart is None or pstart < 0:
            result.chunk_results.append(_chunk_refused(chunk, "no physical_page_start"))
            result.pages_refused += 1
            continue
        pr_s, conf_s, spread_s = _interpolate(anchors, pstart)
        # a single-page or open-ended chunk interpolates its end at the
        # start position (end == start); guard None for safety
        pend_phys = pend if pend is not None and pend >= pstart else pstart
        pr_e, conf_e, spread_e = _interpolate(anchors, pend_phys)
        if pr_s is None:
            result.chunk_results.append(
                _chunk_refused(chunk, "physical page outside anchored range"))
            result.pages_refused += 1
            continue
        page_end = max(pr_e, pr_s) if pr_e is not None else pr_s
        # text-overlap cross-check: the chunk's own text must actually appear
        # on the predicted physical page (never guess against the evidence).
        chunk_tokens = norm_tokens(chunk.get("text", ""), 500)
        if 0 <= pstart < len(phys_tokens):
            text_conf = containment(chunk_tokens, phys_tokens[pstart], stop)
        else:
            text_conf = 0.0
        anchor_conf = 0.5 * conf_s + 0.5 * conf_e
        spread_conf = 0.5 * spread_s + 0.5 * spread_e
        confidence = (anchor_conf * _WEIGHT_ANCHOR
                      + text_conf * _WEIGHT_TEXT
                      + spread_conf * _WEIGHT_SPREAD)
        ca = ChunkAlignment(
            chunk_id=chunk["id"],
            page_start=pr_s,
            page_end=page_end,
            confidence=round(confidence, 3),
        )
        if confidence < MIN_CONFIDENCE or text_conf < MIN_TEXT_OVERLAP:
            ca.refused = True
            ca.reason = (
                f"low confidence ({confidence:.2f}, text_overlap {text_conf:.2f})"
            )
            result.pages_refused += 1
        else:
            ca.enrich = True
            result.pages_enriched += 1
        result.chunk_results.append(ca)
    return result


# ---------------------------------------------------------------------------
# EPUB path — CFI-based
# ---------------------------------------------------------------------------

def align_epub_chunks(candidate_epub: str,
                      chunks: list[dict[str, Any]]) -> BackfillResult:
    """EPUB-active path: match each chunk's text to the candidate's annotated
    CFI entries and read the print page carried there.

    The candidate's own page map must be monotone and carry enough anchors,
    else the whole backfill refuses (#226)."""
    result = BackfillResult()
    pm = epub_pagelist.parse_page_map(candidate_epub)
    if not pm["monotone"] or pm["count"] < MIN_ANCHORS:
        result.aligned = False
        result.refused_reason = "non-monotone or too few page anchors (#226 refuse)"
        logger.warning("locator_backfill: refused EPUB alignment (page map %s)",
                       "non-monotone" if not pm["monotone"] else "too few anchors")
        return result
    cfi_entries = build_cfi_map(candidate_epub)
    epub_pagelist.annotate_cfi_entries(cfi_entries, pm["anchors"])
    result.anchor_count = pm["count"]

    from axiom_ng_runner.epub_cfi import _normalize_text

    # index entries by normalized text prefix for lookup
    for chunk in chunks:
        if not is_aligned_chunk(chunk):
            result.chunk_results.append(_chunk_after_target(chunk, None, None))
            continue
        result.enrichment_targets += 1
        clean = _normalize_text(chunk.get("text", ""))
        # find the first cfi entry whose text matches the chunk start
        matched = None
        for e in cfi_entries:
            if e.get("page") is None:
                continue
            etext = _normalize_text(e["text"])
            # C3 guard (as in match_text_to_cfi): ultra-short entries (a
            # bare page number) substring-match everywhere — poison.
            if len(etext) < 12:
                continue
            if etext[:40] in clean:
                matched = e
                break
        if matched is None or matched.get("page") is None:
            result.chunk_results.append(
                _chunk_refused(chunk, "chunk text not found in candidate CFI map"))
            result.pages_refused += 1
            continue
        ca = ChunkAlignment(
            chunk_id=chunk["id"],
            page_start=matched["page"],
            page_end=matched["page"],
            confidence=0.9,
            enrich=True,
        )
        result.pages_enriched += 1
        result.chunk_results.append(ca)
    return result


# ---------------------------------------------------------------------------
# dispatcher
# ---------------------------------------------------------------------------

def backfill_chunks(candidate_epub: str, source_kind: str,
                    pdf_path: str | None,
                    chunks: list[dict[str, Any]]) -> BackfillResult:
    """Run the right alignment path for the active snapshot's source kind."""
    if source_kind == "pdf":
        if not pdf_path:
            res = BackfillResult(aligned=False,
                                 refused_reason="no pdf_path for pdf snapshot")
            return res
        return align_pdf_chunks(candidate_epub, pdf_path, chunks)
    return align_epub_chunks(candidate_epub, chunks)
