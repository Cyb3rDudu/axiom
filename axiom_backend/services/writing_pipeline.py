"""Post-agent response pipeline for writing chat.

Prior state: the writing-chat background task in api/writing.py inlined
~250 lines of post-agent processing — bibliography ingest, citation
sync, audit, three-pass completeness post-process, persistence,
WebSocket dispatch — wedged together with all the upstream context
resolution. A bug in one stage (e.g. wordcount recompute) was
indistinguishable from a bug in another (e.g. fence-balance audit) at
review time.

This module isolates the post-agent flow into a single async function
that takes the raw agent response plus a frozen ``PipelineContext``
and returns a ``PipelineResult``. The chat task becomes a thin caller
that resolves context, invokes the agent, then runs the pipeline.

Each stage is independently flag-gated (see WritingFlags) and
non-fatal — exceptions are logged and the pipeline keeps running so a
single misbehaving post-process can't crater an otherwise-good
response.
"""

from __future__ import annotations

import logging
import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Awaitable, Callable, Dict, List, Optional

from sqlalchemy.orm import Session

from database import models
from services.writing_flags import WritingFlags

logger = logging.getLogger(__name__)


@dataclass
class PipelineContext:
    """Frozen-per-request inputs to the response pipeline.

    All fields are read-only from the pipeline's perspective. The
    pipeline mutates the database (persisting message + structured
    refs) but never the context dict.
    """

    db: Session
    draft: models.Draft
    chat_id: str
    session_id: str
    user_id: int
    task_id: str
    flags: WritingFlags
    citation_profile: Optional[Any] = None
    figure_resolution: Optional[Dict[str, Any]] = None


@dataclass
class PipelineResult:
    """Structured outcome of running the response pipeline.

    The pipeline returns this once after all stages complete. The
    caller's job is to ship it over WebSocket and into chat history.
    """

    final_response_text: str
    persisted_message_id: str
    audit_dict: Optional[Dict[str, Any]] = None
    structured_refs_summary: Optional[Dict[str, Any]] = None
    citation_sync_dict: Optional[Dict[str, Any]] = None
    completeness_telemetry: Dict[str, Any] = field(default_factory=dict)
    websocket_payload: Dict[str, Any] = field(default_factory=dict)


async def run_response_pipeline(
    *,
    raw_response: str,
    sources: List[Any],
    context: PipelineContext,
) -> PipelineResult:
    """Run the post-agent processing chain.

    Stages, in order:

      1. Structured bibliography ingest (flag-gated)
      2. Audit (always)
      3. Citation sync (flag-gated)
      4. Wordcount recompute (flag-gated)
      5. Sources block synthesis (flag-gated)
      6. Figure URL validation (flag-gated)
      7. Persist assistant message
      8. Build WebSocket payload (caller dispatches)

    Each flag-gated stage is wrapped in a try/except so a single failure
    doesn't propagate. Telemetry from each stage lands on the
    ``PipelineResult`` so callers can log it once at the end.
    """
    final_response_text = raw_response or ""
    structured_refs_summary: Optional[Dict[str, Any]] = None
    citation_sync_dict: Optional[Dict[str, Any]] = None
    completeness_telemetry: Dict[str, Any] = {}

    # ---- Structured bibliography ingest ------------------------------------
    if context.flags.structured_bibliography:
        structured_refs_summary = await _ingest_structured_bibliography(
            final_response_text, context
        )

    # ---- Audit (always) ----------------------------------------------------
    audit_dict: Optional[Dict[str, Any]] = None
    try:
        from services.writing_response_audit import audit_writing_response
        audit = audit_writing_response(final_response_text)
        if audit.has_warnings:
            logger.warning(
                "Writing response audit warnings for task %s: "
                "url_in_parens=%d unbalanced_fences=%s wordcount_delta_pct=%s",
                context.task_id,
                len(audit.url_in_parens),
                audit.unbalanced_fences,
                audit.wordcount_delta_pct,
            )
            audit_dict = audit.to_dict()
    except Exception as exc:  # noqa: BLE001
        logger.warning("audit failed (non-fatal): %s", exc, exc_info=True)

    # ---- Citation sync -----------------------------------------------------
    if context.flags.structured_bibliography:
        citation_sync_dict = await _run_citation_sync(final_response_text, context)

    # ---- Completeness post-process (3 passes) ------------------------------
    final_response_text, completeness_telemetry = _run_completeness_passes(
        final_response_text, context
    )

    # ---- Persist assistant message -----------------------------------------
    message_id = _persist_assistant_message(
        final_response_text, sources, context
    )

    # ---- Compose WebSocket payload -----------------------------------------
    payload: Dict[str, Any] = {
        "message": final_response_text,
        "sources": sources or [],
        "task_id": context.task_id,
        "audit": audit_dict,
        "structured_references": structured_refs_summary,
        "citation_sync": citation_sync_dict,
        "completeness": completeness_telemetry or None,
    }

    return PipelineResult(
        final_response_text=final_response_text,
        persisted_message_id=message_id,
        audit_dict=audit_dict,
        structured_refs_summary=structured_refs_summary,
        citation_sync_dict=citation_sync_dict,
        completeness_telemetry=completeness_telemetry,
        websocket_payload=payload,
    )


