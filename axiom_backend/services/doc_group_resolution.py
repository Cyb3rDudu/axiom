"""Resolve one or more document-group IDs to the union of their document IDs.

Both the writing agent and the research mission controller need the same
plumbing: given either the legacy singular ``document_group_id`` or the
new ``document_group_ids`` list, produce a flat, deduplicated list of
document UUIDs that can be fed into ``document_search_tool`` via its
existing ``filter_doc_ids`` parameter.
"""

from __future__ import annotations

from typing import Iterable, List, Optional

from sqlalchemy.orm import Session

from database import models


def normalize_group_ids(
    group_id: Optional[str],
    group_ids: Optional[Iterable[str]],
) -> List[str]:
    """Fold both input forms into a single deduplicated list.

    Preserves first-seen order so the caller can keep a stable 'primary
    group first' convention if desired. Drops empty/None entries.
    """
    collected: List[str] = []
    seen: set = set()
    for candidate in list(group_ids or []) + ([group_id] if group_id else []):
        if not candidate:
            continue
        if candidate in seen:
            continue
        seen.add(candidate)
        collected.append(candidate)
    return collected


def resolve_group_ids_to_doc_ids(
    db: Session,
    user_id: int,
    group_ids: Iterable[str],
) -> List[str]:
    """Return the union of document IDs across the given groups.

    Groups not owned by ``user_id`` are silently skipped — this mirrors the
    behaviour of the single-group path and avoids leaking the existence of
    other users' groups when a client passes a stale or wrong UUID.
    """
    ids = [gid for gid in group_ids if gid]
    if not ids:
        return []

    # Join groups → association so we only touch owned groups and pull
    # their document IDs in one query.
    rows = (
        db.query(models.Document.id)
        .join(
            models.document_group_association,
            models.Document.id == models.document_group_association.c.document_id,
        )
        .join(
            models.DocumentGroup,
            models.DocumentGroup.id
            == models.document_group_association.c.document_group_id,
        )
        .filter(
            models.DocumentGroup.id.in_(ids),
            models.DocumentGroup.user_id == user_id,
        )
        .distinct()
        .all()
    )
    return [str(row[0]) for row in rows]
