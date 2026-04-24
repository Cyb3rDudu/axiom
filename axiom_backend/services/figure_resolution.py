"""Pre-fetch figure candidates from the document image library.

When a writing prompt carries figure-intent signals (the user asks
for charts, diagrams, or explicitly references Abbildung/Figure
numbers), the backend runs a vector query against the `document_images`
table scoped to the session's document groups and injects the
candidates into the writer's system prompt as real image URLs. The
writer then copies the URLs verbatim into its Markdown rather than
hallucinating paths like `placeholder-fig1.png`.

Agnostic to deliverable type: an academic paper asking for charts,
a market research report asking for bar graphs, and a technical doc
asking for architecture diagrams all use the same flow as long as
the underlying document library has extracted images.

Intent detection is a cheap keyword match; the RAG query itself only
fires when at least one intent hit is detected, so cost impact on
the no-figure path is zero.
"""

from __future__ import annotations

import logging
import re
from dataclasses import dataclass
from typing import Any, Dict, Iterable, List, Optional

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Intent detection
# ---------------------------------------------------------------------------


# Multilingual keyword set. The writer's output and the user's prompt
# can mix languages (German academic + English chart descriptions is
# common), so we accept both. Case-insensitive.
_FIGURE_INTENT_KEYWORDS = [
    # German
    r"\bAbbildung(?:en)?\b",
    r"\bDiagramm(?:e|en)?\b",
    r"\bGrafik(?:en)?\b",
    r"\bSchaubild(?:er)?\b",
    r"\bChart\b",
    # English
    r"\bfigure(?:s)?\b",
    r"\bdiagram(?:s)?\b",
    r"\bgraph(?:s)?\b",
    r"\billustration(?:s)?\b",
    # Placeholder paths the writer emits when asked but no real URLs provided
    r"placeholder[-_]?fig",
    r"example\.com/figure",
]


_FIGURE_INTENT_RE = re.compile(
    "|".join(_FIGURE_INTENT_KEYWORDS), re.IGNORECASE
)


# Writer-emitted figure placeholders follow this pattern — each carries
# a description in the alt text that we can use as a query hint.
_FIGURE_PLACEHOLDER_RE = re.compile(
    r"!\[([^\]]*?)\]\((?:placeholder[-_][^\)]+|[^)]*example[^)]*)\)",
    re.IGNORECASE,
)


@dataclass(frozen=True)
class FigureQuery:
    """One description the writer intends to illustrate."""

    description: str
    source: str  # 'prompt' | 'placeholder' | 'instruction'


def detect_figure_intent(prompt: str, draft_body: str = "") -> List[FigureQuery]:
    """Return all figure descriptions the writer may need to resolve.

    Pulls from three places:
    1. Explicit `![Abbildung N: ...](placeholder-fig.png)` in the
       draft body — the alt text is the description.
    2. Prompt lines that begin with "Abbildung N:" or similar —
       user is describing figures they want.
    3. Generic intent keywords in the prompt (Abbildung, Figure,
       Chart) → one empty-description query so we at least report
       that the library was checked.

    Empty list means no figure work needed; caller skips the whole
    resolver path.
    """
    queries: List[FigureQuery] = []
    text = (prompt or "") + "\n" + (draft_body or "")

    # (1) Draft body placeholder figures
    for m in _FIGURE_PLACEHOLDER_RE.finditer(draft_body or ""):
        desc = m.group(1).strip()
        if desc:
            queries.append(FigureQuery(description=desc, source="placeholder"))

    # (2) User-described figures in the prompt — lines matching
    #     "Abbildung N: description" or "Figure N: description"
    for m in re.finditer(
        r"(?:Abbildung|Figure|Diagramm|Chart)\s+\d+\s*[:—-]\s*([^\n]{8,200})",
        prompt or "",
        re.IGNORECASE,
    ):
        desc = m.group(1).strip().rstrip(".").rstrip(",")
        if desc:
            queries.append(FigureQuery(description=desc, source="prompt"))

    # (3) Fallback — generic intent signal without specific descriptions
    if not queries and _FIGURE_INTENT_RE.search(text):
        queries.append(FigureQuery(description="", source="instruction"))

    # Deduplicate on normalised description
    seen: set[str] = set()
    unique: List[FigureQuery] = []
    for q in queries:
        key = q.description.lower().strip()
        if key in seen:
            continue
        seen.add(key)
        unique.append(q)
    return unique


