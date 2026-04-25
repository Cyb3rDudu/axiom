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


# Description fragments that mark a captured "Abbildung N: ..." string
# as a TEMPLATE rather than a real figure description. User prompts
# often contain example caption shapes like
#   *Abbildung N: <Beschreibung>. Quelle: <Quellenangabe>. Eigene Darstellung.*
# meant as a writer instruction. Captured descriptions that read like
# `... Quelle: ... Eigene Darstellung` or `<Beschreibung>` would otherwise
# be embedded by CLIP into noise and produce zero matches.
_TEMPLATE_PLACEHOLDER_TOKENS = (
    "...",  # ellipsis as placeholder
    "<",    # angle-bracket placeholder like <Beschreibung>
    "[",    # square-bracket placeholder like [Beschreibung]
)


def _is_template_placeholder(description: str) -> bool:
    """Return True if ``description`` looks like an instruction template.

    Conservative: triggers only when the description contains an
    ellipsis or angle/square-bracket placeholder marker. Real captions
    almost never contain these characters, while template phrasings do.
    """
    if not description:
        return False
    return any(token in description for token in _TEMPLATE_PLACEHOLDER_TOKENS)


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
    #     "Abbildung N: description" or "Figure N: description".
    #
    # Description capture stops at Markdown delimiters (`]`, `*`, `(`)
    # so URLs and italic-caption boundaries don't bleed in. Without
    # this, a prompt that contains a Markdown image example like
    #   ![Abbildung 1: BIP-Wachstum](/api/images/x/y.png)
    # would yield description="BIP-Wachstum](/api/images/x/y.png)" —
    # which the CLIP encoder dutifully embeds into noise.
    for m in re.finditer(
        r"(?:Abbildung|Figure|Diagramm|Chart)\s+\d+\s*[:—-]\s*([^\n\]\*\(]{8,200})",
        prompt or "",
        re.IGNORECASE,
    ):
        desc = m.group(1).strip().rstrip(".").rstrip(",")
        if not desc:
            continue
        # Template/placeholder filter: prompts often contain example
        # captions like `*Abbildung N: <Beschreibung>. Quelle: ...*`
        # to instruct the writer. Such templated descriptions encode
        # to noise in CLIP and produce zero matches. Skip when the
        # captured text is dominated by ellipses or angle-bracket
        # placeholders.
        if _is_template_placeholder(desc):
            continue
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
    image_url: str       # /api/images/{doc_id}/{filename}
    alt_text: Optional[str]
    relevance: float     # 0-1; higher = more confident match
    source_document_title: Optional[str] = None
    source_page: Optional[int] = None
    metadata: Dict[str, Any] = None  # type: ignore[assignment]

    def to_prompt_snippet(self) -> str:
        """Render the candidate as a prompt line the writer copies from.

        Previous iterations failed two different ways:
        - First: pre-rendered `![candidate figure](url)` → writer
          copied verbatim, leaving every figure with alt="candidate
          figure".
        - Then: switched to labelled fields with "URL (copy verbatim)"
          → writer invented its own paths instead of pasting the real
          URL, producing invalid links the backend had to replace with
          `about:blank#figure-not-resolved`.

        Current approach: emit a FULLY-FORMED Markdown image tag with a
        WRITER-REPLACES-THIS alt-text sigil. The writer is instructed
        (in the injection header) to keep the URL and replace ONLY the
        alt text between the square brackets. This gives the writer a
        syntactic scaffold while still requiring a meaningful caption.
        """
        alt = (self.alt_text or "").strip() or "(no stored caption)"
        src = self.source_document_title or self.doc_id[:8]
        page = f", p. {self.source_page}" if self.source_page else ""
        return (
            f"  ![<REPLACE-WITH-GERMAN-CAPTION>]({self.image_url})\n"
            f"  ↑ stored caption hint: {alt}\n"
            f"  ↑ source: {src}{page}\n"
            f"  ↑ relevance: {self.relevance:.2f}"
        )


