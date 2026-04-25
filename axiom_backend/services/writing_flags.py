"""Resolved feature-flag state for a single writing-chat request.

Prior state: a dozen scattered ``xxx_enabled(current_user.settings)``
call sites in api/writing.py, each re-resolving the same flag from
the same settings blob. Easy to drift (one site uses ``user.settings``,
another ``current_user.settings``); easy to add a new flag and forget
to wire it into one of the consumers.

This module resolves the full flag set once per request and freezes it
into a dataclass passed down through the pipeline. Adding a new flag
becomes a one-line change here; consumers stay agnostic of the
underlying ``feature_flags.py`` helpers.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping, Optional


@dataclass(frozen=True)
class WritingFlags:
    """Frozen snapshot of writing-subsystem feature flags for one request.

    Resolution happens once at request entry via ``WritingFlags.resolve``.
    The pipeline and any downstream stage consume the snapshot rather
    than re-querying ``feature_flags`` helpers, so a flag flip mid-request
    can't produce inconsistent behaviour across stages.
    """

    structured_bibliography: bool = False
    wordcount_fix: bool = False
    sources_always: bool = False
    transparent_continuation: bool = False
    rag_figures: bool = False

    @classmethod
    def resolve(cls, user_settings: Optional[Mapping[str, Any]]) -> "WritingFlags":
        """Resolve all flags from the user's settings blob.

        Imports happen lazily so this module stays import-cheap for
        tests that stub the feature_flags layer.
        """
        from services.feature_flags import (
            rag_figures_enabled,
            sources_always_enabled,
            structured_bibliography_enabled,
            transparent_continuation_enabled,
            wordcount_fix_enabled,
        )

        settings = user_settings or {}
        return cls(
            structured_bibliography=structured_bibliography_enabled(settings),
            wordcount_fix=wordcount_fix_enabled(settings),
            sources_always=sources_always_enabled(settings),
            transparent_continuation=transparent_continuation_enabled(settings),
            rag_figures=rag_figures_enabled(settings),
        )

    @classmethod
    def all_off(cls) -> "WritingFlags":
        """Convenience for tests + planner-disabled paths."""
        return cls()
