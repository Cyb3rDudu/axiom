"""Feature flag resolution helpers.

Minimal two-layer flag system used by the writing-mode rollouts (#58
and onwards): per-user opt-in in user.settings.writing_settings, plus
an env-level kill switch so ops can disable a path globally.

Resolution order (first wins):
    1. env kill switch = "false" (explicit off) → False, no matter what
    2. env kill switch = "true"  → per-user opt-in respected
    3. env unset                 → per-user opt-in respected (default off)

The env switch is intentionally opt-in: staying dark for users who
haven't explicitly enabled the flag in their settings, even if the env
is flipped on globally. Ops uses the env switch only to force-disable.
"""

from __future__ import annotations

import os
from typing import Any, Mapping, Optional


_TRUTHY = {"1", "true", "yes", "on"}
_FALSY = {"0", "false", "no", "off"}


def _env_bool(name: str) -> Optional[bool]:
    raw = os.getenv(name)
    if raw is None:
        return None
    lowered = raw.strip().lower()
    if lowered in _TRUTHY:
        return True
    if lowered in _FALSY:
        return False
    return None


def _nested_get(data: Any, *keys: str) -> Any:
    cur = data
    for key in keys:
        if not isinstance(cur, Mapping):
            return None
        cur = cur.get(key)
    return cur


# ---------------------------------------------------------------------------
# Structured bibliography flag (#51/#58)
# ---------------------------------------------------------------------------

STRUCTURED_BIBLIOGRAPHY_ENV = "WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED"
STRUCTURED_BIBLIOGRAPHY_SETTING = (
    "writing_settings",
    "structured_bibliography_enabled",
)


def structured_bibliography_enabled(user_settings: Any) -> bool:
    """Is the structured-bibliography path on for this user?

    Accepts a dict (user.settings) or None. Resolution:
    - env kill switch explicitly false → off
    - user flag true  → on
    - anything else   → off (default closed)
    """
    env_flag = _env_bool(STRUCTURED_BIBLIOGRAPHY_ENV)
    if env_flag is False:
        return False

    per_user = _nested_get(user_settings, *STRUCTURED_BIBLIOGRAPHY_SETTING)
    return bool(per_user)


def resolve_flag_for_logging(user_settings: Any) -> dict:
    """Expose both inputs so we can log the flag decision per session."""
    return {
        "env": _env_bool(STRUCTURED_BIBLIOGRAPHY_ENV),
        "user": bool(_nested_get(user_settings, *STRUCTURED_BIBLIOGRAPHY_SETTING)),
        "resolved": structured_bibliography_enabled(user_settings),
    }


# ---------------------------------------------------------------------------
# Writing Completeness Contract (docs/plans/WRITING_COMPLETENESS_CONTRACT.md)
# ---------------------------------------------------------------------------
#
# Four sub-flags gated by one env kill switch + per-user opt-in. The
# stages can be rolled in independently so a single bad path doesn't
# force a full rollback.
#
# Resolution mirrors structured_bibliography_enabled:
# - env kill switch false → all four stages off
# - env unset OR true → per-user settings.writing.completeness.*
#                        (each independently defaults off)

COMPLETENESS_ENV = "WRITING_COMPLETENESS_CONTRACT_ENABLED"


def _completeness_flag(user_settings: Any, sub_key: str) -> bool:
    """Resolve writing.completeness.<sub_key> with env kill-switch."""
    env_flag = _env_bool(COMPLETENESS_ENV)
    if env_flag is False:
        return False
    val = _nested_get(user_settings, "writing_settings", "completeness", sub_key)
    return bool(val)


def wordcount_fix_enabled(user_settings: Any) -> bool:
    """Stage 1a — deterministic Wortbilanz recompute overrides the LLM's
    hallucinated word count in the response before persist."""
    return _completeness_flag(user_settings, "wordcount_fix")


def sources_always_enabled(user_settings: Any) -> bool:
    """Stage 1b — backend always emits a canonical references block
    based on the current registry, regardless of what the LLM produced
    this turn."""
    return _completeness_flag(user_settings, "sources_always")


def transparent_continuation_enabled(user_settings: Any) -> bool:
    """Stage 2 — backend detects truncation, fires section-scoped
    continuation calls, stitches deterministically. User sees a single
    coherent draft regardless of token budget hit."""
    return _completeness_flag(user_settings, "transparent_continuation")


def rag_figures_enabled(user_settings: Any) -> bool:
    """Stage 3 — backend pre-fetches figure candidates from
    document_images before the writer call when the prompt carries
    figure-intent signals."""
    return _completeness_flag(user_settings, "rag_figures")


def deliverable_planner_enabled(user_settings: Any) -> bool:
    """One cheap planner LLM call before the main writer turn that
    decides expected section count, language, word budget, references
    target, and figure intent. Subsequent revision turns reuse the
    persisted plan from WritingSession.settings."""
    return _completeness_flag(user_settings, "deliverable_planner")


def resolve_completeness_flags(user_settings: Any) -> dict:
    """One-shot resolver for structured telemetry / debug logs."""
    return {
        "env": _env_bool(COMPLETENESS_ENV),
        "wordcount_fix": wordcount_fix_enabled(user_settings),
        "sources_always": sources_always_enabled(user_settings),
        "transparent_continuation": transparent_continuation_enabled(user_settings),
        "rag_figures": rag_figures_enabled(user_settings),
        "deliverable_planner": deliverable_planner_enabled(user_settings),
    }