def _image_url_from_path(doc_id: str, image_path: str) -> str:
    """Project the absolute disk path into the HTTP serving URL.

    `DocumentImage.image_path` is typically
    `/app/data/processed/images/{doc_id}/{filename}`; the serving
    endpoint lives at `/api/images/{doc_id}/{filename}` (defined on
    api/documents.py:1463 with the documents router mounted at /api).
    Fall back gracefully for unexpected shapes.
    """
    if not image_path:
        return f"/api/images/{doc_id}/unknown"
    filename = image_path.rsplit("/", 1)[-1]
    return f"/api/images/{doc_id}/{filename}"


# ---------------------------------------------------------------------------
# CLIP semantic search path
# ---------------------------------------------------------------------------


# Cosine threshold below which a CLIP candidate is dropped. CLIP
# ViT-B/32 image-text similarities for genuine semantic matches sit in
# 0.20-0.35; anything below ~0.20 is noise. 0.22 is the architect's
# starting point — tune from telemetry, not from the spec.
_CLIP_MIN_SIMILARITY = 0.22


# Heuristic alt-text patterns we drop unconditionally. Logos, covers,
# title pages, imprints — extracted by Marker from PDF prelims that
# almost never carry analytical content. No word-boundary anchors:
# German compounds like "Verlagslogo" / "Buchcover" must match.
_ALT_TEXT_REJECT_RE = re.compile(
    r"(?:logo|cover|titelblatt|impressum|copyright|frontispiz)",
    re.IGNORECASE,
)


# Minimum image dimension (in pixels) we accept. Logos and decorative
# bullets are usually <300px on at least one axis.
_MIN_IMAGE_DIMENSION = 300


def _is_likely_decorative(alt_text: Optional[str], metadata: Optional[dict]) -> bool:
    """Heuristic pre-filter for the obvious-junk class.

    Returns True if the candidate is almost certainly NOT useful for an
    academic deliverable: alt_text matches the reject list, or the
    image is too small on either axis. Applied AFTER cosine ordering
    so we only drop candidates that the heuristic class catches, not
    every unusual image.
    """
    if alt_text and _ALT_TEXT_REJECT_RE.search(alt_text):
        return True
    if isinstance(metadata, dict):
        for k in ("width", "image_width"):
            try:
                w = int(metadata.get(k) or 0)
            except (TypeError, ValueError):
                w = 0
            if w and w < _MIN_IMAGE_DIMENSION:
                return True
        for k in ("height", "image_height"):
            try:
                h = int(metadata.get(k) or 0)
            except (TypeError, ValueError):
                h = 0
            if h and h < _MIN_IMAGE_DIMENSION:
                return True
    return False


# Lazy module-level cache for the SentenceTransformer instance. The
# CLIP model is ~150MB on disk and 2-4s to cold-load; we keep one
# instance per process and reuse for every text-encode call.
_clip_text_encoder = None


def _get_clip_text_encoder():
    """Return the shared SentenceTransformer used for query encoding.

    CLIP ViT-B/32 lives in the same SentenceTransformer instance as the
    one the indexer uses for image embedding (see
    core_rag/vision_embedder.py). Reusing the existing class keeps us
    on the same model + device config and avoids duplicate downloads.
    """
    global _clip_text_encoder
    if _clip_text_encoder is None:
        from ai_researcher.core_rag.vision_embedder import VisionEmbedder
        _clip_text_encoder = VisionEmbedder()
    return _clip_text_encoder