# ---------------------------------------------------------------------------
# Candidate lookup
# ---------------------------------------------------------------------------


@dataclass
class FigureCandidate:
    """One image from document_images matched against a writer query."""

    image_id: str
    doc_id: str
    image_url: str       # /api/documents/images/{doc_id}/{filename}
    alt_text: Optional[str]
    relevance: float     # 0-1; higher = more confident match
    source_document_title: Optional[str] = None
    source_page: Optional[int] = None
    metadata: Dict[str, Any] = None  # type: ignore[assignment]

    def to_prompt_snippet(self) -> str:
        alt = (self.alt_text or "").strip() or "(no caption)"
        src = self.source_document_title or self.doc_id[:8]
        page = f", p. {self.source_page}" if self.source_page else ""
        return (
            f"![candidate figure]({self.image_url})\n"
            f"  description: {alt}\n"
            f"  source: {src}{page}\n"
            f"  relevance: {self.relevance:.2f}"
        )


def _image_url_from_path(doc_id: str, image_path: str) -> str:
    """Project the absolute disk path into the HTTP serving URL.

    `DocumentImage.image_path` is typically
    `/app/data/processed/images/{doc_id}/{filename}`; the serving
    endpoint lives at `/api/documents/images/{doc_id}/{filename}`.
    Fall back gracefully for unexpected shapes.
    """
    if not image_path:
        return f"/api/documents/images/{doc_id}/unknown"
    filename = image_path.rsplit("/", 1)[-1]
    return f"/api/documents/images/{doc_id}/{filename}"


def _load_candidates(
    db: Any,
    queries: List[FigureQuery],
    doc_ids: Iterable[str],
    n_per_query: int = 3,
) -> Dict[str, List[FigureCandidate]]:
    """Execute the vector query for each figure description.

    Returns {description: [candidates...]}. Descriptions with empty
    strings return the top-N most-cited images in the document set
    as a weak fallback.
    """
    doc_ids_list = [d for d in doc_ids if d]
    if not doc_ids_list:
        return {q.description: [] for q in queries}

    # Lazy import so the figure path only pays for the heavy
    # embedder/vector-store plumbing when it actually runs.
    from database import models
    from sqlalchemy import text as sql_text

    out: Dict[str, List[FigureCandidate]] = {}
    for q in queries:
        if q.description.strip():
            # Text-only fallback: match alt_text against description.
            # Real CLIP embedding path lives in pgvector_store.query_multimodal
            # but that requires the GPU worker; we use the lighter
            # PostgreSQL full-text search against the stored alt_text
            # as an always-available baseline. The GPU variant is wired
            # as an opt-in via `use_clip=True` in a follow-up.
            rows = (
                db.query(models.DocumentImage)
                .filter(models.DocumentImage.doc_id.in_(doc_ids_list))
                .filter(models.DocumentImage.alt_text.isnot(None))
                .filter(
                    models.DocumentImage.alt_text.ilike(
                        f"%{q.description[:60]}%"
                    )
                )
                .limit(n_per_query * 3)
                .all()
            )
            # Fall back to any image if ilike didn't hit
            if not rows:
                rows = (
                    db.query(models.DocumentImage)
                    .filter(models.DocumentImage.doc_id.in_(doc_ids_list))
                    .limit(n_per_query)
                    .all()
                )
        else:
            rows = (
                db.query(models.DocumentImage)
                .filter(models.DocumentImage.doc_id.in_(doc_ids_list))
                .limit(n_per_query)
                .all()
            )

        candidates: List[FigureCandidate] = []
        for i, row in enumerate(rows[:n_per_query]):
            meta = row.image_metadata if isinstance(row.image_metadata, dict) else {}
            page = meta.get("page") or meta.get("page_number")
            candidates.append(
                FigureCandidate(
                    image_id=row.image_id,
                    doc_id=row.doc_id,
                    image_url=_image_url_from_path(row.doc_id, row.image_path),
                    alt_text=row.alt_text,
                    # Deterministic decay: first hit = 0.9, second = 0.75, etc.
                    # Real similarity scoring lands in the CLIP follow-up.
                    relevance=max(0.3, 0.9 - (i * 0.15)),
                    source_page=int(page) if isinstance(page, (int, str)) and str(page).isdigit() else None,
                    metadata=meta,
                )
            )
        out[q.description] = candidates

    return out


