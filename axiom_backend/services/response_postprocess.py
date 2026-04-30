"""Writer response post-processing (Writing Completeness Contract).

Three passes run after the LLM response lands, before it's persisted
as the assistant message the user sees:

1. **Recompute Wortbilanz** — replace the model's hallucinated word
   count with the deterministic body count (Stage 1a).
2. **Synthesize sources block** — ensure the response always carries
   a `content-block:references` fence reflecting the CURRENT registry
   state, not whatever the LLM happened to emit this turn (Stage 1b).
3. **Validate figure URLs** — replace fabricated placeholder paths
   with real `document_images` URLs or a clearly-flagged fallback
   (Stage 3b; the actual RAG lookup happens upstream).

Each pass is individually feature-flag gated and idempotent — safe to
run twice, safe to run on inputs that already satisfy the invariant.
"""

from __future__ import annotations

import json
import logging
import re
from typing import Any, Dict, List, Optional, Tuple

from services.writing_markdown import REFS_FENCE_RE

logger = logging.getLogger(__name__)


# Re-export so existing tests that import `_REFERENCES_FENCE_RE` keep working.
_REFERENCES_FENCE_RE = REFS_FENCE_RE


# ---------------------------------------------------------------------------
# Stage 1b: synthesize canonical references block
# ---------------------------------------------------------------------------


def _reference_to_dict(ref: Any) -> Dict[str, Any]:
    """Project a SQLAlchemy Reference row into the JSON shape the writer
    emits, so round-tripping is clean."""
    out: Dict[str, Any] = {
        "entry_key": ref.entry_key,
        "authors": ref.authors or [],
        "title": ref.title or "",
    }
    # Only include populated optional fields so the JSON stays tight
    if ref.year is not None:
        out["year"] = ref.year
    if ref.container_title:
        out["container_title"] = ref.container_title
    if ref.publisher:
        out["publisher"] = ref.publisher
    if ref.pages:
        out["pages"] = ref.pages
    if ref.url or ref.web_url:
        out["url"] = ref.url or ref.web_url
    if ref.doi:
        out["doi"] = ref.doi
    if ref.accessed_at:
        accessed = ref.accessed_at
        # Normalise to YYYY-MM-DD
        out["accessed_at"] = accessed.strftime("%Y-%m-%d") if hasattr(accessed, "strftime") else str(accessed)
    out["reference_type"] = ref.reference_type or "web"
    return out


def _render_references_fence(entries: List[Dict[str, Any]]) -> str:
    """Serialize entries as a content-block:references fence, sorted by entry_key
    for stable diffing across turns."""
    ordered = sorted(entries, key=lambda e: e.get("entry_key") or "")
    body = json.dumps(ordered, ensure_ascii=False, indent=2)
    return f"```content-block:references\n{body}\n```"


def synthesize_sources_block(
    content: str,
    registry_refs: List[Any],
) -> Tuple[str, Dict[str, Any]]:
    """Inject / replace the references fence based on the CURRENT registry.

    If the response already has a references fence, replace its body
    with the registry snapshot (overrides anything the LLM improvised).
    If the response has no references fence, prepend one before the
    document block — consistent with the writer prompt's "refs first"
    contract (PR #82).

    Returns (updated_content, telemetry).
    """
    entries = [_reference_to_dict(r) for r in registry_refs if getattr(r, "entry_key", None)]
    telemetry: Dict[str, Any] = {
        "registry_count": len(entries),
        "llm_emitted_fence": False,
        "action": "no_registry",
    }

    if not entries:
        return content, telemetry

    canonical = _render_references_fence(entries)

    match = _REFERENCES_FENCE_RE.search(content or "")
    if match is not None:
        telemetry["llm_emitted_fence"] = True
        # Compare sorted entry_keys to decide if we actually changed anything
        try:
            llm_entries = json.loads(match.group(1))
            llm_keys = sorted(
                [e.get("entry_key") for e in llm_entries if isinstance(e, dict)]
            )
            reg_keys = sorted([e["entry_key"] for e in entries])
            if llm_keys == reg_keys:
                telemetry["action"] = "replaced_equivalent"
            else:
                telemetry["action"] = "replaced_corrected"
        except (ValueError, AttributeError, TypeError):
            telemetry["action"] = "replaced_malformed"

        start, end = match.span()
        updated = content[:start] + canonical + content[end:]
        return updated, telemetry

    # No fence — prepend canonical one, before the document block if present
    doc_idx = (content or "").find("```content-block:document")
    if doc_idx >= 0:
        updated = (content[:doc_idx].rstrip() + "\n\n" + canonical + "\n\n" + content[doc_idx:])
        telemetry["action"] = "prepended"
    else:
        updated = canonical + "\n\n" + (content or "")
        telemetry["action"] = "prefixed_no_doc"

    return updated, telemetry


# ---------------------------------------------------------------------------
# Stage 3b: figure URL validation + fallback
# ---------------------------------------------------------------------------


_FIGURE_MARKDOWN_RE = re.compile(r"!\[([^\]]*)\]\(([^)]+)\)")

# Real figure URLs follow /api/images/{doc_id}/{filename}
# (route defined in api/documents.py:1463; documents router is mounted
# at /api so the full served path is /api/images/...). Anything else in
# a writing response was either manually typed by a user (rare) or
# hallucinated by the writer (common — "placeholder-fig1.png").
#
# Legacy drafts persisted prior to PR #98 used the wrong path
# /api/documents/images/... (figure resolver bug). Match either form so
# old drafts don't get their valid URLs flagged as invalid; new drafts
# emit only the correct path.
_VALID_FIGURE_URL_RE = re.compile(
    r"^/api/(?:documents/)?images/[^/]+/[^/]+$"
)


