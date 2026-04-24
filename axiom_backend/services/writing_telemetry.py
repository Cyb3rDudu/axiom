"""Structured telemetry for the writing subsystem (#74).

No dedicated metrics backend is wired in axiom today, so counters ride
on the stdlib logger via JSON-shaped `extra` fields. Downstream log
shippers (promtail / filebeat / vector) aggregate on the `metric`,
`subsystem`, and labelled fields. When a proper prometheus client
lands, `record_*` helpers stay as the public API and pick up a real
Counter/Histogram under the hood.

Log line shape (example):
    logger.info("writing.metric", extra={
        "subsystem": "writing_portfolio",
        "metric": "writing_portfolio_generations_total",
        "trigger": "manual",
        "outcome": "generated",
        "traffic_light": "green",
        "draft_id": "…",
        "user_id": 4,
    })

Rules:
- Every log from the writing subsystem should carry `subsystem`.
- Counter-style events use the `writing.metric` message + `metric` key.
- Free-form traces use their own messages but still pass `subsystem`.
"""

from __future__ import annotations

import logging
from typing import Any, Dict, Literal, Optional

logger = logging.getLogger("axiom.writing_telemetry")


Subsystem = Literal[
    "writing_chat",         # chat-task lifecycle
    "writing_portfolio",    # portfolio manager runs
    "writing_bibliography", # structured-ref parse / persist / migration
    "writing_sync",         # in-text citation sync validator
    "writing_export",       # DOCX export
]


# ---------------------------------------------------------------------------
# Flag logging
# ---------------------------------------------------------------------------


def log_flag_state(
    *,
    subsystem: Subsystem,
    user_settings: Any,
    draft_id: Optional[str] = None,
    user_id: Optional[int] = None,
    session_id: Optional[str] = None,
) -> None:
    """Emit a structured record for each writing-mode session start.

    Dedicated helper so the flag-resolution triple (env / user / resolved)
    ships as a structured field rather than a free-form log line, making
    it trivial to grep for flag-off users who ended up with structured
    state.
    """
    from services.feature_flags import resolve_flag_for_logging

    resolution = resolve_flag_for_logging(user_settings)
    logger.info(
        "writing.flag_state",
        extra={
            "subsystem": subsystem,
            "metric": "writing_flag_state",
            "flag_name": "structured_bibliography_enabled",
            "flag_env": resolution["env"],
            "flag_user": resolution["user"],
            "flag_resolved": resolution["resolved"],
            "draft_id": draft_id,
            "user_id": user_id,
            "session_id": session_id,
        },
    )


# ---------------------------------------------------------------------------
# Counter helpers
# ---------------------------------------------------------------------------


def _bucket_count(n: int) -> str:
    """Bucket a non-negative count into 0 / 1–2 / 3+."""
    if n <= 0:
        return "0"
    if n <= 2:
        return "1-2"
    return "3+"


def record_portfolio_generation(
    *,
    trigger: Literal["manual", "session_close"],
    outcome: Literal["generated", "skipped_flag", "skipped_optout", "skipped_empty", "error"],
    traffic_light: Optional[Literal["green", "yellow", "red"]] = None,
    draft_id: Optional[str] = None,
    user_id: Optional[int] = None,
    source_count: Optional[int] = None,
) -> None:
    logger.info(
        "writing.metric",
        extra={
            "subsystem": "writing_portfolio",
            "metric": "writing_portfolio_generations_total",
            "trigger": trigger,
            "outcome": outcome,
            "traffic_light": traffic_light,
            "draft_id": draft_id,
            "user_id": user_id,
            "source_count": source_count,
        },
    )


def record_bibliography_parse(
    *,
    result: Literal["no_block", "parsed", "malformed", "empty_valid"],
    entries_count: int = 0,
    errors_count: int = 0,
    draft_id: Optional[str] = None,
    user_id: Optional[int] = None,
) -> None:
    logger.info(
        "writing.metric",
        extra={
            "subsystem": "writing_bibliography",
            "metric": "structured_bibliography_parse_total",
            "result": result,
            "entries_count": entries_count,
            "errors_count": errors_count,
            "draft_id": draft_id,
            "user_id": user_id,
        },
    )


def record_sync_report(
    *,
    resolved_count: int,
    orphan_count: int,
    dead_count: int,
    ambiguous_count: int = 0,
    draft_id: Optional[str] = None,
    user_id: Optional[int] = None,
) -> None:
    logger.info(
        "writing.metric",
        extra={
            "subsystem": "writing_sync",
            "metric": "citation_sync_report_total",
            "resolved_count": resolved_count,
            "orphan_count": orphan_count,
            "dead_count": dead_count,
            "ambiguous_count": ambiguous_count,
            "orphans_bucket": _bucket_count(orphan_count),
            "dead_bucket": _bucket_count(dead_count),
            "draft_id": draft_id,
            "user_id": user_id,
        },
    )


def record_docx_export(
    *,
    bibliography_source: Literal["inline", "structured", "both_stripped", "none"],
    portfolio_source: Literal["inline", "structured", "both_stripped", "none"],
    draft_id: Optional[str] = None,
    user_id: Optional[int] = None,
    markdown_size: Optional[int] = None,
) -> None:
    logger.info(
        "writing.metric",
        extra={
            "subsystem": "writing_export",
            "metric": "docx_export_total",
            "bibliography_source": bibliography_source,
            "portfolio_source": portfolio_source,
            "draft_id": draft_id,
            "user_id": user_id,
            "markdown_size": markdown_size,
        },
    )


__all__ = [
    "log_flag_state",
    "record_portfolio_generation",
    "record_bibliography_parse",
    "record_sync_report",
    "record_docx_export",
]
