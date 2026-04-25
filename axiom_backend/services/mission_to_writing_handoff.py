"""Mission → writing-session handoff projection (#73).

When a user continues a finished research mission in writing mode, the
previous flow copied only the draft body. The mission's citation graph
+ Literaturportfolio were left behind, forcing either:

- a manual "Aus Markdown importieren" migration (lossy — loses
  document_id links, only recovers surface fields from the inline
  Markdown), or
- a wasted writer turn that re-emits content-block:references (costs
  another Sonnet call for data we already have).

This module projects the mission's structured state directly into the
writing session's `draft_references` + `drafts.portfolio_output` so the
Bibliography widget starts populated and the Portfolio panel already
shows the mission's traffic light.

Invoked from `api/writing.py::create_writing_session` or the first
draft-create path, depending on when `mission_source_id` is known.
"""

from __future__ import annotations

import logging
import uuid
from datetime import datetime
from typing import Any, Dict, Iterable, List, Mapping, Optional

from sqlalchemy.orm import Session

from database import models
from services.author_parser import parse_authors
from services.structured_bibliography import (
    compute_source_fingerprint,
    slugify_entry_key,
)

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _parse_authors_string(raw: Any) -> List[Dict[str, str]]:
    """Delegate to shared author parser for the {family, given} shape.

    Kept as a thin alias so the mission-handoff call site reads as a
    semantic operation rather than a generic parser call.
    """
    return parse_authors(raw)


def _coerce_year(raw: Any) -> Optional[int]:
    if raw in (None, "", "n.d.", "o. J.", "o.J."):
        return None
    try:
        return int(str(raw).strip()[:4])
    except (ValueError, TypeError):
        return None


def _note_meta(note: Mapping[str, Any]) -> Dict[str, Any]:
    meta = note.get("source_metadata") or {}
    if not isinstance(meta, Mapping):
        return {}
    return dict(meta)


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------


def project_mission_into_draft(
    db: Session,
    *,
    mission_id: str,
    draft: models.Draft,
    user_id: int,
) -> Dict[str, Any]:
    """Copy a finished mission's citation graph + portfolio into `draft`.

    - `draft_references`: one row per cited source, with structured
      fields populated from `Note.source_metadata`. Uses a stable
      entry_key slug so subsequent writer turns can pick up where this
      left off without creating duplicates.
    - `drafts.portfolio_output`: verbatim copy of
      `missions.literature_portfolio_output`.

    Returns a summary dict the caller can log or surface on the
    response so the UI knows what landed.

    Idempotent: re-running on the same (mission, draft) pair skips
    refs whose entry_key already exists and overwrites the portfolio
    column unconditionally (mission output is the source of truth
    until the user regenerates it in writing mode).
    """
    mission = (
        db.query(models.Mission).filter(models.Mission.id == mission_id).first()
    )
    if mission is None:
        logger.info("handoff: mission %s not found — skipping projection", mission_id)
        return {"refs_created": 0, "refs_skipped": 0, "portfolio_copied": False}

    result = {
        "refs_created": 0,
        "refs_skipped": 0,
        "portfolio_copied": False,
    }

    # --- portfolio projection ----------------------------------------------

    portfolio = getattr(mission, "literature_portfolio_output", None)
    if isinstance(portfolio, dict) and portfolio:
        draft.portfolio_output = portfolio
        result["portfolio_copied"] = True
        logger.info(
            "handoff: copied portfolio from mission %s to draft %s",
            mission_id,
            draft.id,
        )

    # --- references projection ---------------------------------------------

    mission_context = getattr(mission, "mission_context", None)
    if not isinstance(mission_context, Mapping):
        db.commit()
        logger.info(
            "handoff: mission %s has no mission_context; skipping refs",
            mission_id,
        )
        return result

    notes = mission_context.get("notes") or []
    if not isinstance(notes, Iterable) or isinstance(notes, (str, bytes)):
        notes = []

    used_doc_ids = mission_context.get("used_doc_ids") or mission_context.get("cited_source_ids")
    used_doc_ids_set = set(used_doc_ids or [])

    # Dedup existing entry_keys for this draft so a retry doesn't
    # multiply rows — we only project refs the draft doesn't already have.
    existing_keys = {
        r.entry_key
        for r in (
            db.query(models.Reference.entry_key)
            .filter(
                models.Reference.draft_id == draft.id,
                models.Reference.entry_key.isnot(None),
            )
            .all()
        )
        if r.entry_key
    }

    seen_in_this_run: set[str] = set()

    for note in notes:
        if not isinstance(note, Mapping):
            continue
        source_id = note.get("source_id") or note.get("doc_id")
        if not source_id:
            continue
        # Respect the mission's "cited-only" set when available
        if used_doc_ids_set and source_id not in used_doc_ids_set:
            continue

        meta = _note_meta(note)
        source_type = note.get("source_type", "document")
        authors = _parse_authors_string(meta.get("authors"))
        year = _coerce_year(meta.get("publication_year") or meta.get("year"))
        title = (meta.get("title") or meta.get("original_filename") or "").strip()
        publisher = (meta.get("publisher") or "").strip() or None
        journal = (meta.get("journal") or "").strip() or None
        url = meta.get("url") if isinstance(meta.get("url"), str) else None
        doi = (meta.get("doi") or "").strip() or None
        pages = (meta.get("pages") or "").strip() or None

        if not title:
            # Without a title, the row would fail the writer-block
            # validator anyway. Skip rather than persist garbage.
            result["refs_skipped"] += 1
            continue

        # Stable slug from authors + year + title hint
        hint_parts = [
            authors[0]["family"] if authors else "",
            str(year) if year else "",
            title[:40],
        ]
        entry_key = slugify_entry_key(*hint_parts)
        # Uniqueness within this projection run
        base = entry_key
        n = 2
        while entry_key in seen_in_this_run or entry_key in existing_keys:
            entry_key = f"{base}-{n}"
            n += 1
        seen_in_this_run.add(entry_key)

        fingerprint = compute_source_fingerprint(
            document_id=source_id if source_type == "document" else None,
            url=url,
            pages=pages,
        )

        ref = models.Reference(
            id=str(uuid.uuid4()),
            draft_id=draft.id,
            document_id=source_id if source_type == "document" else None,
            web_url=url if source_type == "web" else None,
            citation_text=meta.get("apa_citation") or "",
            reference_type="web" if source_type == "web" else "document",
            authors=authors or None,
            year=year,
            title=title,
            container_title=journal,
            publisher=publisher,
            pages=pages,
            url=url,
            doi=doi,
            entry_key=entry_key,
            source_fingerprint=fingerprint,
            created_at=datetime.utcnow(),
        )
        db.add(ref)
        result["refs_created"] += 1

    db.commit()
    logger.info(
        "handoff: mission %s → draft %s: %d refs created, %d skipped, portfolio=%s",
        mission_id,
        draft.id,
        result["refs_created"],
        result["refs_skipped"],
        result["portfolio_copied"],
    )
    return result
