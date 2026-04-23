"""Structured bibliography service (#51/#52).

Manages the structured citation registry on draft_references — the
first-class-data replacement for the inline-Markdown Literaturverzeichnis
that used to live in the draft body.

Contract:
- One Reference row per bibliography entry, keyed by (draft_id, entry_key).
- One CitationEntry row per in-text citation occurrence.
- citation_text stays populated (rendered on write by the citation profile
  layer) so legacy readers keep working even when the entry was created
  through the structured path.

This service is additive: nothing here changes the behaviour of
legacy drafts that only use ReferenceService's chunk/web paths.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
import unicodedata
import uuid
from dataclasses import dataclass
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

from fastapi import HTTPException, status
from sqlalchemy.orm import Session

from database import models

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Entry-key slugging
# ---------------------------------------------------------------------------

_SLUG_DROP = re.compile(r"[^a-z0-9]+")
_GERMAN_UMLAUTS = str.maketrans({
    "ä": "ae", "Ä": "ae",
    "ö": "oe", "Ö": "oe",
    "ü": "ue", "Ü": "ue",
    "ß": "ss",
})


def slugify_entry_key(*parts: str) -> str:
    """Turn author/year/title hints into a stable ASCII slug.

    The writer emits entry_keys explicitly; this helper is used when the
    UI or migration layer needs to synthesize one (e.g. inline-markdown
    backfill). German umlauts are transliterated explicitly because NFKD
    decomposition doesn't map ß → ss.
    """
    joined = "-".join(p for p in parts if p)
    transliterated = joined.translate(_GERMAN_UMLAUTS)
    normalised = unicodedata.normalize("NFKD", transliterated).encode("ascii", "ignore").decode("ascii")
    slug = _SLUG_DROP.sub("-", normalised.lower()).strip("-")
    return slug or "ref"


def ensure_unique_entry_key(
    db: Session, draft_id: str, desired: str, *, exclude_id: Optional[str] = None
) -> str:
    """Append a numeric suffix when `desired` collides on (draft_id, entry_key).

    The partial unique index catches collisions at the DB level; this is
    the friendly resolver so the caller gets a usable key without
    bouncing off an IntegrityError.
    """
    base = desired or "ref"
    candidate = base
    n = 2
    while True:
        q = db.query(models.Reference.id).filter(
            models.Reference.draft_id == draft_id,
            models.Reference.entry_key == candidate,
        )
        if exclude_id:
            q = q.filter(models.Reference.id != exclude_id)
        if q.first() is None:
            return candidate
        candidate = f"{base}-{n}"
        n += 1


# ---------------------------------------------------------------------------
# Fingerprinting — used for dedup hints during writer ingest and migration
# ---------------------------------------------------------------------------

_URL_TRAILING_SLASH = re.compile(r"/+$")
_URL_WWW = re.compile(r"^https?://(www\.)?", re.IGNORECASE)


def compute_source_fingerprint(
    *,
    document_id: Optional[str] = None,
    url: Optional[str] = None,
    pages: Optional[str] = None,
) -> Optional[str]:
    """Short hash used to dedup obviously-same sources.

    - For local docs: hash of document_id + optional page range.
    - For web: hash of normalised URL (scheme + www stripped, trailing
      slashes trimmed, lowercased).
    - Neither present → None, service won't dedup.
    """
    if document_id:
        payload = f"doc:{document_id}"
        if pages:
            payload += f"|p:{pages.strip()}"
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]
    if url:
        normalised = _URL_WWW.sub("", url.strip().lower())
        normalised = _URL_TRAILING_SLASH.sub("", normalised)
        return hashlib.sha256(f"web:{normalised}".encode("utf-8")).hexdigest()[:16]
    return None


# ---------------------------------------------------------------------------
# Structured CRUD
# ---------------------------------------------------------------------------


@dataclass
class UpsertResult:
    reference: models.Reference
    created: bool  # False => existing entry updated


class StructuredBibliographyService:
    """Thin wrapper around models.Reference for the structured path."""

    def __init__(self, db: Session):
        self.db = db

    # -- access control ----------------------------------------------------

    def _assert_draft_access(self, draft_id: str, user_id: int) -> models.Draft:
        draft = (
            self.db.query(models.Draft)
            .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
            .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
            .filter(models.Draft.id == draft_id, models.Chat.user_id == user_id)
            .first()
        )
        if draft is None:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail="Draft not found or access denied",
            )
        return draft

    # -- read --------------------------------------------------------------

    def list_references(self, draft_id: str, user_id: int) -> List[models.Reference]:
        self._assert_draft_access(draft_id, user_id)
        return (
            self.db.query(models.Reference)
            .filter(models.Reference.draft_id == draft_id)
            .order_by(models.Reference.created_at.asc().nulls_last(), models.Reference.id.asc())
            .all()
        )

    def get_reference(self, reference_id: str, user_id: int) -> models.Reference:
        ref = (
            self.db.query(models.Reference)
            .join(models.Draft, models.Reference.draft_id == models.Draft.id)
            .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
            .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
            .filter(models.Reference.id == reference_id, models.Chat.user_id == user_id)
            .first()
        )
        if ref is None:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail="Reference not found or access denied",
            )
        return ref

    # -- write -------------------------------------------------------------

    def upsert_structured(
        self,
        draft_id: str,
        payload: Dict[str, Any],
        *,
        user_id: int,
        citation_text: str = "",
    ) -> UpsertResult:
        """Create or update a structured entry keyed on (draft_id, entry_key).

        `payload` is a validated dict (from StructuredReferenceCreate or the
        writer's JSON). `citation_text` is the rendered Markdown — caller is
        responsible for producing it via the citation profile layer; passed
        here so the legacy code paths still see a formatted string.
        """
        self._assert_draft_access(draft_id, user_id)

        entry_key = (payload.get("entry_key") or "").strip()
        if not entry_key:
            raise HTTPException(status_code=400, detail="entry_key is required")

        # Structural minimum: at least one of url / container_title / publisher / document_id
        if not any(
            payload.get(k)
            for k in ("url", "container_title", "publisher", "document_id")
        ):
            raise HTTPException(
                status_code=400,
                detail="Reference must have at least one of: url, container_title, publisher, document_id",
            )

        existing = (
            self.db.query(models.Reference)
            .filter(
                models.Reference.draft_id == draft_id,
                models.Reference.entry_key == entry_key,
            )
            .first()
        )

        fingerprint = payload.get("source_fingerprint") or compute_source_fingerprint(
            document_id=payload.get("document_id"),
            url=payload.get("url"),
            pages=payload.get("pages"),
        )

        fields = dict(
            draft_id=draft_id,
            document_id=payload.get("document_id"),
            web_url=payload.get("url") if payload.get("reference_type", "web") == "web" else None,
            citation_text=citation_text or payload.get("citation_text") or "",
            reference_type=payload.get("reference_type", "web"),
            authors=_normalise_authors(payload.get("authors")),
            year=payload.get("year"),
            title=payload.get("title"),
            container_title=payload.get("container_title"),
            publisher=payload.get("publisher"),
            pages=payload.get("pages"),
            url=payload.get("url"),
            accessed_at=payload.get("accessed_at"),
            doi=payload.get("doi"),
            entry_key=entry_key,
            source_fingerprint=fingerprint,
        )

        if existing is not None:
            for k, v in fields.items():
                if k == "draft_id":
                    continue
                setattr(existing, k, v)
            self.db.commit()
            self.db.refresh(existing)
            return UpsertResult(reference=existing, created=False)

        ref = models.Reference(
            id=str(uuid.uuid4()),
            created_at=datetime.utcnow(),
            **fields,
        )
        self.db.add(ref)
        self.db.commit()
        self.db.refresh(ref)
        return UpsertResult(reference=ref, created=True)

    def delete_reference(self, reference_id: str, user_id: int) -> None:
        ref = self.get_reference(reference_id, user_id)
        self.db.delete(ref)
        self.db.commit()

    # -- bulk replace ------------------------------------------------------

    def replace_draft_registry(
        self,
        draft_id: str,
        entries: List[Dict[str, Any]],
        *,
        user_id: int,
        render_citation: Optional[Any] = None,
    ) -> List[models.Reference]:
        """Atomic replace: structured entries become the full registry.

        Existing rows whose entry_key is missing from `entries` are deleted.
        Rows whose entry_key matches are updated. New entries are created.

        Called by the writer's content-block:references ingest flow — the
        writer owns the full bibliography on each revision turn, so this
        gives us the "replace, don't accumulate" behaviour that #51 is all
        about.
        """
        self._assert_draft_access(draft_id, user_id)

        desired_keys = {(e.get("entry_key") or "").strip() for e in entries}
        desired_keys.discard("")

        existing = (
            self.db.query(models.Reference)
            .filter(models.Reference.draft_id == draft_id)
            .all()
        )
        by_key = {r.entry_key: r for r in existing if r.entry_key}

        # Delete entries no longer referenced
        for r in existing:
            if r.entry_key and r.entry_key not in desired_keys:
                self.db.delete(r)

        results: List[models.Reference] = []
        for entry in entries:
            key = (entry.get("entry_key") or "").strip()
            if not key:
                logger.warning(
                    "replace_draft_registry skipping entry without entry_key: %s",
                    entry.get("title"),
                )
                continue

            citation_text = entry.get("citation_text") or ""
            if render_citation is not None:
                try:
                    citation_text = render_citation(entry) or citation_text
                except Exception as exc:  # pragma: no cover - caller provides renderer
                    logger.warning("render_citation failed for %s: %s", key, exc)

            row = by_key.get(key)
            fingerprint = entry.get("source_fingerprint") or compute_source_fingerprint(
                document_id=entry.get("document_id"),
                url=entry.get("url"),
                pages=entry.get("pages"),
            )
            if row is None:
                row = models.Reference(
                    id=str(uuid.uuid4()),
                    draft_id=draft_id,
                    created_at=datetime.utcnow(),
                )
                self.db.add(row)

            row.document_id = entry.get("document_id")
            row.web_url = entry.get("url") if entry.get("reference_type", "web") == "web" else None
            row.citation_text = citation_text
            row.reference_type = entry.get("reference_type", "web")
            row.authors = _normalise_authors(entry.get("authors"))
            row.year = entry.get("year")
            row.title = entry.get("title")
            row.container_title = entry.get("container_title")
            row.publisher = entry.get("publisher")
            row.pages = entry.get("pages")
            row.url = entry.get("url")
            row.accessed_at = entry.get("accessed_at")
            row.doi = entry.get("doi")
            row.entry_key = key
            row.source_fingerprint = fingerprint
            results.append(row)

        self.db.commit()
        for r in results:
            self.db.refresh(r)
        return results


def _normalise_authors(authors: Any) -> Optional[List[Dict[str, str]]]:
    """Accept list[AuthorName|dict|str] and return list[{family, given}]."""
    if not authors:
        return None
    out: List[Dict[str, str]] = []
    for a in authors:
        if isinstance(a, str):
            # "Family, Given" or "Given Family"
            if "," in a:
                family, _, given = a.partition(",")
                out.append({"family": family.strip(), "given": given.strip()})
            else:
                parts = a.strip().split()
                if len(parts) >= 2:
                    out.append({"family": parts[-1], "given": " ".join(parts[:-1])})
                else:
                    out.append({"family": a.strip(), "given": ""})
        elif hasattr(a, "family"):
            out.append({"family": getattr(a, "family", ""), "given": getattr(a, "given", "") or ""})
        elif isinstance(a, dict):
            out.append({"family": a.get("family", ""), "given": a.get("given", "") or ""})
    return out or None


# ---------------------------------------------------------------------------
# Citation-entry (occurrence) helpers — consumed by #55 sync
# ---------------------------------------------------------------------------


def record_citation_occurrences(
    db: Session,
    draft_id: str,
    occurrences: List[Dict[str, Any]],
) -> List[models.CitationEntry]:
    """Replace existing citation_entries for a draft with a fresh set.

    Occurrence dict shape:
        {reference_id, in_text_marker, paragraph_index?, char_offset_start?, char_offset_end?}
    """
    db.query(models.CitationEntry).filter(
        models.CitationEntry.draft_id == draft_id
    ).delete(synchronize_session=False)

    rows: List[models.CitationEntry] = []
    for occ in occurrences:
        row = models.CitationEntry(
            id=str(uuid.uuid4()),
            draft_id=draft_id,
            reference_id=occ["reference_id"],
            in_text_marker=occ["in_text_marker"],
            paragraph_index=occ.get("paragraph_index"),
            char_offset_start=occ.get("char_offset_start"),
            char_offset_end=occ.get("char_offset_end"),
        )
        db.add(row)
        rows.append(row)
    db.commit()
    return rows


# ---------------------------------------------------------------------------
# Writer output parser (#53) — extracts content-block:references JSON
# ---------------------------------------------------------------------------


_REFERENCES_BLOCK_RE = re.compile(
    r"```content-block:references\s*\n(?P<json>.*?)\n```",
    re.DOTALL | re.IGNORECASE,
)


@dataclass
class ReferencesParseResult:
    entries: List[Dict[str, Any]]  # validated (partially) dicts, ready for upsert
    block_found: bool
    errors: List[str]  # human-readable warnings; non-empty -> treat as malformed


def parse_references_block(response_text: str) -> ReferencesParseResult:
    """Extract structured references from the writer's response.

    Returns ReferencesParseResult. Callers treat a non-empty `errors`
    list as a signal to fall back to the legacy inline-Markdown path
    (the agent still emits a Markdown Literaturverzeichnis in parallel,
    so nothing is lost).
    """
    match = _REFERENCES_BLOCK_RE.search(response_text or "")
    if match is None:
        return ReferencesParseResult(entries=[], block_found=False, errors=[])

    raw = match.group("json").strip()
    if not raw:
        return ReferencesParseResult(
            entries=[], block_found=True, errors=["references block is empty"]
        )

    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError as exc:
        return ReferencesParseResult(
            entries=[], block_found=True, errors=[f"malformed JSON: {exc}"]
        )

    if not isinstance(parsed, list):
        return ReferencesParseResult(
            entries=[], block_found=True, errors=["references payload must be a JSON array"]
        )

    entries: List[Dict[str, Any]] = []
    errors: List[str] = []
    seen_keys: set[str] = set()
    for i, item in enumerate(parsed):
        if not isinstance(item, dict):
            errors.append(f"entry {i}: not an object")
            continue
        entry_key = (item.get("entry_key") or "").strip()
        title = (item.get("title") or "").strip()
        if not entry_key:
            errors.append(f"entry {i}: missing entry_key")
            continue
        if not title:
            errors.append(f"entry {i} ({entry_key}): missing title")
            continue
        if entry_key in seen_keys:
            errors.append(f"duplicate entry_key: {entry_key}")
            continue
        # Structural minimum: at least one of url / container_title / publisher / document_id
        if not any(item.get(k) for k in ("url", "container_title", "publisher", "document_id")):
            errors.append(
                f"entry {entry_key}: needs at least one of url, container_title, publisher, document_id"
            )
            continue

        seen_keys.add(entry_key)
        entries.append({
            "entry_key": entry_key,
            "authors": item.get("authors") or [],
            "year": item.get("year"),
            "title": title,
            "container_title": item.get("container_title"),
            "publisher": item.get("publisher"),
            "pages": item.get("pages"),
            "url": item.get("url"),
            "doi": item.get("doi"),
            "accessed_at": item.get("accessed_at"),
            "reference_type": item.get("reference_type", "web"),
            "document_id": item.get("document_id"),
        })

    return ReferencesParseResult(entries=entries, block_found=True, errors=errors)


def count_occurrences_by_reference(
    db: Session, draft_id: str
) -> Dict[str, int]:
    """Return {reference_id: occurrence_count} for live orphan/dead-entry diagnostics."""
    from sqlalchemy import func as sa_func

    rows = (
        db.query(
            models.CitationEntry.reference_id,
            sa_func.count(models.CitationEntry.id),
        )
        .filter(models.CitationEntry.draft_id == draft_id)
        .group_by(models.CitationEntry.reference_id)
        .all()
    )
    return {r[0]: int(r[1]) for r in rows}