# ---------------------------------------------------------------------------
# Stage implementations (private)
# ---------------------------------------------------------------------------


async def _ingest_structured_bibliography(
    response_text: str, context: PipelineContext
) -> Optional[Dict[str, Any]]:
    """Parse + persist content-block:references entries for the draft.

    Non-fatal: a parse error logs a warning and returns None — the
    legacy inline-Markdown path stays as fallback.
    """
    try:
        from services.citation_rendering import render_entry
        from services.structured_bibliography import (
            StructuredBibliographyService,
            parse_references_block,
        )
        from services.writing_telemetry import record_bibliography_parse

        parse_result = parse_references_block(response_text)
        if not parse_result.block_found:
            record_bibliography_parse(
                result="no_block",
                draft_id=context.draft.id,
                user_id=context.user_id,
            )
            return None

        if parse_result.errors:
            logger.warning(
                "Structured references block malformed for task %s: %s",
                context.task_id,
                parse_result.errors,
            )

        if not parse_result.entries:
            record_bibliography_parse(
                result="malformed" if parse_result.errors else "empty_valid",
                entries_count=0,
                errors_count=len(parse_result.errors),
                draft_id=context.draft.id,
                user_id=context.user_id,
            )
            return None

        service = StructuredBibliographyService(context.db)
        pid = (
            context.citation_profile.id
            if context.citation_profile is not None
            else "numbered"
        )
        rendered_refs = service.replace_draft_registry(
            draft_id=context.draft.id,
            entries=parse_result.entries,
            user_id=context.user_id,
            render_citation=lambda e, _pid=pid: render_entry(e, _pid),
        )
        logger.info(
            "Persisted %d structured references for draft %s (task %s)",
            len(rendered_refs),
            context.draft.id,
            context.task_id,
        )
        record_bibliography_parse(
            result="parsed" if not parse_result.errors else "malformed",
            entries_count=len(parse_result.entries),
            errors_count=len(parse_result.errors),
            draft_id=context.draft.id,
            user_id=context.user_id,
        )
        return {
            "count": len(rendered_refs),
            "errors": parse_result.errors,
        }
    except Exception as exc:  # noqa: BLE001
        logger.exception(
            "Structured references ingest failed for task %s: %s",
            context.task_id,
            exc,
        )
        return None