def _load_candidates_clip(
    db: Any,
    queries: List[FigureQuery],
    doc_ids: Iterable[str],
    n_per_query: int = 3,
) -> Dict[str, List[FigureCandidate]]:
    """CLIP semantic-search candidate lookup.

    For each non-empty description, encodes the description through
    CLIP's text encoder, runs a cosine-similarity query against
    pgvector image embeddings scoped to ``doc_ids``, applies the
    heuristic pre-filter and the cosine threshold, and returns the
    top-N survivors.

    Failure modes (each returns ``[]`` for the offending query):
    - Empty description (generic intent) — short-circuit.
    - SentenceTransformer load error — propagate to the caller's
      try/except so the keyword fallback can take over.
    - All candidates below threshold — return [].
    - All candidates filtered by heuristic — return [].

    Logs structured per-query: (description, top-5 sims, accepted,
    rejected_threshold, rejected_heuristic). The architect's tuning
    workflow depends on this telemetry.
    """
    doc_ids_list = [d for d in doc_ids if d]
    if not doc_ids_list:
        return {q.description: [] for q in queries}

    from ai_researcher.core_rag.pgvector_store import PGVectorStore

    encoder = _get_clip_text_encoder()
    store = PGVectorStore()

    out: Dict[str, List[FigureCandidate]] = {}
    for q in queries:
        if not q.description.strip():
            out[q.description] = []
            continue

        # CLIP's text encoder lives in the same SentenceTransformer
        # instance as the image encoder (shared embedding space). We
        # call .model.encode(text) — produces a 512-dim numpy array
        # comparable to image embeddings via cosine similarity.
        try:
            text_vec = encoder.model.encode(
                q.description, convert_to_numpy=True
            )
        except Exception as exc:
            logger.warning(
                "clip text-encode failed for description=%r: %s",
                q.description[:80], exc,
            )
            out[q.description] = []
            continue

        # Cosine search against image_embedding column. Pull n*3
        # candidates so heuristic + threshold filtering still leave
        # us with n. doc_id scoping enforced by pgvector_store.
        raw_results = store.query_multimodal(
            query_image_embedding=text_vec,
            n_results=n_per_query * 3,
            text_weight=0.0,
            image_weight=1.0,
            doc_ids=doc_ids_list,
        )

        accepted: List[FigureCandidate] = []
        rejected_threshold = 0
        rejected_heuristic = 0
        top_sims: List[float] = []
        for r in raw_results:
            if r.get("type") != "image":
                continue
            sim = float(r.get("similarity") or 0.0)
            top_sims.append(sim)
            if sim < _CLIP_MIN_SIMILARITY:
                rejected_threshold += 1
                continue
            if _is_likely_decorative(r.get("alt_text"), r.get("metadata")):
                rejected_heuristic += 1
                continue

            meta = r.get("metadata") if isinstance(r.get("metadata"), dict) else {}
            page = meta.get("page") or meta.get("page_number")
            accepted.append(
                FigureCandidate(
                    image_id=r.get("image_id") or "",
                    doc_id=r.get("doc_id") or "",
                    image_url=_image_url_from_path(
                        r.get("doc_id") or "", r.get("image_path") or ""
                    ),
                    alt_text=r.get("alt_text"),
                    relevance=sim,
                    source_page=int(page) if isinstance(page, (int, str)) and str(page).isdigit() else None,
                    metadata=meta,
                )
            )
            if len(accepted) >= n_per_query:
                break

        logger.info(
            "clip figure search: desc=%r top5_sims=%s accepted=%d "
            "rejected_threshold=%d rejected_heuristic=%d",
            q.description[:80],
            [round(s, 3) for s in top_sims[:5]],
            len(accepted),
            rejected_threshold,
            rejected_heuristic,
        )
        out[q.description] = accepted

    return out