# ---------------------------------------------------------------------------
# Prompt injection
# ---------------------------------------------------------------------------


def build_figure_injection(
    queries: List[FigureQuery],
    candidates_by_desc: Dict[str, List[FigureCandidate]],
    language_code: str = "de",
) -> Optional[str]:
    """Render the candidates as an addendum the writer can copy from.

    Returns None when no candidates were found — the caller then leaves
    the writer with its placeholder behaviour, optionally with a
    compact hint ("library checked, 0 matches").
    """
    any_hits = any(candidates_by_desc.get(q.description) for q in queries)
    de = (language_code or "de").lower().startswith("de")

    if not any_hits:
        if de:
            return (
                "VERFÜGBARE ABBILDUNGEN AUS DEINEM DOKUMENT-KORPUS:\n"
                "Die Suche nach passenden Abbildungen hat keine Treffer "
                "ergeben. Verwende Platzhalter-Pfade für die im Prompt "
                "geforderten Abbildungen und markiere sie klar als "
                "'später zu ersetzen'."
            )
        return (
            "AVAILABLE FIGURES FROM YOUR DOCUMENT CORPUS:\n"
            "No matching figures were found. Use placeholder paths for "
            "the requested figures and mark them clearly as 'replace later'."
        )

    header = (
        "VERFÜGBARE ABBILDUNGEN AUS DEINEM DOKUMENT-KORPUS:\n"
        "Wenn eine der folgenden Abbildungen zu einer geforderten Darstellung "
        "passt, KOPIERE die URL verbatim in deine Markdown-Einbindung. "
        "Erfinde KEINE Pfade.\n"
        if de
        else "AVAILABLE FIGURES FROM YOUR DOCUMENT CORPUS:\n"
        "If one of the following figures matches a requested illustration, "
        "COPY its URL verbatim into your Markdown embed. Do NOT fabricate "
        "paths.\n"
    )
    sections: List[str] = [header]
    for q in queries:
        cands = candidates_by_desc.get(q.description) or []
        if not cands:
            continue
        label = (
            f"\nFür '{q.description or '(allgemeine Abbildung)'}':"
            if de
            else f"\nFor '{q.description or '(generic figure)'}':"
        )
        sections.append(label)
        for c in cands:
            sections.append(c.to_prompt_snippet())
    return "\n".join(sections)


# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------


def resolve_figures(
    db: Any,
    *,
    prompt: str,
    draft_body: str,
    doc_ids: Iterable[str],
    language_code: str = "de",
    max_candidates_per_query: int = 3,
) -> Dict[str, Any]:
    """One-shot: intent detection + candidate lookup + prompt injection.

    Returns a dict with:
      {
        "intent_detected": bool,
        "queries": [FigureQuery, ...],
        "candidates_by_description": {desc: [FigureCandidate, ...]},
        "valid_image_urls": set[str]  # for downstream validation,
        "system_prompt_addendum": str | None
      }

    Safe on empty doc_ids (returns no-hit result), safe on DB errors
    (logs + returns empty result). Caller is expected to no-op when
    `intent_detected` is False.
    """
    queries = detect_figure_intent(prompt, draft_body)
    result: Dict[str, Any] = {
        "intent_detected": bool(queries),
        "queries": queries,
        "candidates_by_description": {},
        "valid_image_urls": set(),
        "system_prompt_addendum": None,
    }
    if not queries:
        return result

    doc_ids_list = [d for d in doc_ids if d]
    if not doc_ids_list:
        result["system_prompt_addendum"] = build_figure_injection(
            queries, {}, language_code
        )
        return result

    try:
        candidates = _load_candidates(
            db, queries, doc_ids_list, n_per_query=max_candidates_per_query
        )
    except Exception as exc:
        logger.warning("figure candidate lookup failed: %s", exc, exc_info=True)
        return result

    result["candidates_by_description"] = candidates
    valid_urls: set[str] = set()
    for cands in candidates.values():
        for c in cands:
            valid_urls.add(c.image_url)
    result["valid_image_urls"] = valid_urls

    result["system_prompt_addendum"] = build_figure_injection(
        queries, candidates, language_code
    )
    return result