async def _run_citation_sync(
    response_text: str, context: PipelineContext
) -> Optional[Dict[str, Any]]:
    """Validate in-text citations against the freshly-persisted registry.

    Persists resolved markers into citation_entries so the portfolio
    adapter can read per-occurrence offsets without re-parsing.
    """
    try:
        from services.citation_sync import strip_references_block, validate_citations
        from services.structured_bibliography import record_citation_occurrences
        from services.writing_telemetry import record_sync_report

        body_for_sync = strip_references_block(response_text)
        registry_rows = (
            context.db.query(models.Reference)
            .filter(
                models.Reference.draft_id == context.draft.id,
                models.Reference.entry_key.isnot(None),
            )
            .all()
        )
        registry_dicts = [
            {
                "entry_key": r.entry_key,
                "authors": r.authors,
                "year": r.year,
                "publisher": r.publisher,
                "container_title": r.container_title,
            }
            for r in registry_rows
        ]
        sync_report = validate_citations(body_for_sync, registry_dicts)
        if sync_report.has_warnings:
            logger.warning(
                "Citation sync warnings for task %s: orphans=%d dead=%d ambiguous=%d",
                context.task_id,
                len(sync_report.orphan_markers),
                len(sync_report.dead_entries),
                len(sync_report.ambiguous),
            )
        record_sync_report(
            resolved_count=len(sync_report.resolved),
            orphan_count=len(sync_report.orphan_markers),
            dead_count=len(sync_report.dead_entries),
            ambiguous_count=len(sync_report.ambiguous),
            draft_id=context.draft.id,
            user_id=context.user_id,
        )

        # Persist resolved markers as citation_entries rows so the portfolio
        # adapter and live-sync UI can read per-occurrence offsets without
        # re-parsing the body.
        ref_id_by_key = {
            r.entry_key: r.id for r in registry_rows if r.entry_key
        }
        occurrences = []
        for marker, entry_key in sync_report.resolved:
            ref_id = ref_id_by_key.get(entry_key)
            if ref_id is None:
                continue
            occurrences.append({
                "reference_id": ref_id,
                "in_text_marker": marker.marker,
                "paragraph_index": marker.paragraph_index,
                "char_offset_start": marker.char_offset_start,
                "char_offset_end": marker.char_offset_end,
            })
        record_citation_occurrences(context.db, context.draft.id, occurrences)

        return sync_report.to_dict()
    except Exception as exc:  # noqa: BLE001
        logger.exception(
            "Citation sync failed for task %s: %s", context.task_id, exc
        )
        return None


def _run_completeness_passes(
    response_text: str, context: PipelineContext
) -> tuple[str, Dict[str, Any]]:
    """Run wordcount / sources / figure-validation passes.

    All three are independently flag-gated and idempotent. Mutates the
    response text in place; the returned tuple carries the final text
    plus a per-pass telemetry dict for the WebSocket payload.
    """
    text = response_text
    telemetry: Dict[str, Any] = {}

    try:
        if context.flags.wordcount_fix:
            from services.writing_response_audit import recompute_wortbilanz
            text, wc_tele = recompute_wortbilanz(text)
            if wc_tele:
                telemetry["wordcount_fix"] = wc_tele

        if context.flags.sources_always:
            from services.response_postprocess import synthesize_sources_block
            registry_for_sources = (
                context.db.query(models.Reference)
                .filter(
                    models.Reference.draft_id == context.draft.id,
                    models.Reference.entry_key.isnot(None),
                )
                .all()
            )
            text, sources_tele = synthesize_sources_block(text, registry_for_sources)
            telemetry["sources_always"] = sources_tele

        if context.flags.rag_figures and context.figure_resolution:
            from services.response_postprocess import validate_figure_urls
            valid_urls = context.figure_resolution.get("valid_image_urls") or set()
            text, fig_tele = validate_figure_urls(
                text, valid_urls if valid_urls else None
            )
            telemetry["figures_validated"] = fig_tele
    except Exception as exc:  # noqa: BLE001
        logger.warning(
            "Writing completeness post-process failed (non-fatal): %s",
            exc,
            exc_info=True,
        )

    if telemetry:
        logger.info(
            "Writing completeness telemetry for task %s: %s",
            context.task_id,
            {k: v for k, v in telemetry.items() if v},
        )

    return text, telemetry


def _persist_assistant_message(
    response_text: str, sources: List[Any], context: PipelineContext
) -> str:
    """Persist the assistant response to chat history.

    The post-processed text is what gets stored — wordcount-fixed,
    sources-synthesised, figure-validated — so the DB matches what the
    user sees in the chat bubble.
    """
    msg = models.Message(
        id=str(uuid.uuid4()),
        chat_id=context.chat_id,
        role="assistant",
        content=response_text,
        sources=sources or [],
        created_at=datetime.utcnow(),
    )
    context.db.add(msg)
    context.db.commit()
    return msg.id