def _load_candidates(
    db: Any,
    queries: List[FigureQuery],
    doc_ids: Iterable[str],
    n_per_query: int = 3,
) -> Dict[str, List[FigureCandidate]]:
    """Execute the alt-text keyword query for each figure description.

    Returns {description: [candidates...]}. Two FAILURE FAST paths
    (architect + reviewer team consensus, after live bug where this
    function surfaced publisher logos and unrelated diagrams as
    "relevance: 0.90"):

    - Empty description → []. The caller's writer prompt already has a
      "no matching figures" branch; better to surface that than to
      return arbitrary images sorted by SQL row order.
    - alt_text ILIKE returns no rows → []. Same reasoning. The previous
      "fall back to any image" path produced the publisher-logo-as-
      relevance-0.9 incident.

    The relevance score on returned candidates is intentionally a fixed
    indication that the keyword path matched (0.5 = "matched on alt
    text, no semantic ranking"). The earlier 0.9 / 0.75 / 0.6 positional
    decay was misleading — it presented results sorted by SQL row order
    as if they had cosine-similarity ranking. CLIP semantic search
    lands in a follow-up PR; until then, the score is a flat indicator.
    """
    doc_ids_list = [d for d in doc_ids if d]
    if not doc_ids_list:
        return {q.description: [] for q in queries}

    # Lazy import so the figure path only pays for the heavy
    # embedder/vector-store plumbing when it actually runs.
    from database import models

    out: Dict[str, List[FigureCandidate]] = {}
    for q in queries:
        if not q.description.strip():
            # Generic figure intent without a description — refuse to
            # guess. The writer gets the "library checked, 0 matches"
            # branch and emits placeholder paths the user can replace.
            out[q.description] = []
            continue

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

        candidates: List[FigureCandidate] = []
        for row in rows[:n_per_query]:
            meta = row.image_metadata if isinstance(row.image_metadata, dict) else {}
            page = meta.get("page") or meta.get("page_number")
            candidates.append(
                FigureCandidate(
                    image_id=row.image_id,
                    doc_id=row.doc_id,
                    image_url=_image_url_from_path(row.doc_id, row.image_path),
                    alt_text=row.alt_text,
                    # Flat indicator — keyword match has no ranking.
                    # CLIP semantic search will replace this with real
                    # cosine similarity.
                    relevance=0.5,
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
        "\n"
        "Jede Abbildung ist als fertige Markdown-Zeile vorgegeben mit\n"
        "  ![<REPLACE-WITH-GERMAN-CAPTION>](<echte-URL-NICHT-ÄNDERN>)\n"
        "\n"
        "WAS DU TUST:\n"
        "1. URL unverändert kopieren — Zeichen für Zeichen. KEINE Pfade "
        "erfinden, KEINE Domänen ersetzen, KEINE placeholder-fig.png\n"
        "2. Nur den alt-Text in den eckigen Klammern ersetzen durch eine "
        "inhaltlich passende Beschreibung, Format 'Abbildung N: <was zu "
        "sehen ist>'.\n"
        "3. Danach eine Bildunterschriften-Zeile in Kursiv direkt drunter:\n"
        "  `*Abbildung N: <Beschreibung>. Quelle: <Quellenangabe>. "
        "Eigene Darstellung.*`\n"
        "\n"
        "NIEMALS: 'candidate figure', 'stored caption', 'REPLACE-WITH' "
        "als alt-Text belassen — das sind Platzhalter.\n"
        if de
        else "AVAILABLE FIGURES FROM YOUR DOCUMENT CORPUS:\n"
        "\n"
        "Each figure is pre-rendered as a ready-to-use Markdown line:\n"
        "  ![<REPLACE-WITH-CAPTION>](<real-url-do-not-modify>)\n"
        "\n"
        "WHAT YOU DO:\n"
        "1. Copy the URL unchanged — character for character. Do NOT "
        "fabricate paths, do NOT substitute domains, do NOT use "
        "placeholder-fig.png.\n"
        "2. Replace ONLY the alt text between the square brackets with "
        "a meaningful 'Figure N: <what it shows>' description.\n"
        "3. Follow with an italic caption line:\n"
        "  `*Figure N: <description>. Source: <citation>.*`\n"
        "\n"
        "NEVER leave 'candidate figure', 'stored caption', or "
        "'REPLACE-WITH' as alt text — those are placeholder tokens.\n"
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
    use_clip: bool = False,
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

    When ``use_clip=True`` (gated on the writing.clip_figures.enabled
    user flag), the candidate lookup uses CLIP semantic search via
    pgvector cosine similarity instead of alt_text keyword matching.
    Falls back to the keyword path on CLIP failure (model load error,
    NULL embeddings, etc.) so a figure resolver outage never blocks the
    writer.
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

    candidates: Dict[str, List[FigureCandidate]] = {}
    used_path = "keyword"
    if use_clip:
        try:
            candidates = _load_candidates_clip(
                db, queries, doc_ids_list, n_per_query=max_candidates_per_query
            )
            used_path = "clip"
        except Exception as exc:
            logger.warning(
                "figure CLIP lookup failed, falling back to keyword: %s",
                exc,
                exc_info=True,
            )
            candidates = {}
            used_path = "clip_failed"

    if not candidates and used_path != "clip":
        # Either CLIP path was off, or it errored — use the keyword path
        # as a safety net. PR 4 will delete this branch once CLIP is
        # default-on with backfill confirmed.
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
    result["resolver_path"] = used_path

    result["system_prompt_addendum"] = build_figure_injection(
        queries, candidates, language_code
    )
    return result