# Trusted external hosts that may legitimately appear in figure
# Markdown when the user explicitly cites institutional / official-data
# sources (Bundesbank, Destatis, IWF, World Bank, IEA reports etc.) —
# typically pasted by the user into the writing prompt as concrete
# image URLs the agent is told to copy verbatim.
#
# Hosts are matched as exact equals OR as a suffix (so subdomains pass:
# `data.worldbank.org`, `klardenker.kpmg.de`). HTTPS is required —
# plain http:// is rejected to avoid mixed-content warnings in the
# editor.
_TRUSTED_EXTERNAL_HOSTS = frozenset({
    # Statistical / central-bank sources
    "destatis.de",
    "bundesbank.de",
    "ecb.europa.eu",
    "imf.org",
    "data.worldbank.org",
    "worldbank.org",
    "oecd.org",
    "data.oecd.org",
    "stats.oecd.org",
    # German institutional / federal sources
    "bpb.de",
    "bmwk.de",
    "bmf.bund.de",
    "europarl.europa.eu",
    # Swiss
    "seco.admin.ch",
    "snb.ch",
    "bfs.admin.ch",
    # Austrian
    "wko.at",
    "statistik.at",
    "oenb.at",
    # Trade promotion / specialised aggregators commonly used in
    # academic citations (curated, not exhaustive)
    "gtai.de",
    "theglobaleconomy.com",
    "iea.org",
    "ember-energy.org",
    # Press / consultancy charts that are routinely cited
    "klardenker.kpmg.de",
    "kpmg.de",
    "merics.org",
    "atlanticcouncil.org",
    "cer.eu",
    "swp-berlin.org",
    "ifo.de",
    "iwkoeln.de",
    "bertelsmann-stiftung.de",
    # Wikimedia Commons / Wikipedia
    "upload.wikimedia.org",
    "commons.wikimedia.org",
})


# Image-bearing path patterns we accept when matching against a
# trusted host: explicit image extension OR a query-string that hints
# at chart rendering (TheGlobalEconomy uses ?p=…&i=… to serve PNGs).
_TRUSTED_PATH_HINT_RE = re.compile(
    r"\.(?:png|jpe?g|gif|webp|svg)(?:\?|#|$)"
    r"|graph_country\.php"
    r"|/charts?/"
    r"|/wp-content/uploads/",
    re.IGNORECASE,
)


def _is_trusted_external_image_url(url: str) -> bool:
    """Return True for an HTTPS URL pointing at a known citable host
    AND whose path pattern looks like an image (extension OR known
    chart-serving idiom). Both checks must pass — neither alone is
    sufficient (a PDF on iea.org is not a figure; an arbitrary .png
    on a random host could be hallucinated).
    """
    if not url or not url.startswith("https://"):
        return False
    # Extract host
    rest = url[len("https://"):]
    host = rest.split("/", 1)[0].split("?", 1)[0].split("#", 1)[0].lower()
    if not host:
        return False
    # Suffix match against trusted set: `data.worldbank.org` matches
    # `worldbank.org` and `data.worldbank.org`; arbitrary subdomains
    # of trusted apex domains pass.
    host_matches = host in _TRUSTED_EXTERNAL_HOSTS or any(
        host.endswith("." + h) for h in _TRUSTED_EXTERNAL_HOSTS
    )
    if not host_matches:
        return False
    return bool(_TRUSTED_PATH_HINT_RE.search(url))


def validate_figure_urls(
    content: str,
    valid_image_paths: Optional[set[str]] = None,
) -> Tuple[str, Dict[str, Any]]:
    """Flag figure Markdown with non-resolvable URLs.

    `valid_image_paths` is a set of `/api/images/{doc_id}/{file}` paths
    that actually exist. If provided, paths outside the set get flagged.
    If None, we only reject obviously-fabricated paths (placeholder-
    fig1.png, example.com, etc.).

    Fabricated paths are rewritten to a deterministic placeholder:
      ![…](about:blank#figure-not-resolved)
    so the editor shows a broken-image glyph the user can fix, rather
    than a misleading "loading forever" spinner.
    """
    telemetry: Dict[str, Any] = {
        "figures_total": 0,
        "figures_resolved": 0,
        "figures_placeholder": 0,
        "figures_invalid": 0,
    }
    if not content:
        return content, telemetry

    def _replace(m: re.Match) -> str:
        alt, url = m.group(1), m.group(2).strip()
        telemetry["figures_total"] += 1
        is_valid = bool(_VALID_FIGURE_URL_RE.match(url))
        if is_valid and valid_image_paths is not None and url not in valid_image_paths:
            is_valid = False
        # Trusted-external pass-through: HTTPS URLs to a curated set of
        # citable institutional sources (Destatis, IWF, IEA, KPMG, …)
        # with an image-shaped path are accepted when the user pasted
        # them into the writing prompt and the writer copied them
        # verbatim. Internal corpus URLs (the matcher above) remain the
        # primary path.
        if not is_valid and _is_trusted_external_image_url(url):
            is_valid = True
        if is_valid:
            telemetry["figures_resolved"] += 1
            return m.group(0)
        # Detect obvious placeholders
        if re.search(r"placeholder|example\.com|example\.org|lorem|ipsum", url, re.I):
            telemetry["figures_placeholder"] += 1
        else:
            telemetry["figures_invalid"] += 1
        return f"![{alt}](about:blank#figure-not-resolved)"

    updated = _FIGURE_MARKDOWN_RE.sub(_replace, content)
    return updated, telemetry
