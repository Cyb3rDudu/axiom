"""Deliverable planner pre-pass for writing sessions.

The continuation detector previously inferred ``expected_sections`` from
the headings the writer actually emitted. A writer that stopped at
section 3 of an intended 5 looked complete (3/3) and no continuation
fired; the user got a truncated draft.

This module runs one cheap LLM call BEFORE the main writer turn and
produces a structured plan: how many sections, in what order, with
what target word counts, in what language, with how many references,
with figures or not. The pipeline + continuation logic consume the
plan as ground truth instead of inferring intent from observed output.

The plan is persisted on ``WritingSession.settings.plan`` so subsequent
revision turns in the same session reuse it without paying the LLM
cost again. Re-planning happens only when the user issues a new
top-level prompt (not on the revision turns).

Feature-flagged via ``writing.deliverable_planner.enabled``; off by
default until dogfooded. When off, all consumers fall back to the
existing observed-output heuristics — nothing breaks.
"""

from __future__ import annotations

import json
import logging
import re
from dataclasses import asdict, dataclass, field
from typing import Any, Dict, List, Mapping, Optional, Tuple

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class PlanSection:
    """One numbered section in the planned deliverable."""

    index: int
    title: str
    target_words: int


@dataclass(frozen=True)
class DeliverablePlan:
    """Frozen plan produced by the planner pre-pass.

    The plan is the single source of truth for "what was the user
    asking for?" — section count, word budget, language, references
    target, figure intent. Stored on ``WritingSession.settings.plan``
    after the first writer turn so revision turns reuse it.
    """

    sections: Tuple[PlanSection, ...]
    total_word_budget: Tuple[int, int]
    language_code: str
    reference_target_count: Tuple[int, int]
    has_figures: bool

    def to_dict(self) -> Dict[str, Any]:
        """JSON-friendly representation for persistence."""
        return {
            "sections": [asdict(s) for s in self.sections],
            "total_word_budget": list(self.total_word_budget),
            "language_code": self.language_code,
            "reference_target_count": list(self.reference_target_count),
            "has_figures": self.has_figures,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "DeliverablePlan":
        sections = tuple(
            PlanSection(
                index=int(s["index"]),
                title=str(s["title"]),
                target_words=int(s["target_words"]),
            )
            for s in data.get("sections", [])
        )
        budget = data.get("total_word_budget") or (0, 0)
        ref_count = data.get("reference_target_count") or (0, 0)
        return cls(
            sections=sections,
            total_word_budget=(int(budget[0]), int(budget[1])),
            language_code=str(data.get("language_code") or "en"),
            reference_target_count=(int(ref_count[0]), int(ref_count[1])),
            has_figures=bool(data.get("has_figures")),
        )

    @property
    def expected_sections(self) -> int:
        """Section count the continuation detector treats as ground truth."""
        return len(self.sections)

    @property
    def section_budgets(self) -> Dict[int, int]:
        """{section_index: target_words} for the continuation helper."""
        return {s.index: s.target_words for s in self.sections}


_PLANNER_SYSTEM_PROMPT = (
    "You are a deliverable planner for a long-form writing assistant. "
    "Given a user prompt and an optional existing draft, produce a "
    "STRUCTURED PLAN as JSON describing what the writer should produce.\n\n"
    "Output JSON shape (return EXACTLY this structure, no markdown):\n"
    "{\n"
    '  "sections": [\n'
    '    {"index": 1, "title": "Einleitung", "target_words": 400},\n'
    '    {"index": 2, "title": "Hauptteil", "target_words": 1200}\n'
    "  ],\n"
    '  "total_word_budget": [min, max],\n'
    '  "language_code": "de" | "en",\n'
    '  "reference_target_count": [min, max],\n'
    '  "has_figures": true | false\n'
    "}\n\n"
    "Rules:\n"
    "1. Sections: numbered starting at 1. Use the deliverable type the\n"
    "   user described (academic paper → Einleitung/Hauptteil/Schluss\n"
    "   in German, Introduction/Body/Conclusion in English; market-\n"
    "   research report → Executive Summary + 5–8 sections; etc.).\n"
    "2. target_words: realistic budget per section. Sum should match\n"
    "   total_word_budget center if the user named one.\n"
    "3. language_code: detect from the prompt + draft. Default 'en' if\n"
    "   ambiguous. Only 'de' or 'en' supported.\n"
    "4. reference_target_count: realistic for the deliverable type and\n"
    "   word budget. KMU Hausarbeit ≈ 12–18 refs per 3000 words.\n"
    "5. has_figures: true ONLY when the user prompt explicitly asks\n"
    "   for figures, charts, diagrams or the draft already shows\n"
    "   figure markdown.\n\n"
    "If the user prompt is a quick edit / single-paragraph request,\n"
    "return a 1-section plan with realistic small budgets — do NOT\n"
    "fabricate a multi-section structure for short prompts.\n"
)


def _build_planner_user_prompt(
    *, user_prompt: str, existing_draft_body: str
) -> str:
    """Compose the user message the planner LLM sees.

    Caps the existing draft to a head + tail excerpt so a 30k-char
    revision doesn't burn the planner budget. The planner doesn't need
    to read the whole body; the prompt + the structural cues from
    head/tail are enough.
    """
    parts = [f"USER PROMPT:\n{user_prompt or '(empty)'}"]
    body = (existing_draft_body or "").strip()
    if body:
        head = body[:1500]
        tail = body[-1500:] if len(body) > 3000 else ""
        excerpt = head + ("\n\n[...]\n\n" + tail if tail else "")
        parts.append(f"EXISTING DRAFT (excerpt):\n{excerpt}")
    parts.append(
        "Output ONLY the plan JSON. No prose, no markdown fences."
    )
    return "\n\n".join(parts)


def _strip_code_fences(text: str) -> str:
    """Remove leading/trailing ```json / ``` fences if the LLM emitted them."""
    text = text.strip()
    fence_match = re.match(
        r"^```(?:json)?\s*\n(.*?)\n```\s*$", text, re.DOTALL
    )
    if fence_match:
        return fence_match.group(1).strip()
    return text


def parse_plan_response(raw_text: str) -> Optional[DeliverablePlan]:
    """Parse a planner LLM response into a DeliverablePlan.

    Returns None on any structural failure — the caller treats that as
    "planner unavailable" and falls back to the legacy detector.
    """
    if not raw_text:
        return None
    cleaned = _strip_code_fences(raw_text)
    try:
        data = json.loads(cleaned)
    except json.JSONDecodeError as exc:
        logger.warning("planner: malformed JSON: %s", exc)
        return None
    if not isinstance(data, dict):
        return None
    sections_raw = data.get("sections")
    if not isinstance(sections_raw, list) or not sections_raw:
        return None
    try:
        return DeliverablePlan.from_dict(data)
    except (KeyError, TypeError, ValueError) as exc:
        logger.warning("planner: plan validation failed: %s", exc)
        return None


async def plan_deliverable(
    *,
    prompt: str,
    existing_draft_body: str,
    dispatcher: Any,
    agent_mode: str = "writing_planner",
) -> Optional[DeliverablePlan]:
    """Run the planner LLM call and return a structured plan.

    Returns None when the dispatcher fails or the response is
    malformed. The caller treats None as "no plan available" and falls
    back to the observed-output heuristics — never raises.

    The dispatcher must accept a ``messages`` list and ``response_format``;
    the same signature ``ModelDispatcher.dispatch`` already exposes.
    """
    if not prompt or not prompt.strip():
        return None

    messages = [
        {"role": "system", "content": _PLANNER_SYSTEM_PROMPT},
        {
            "role": "user",
            "content": _build_planner_user_prompt(
                user_prompt=prompt, existing_draft_body=existing_draft_body
            ),
        },
    ]

    try:
        response, _details = await dispatcher.dispatch(
            messages=messages,
            agent_mode=agent_mode,
            response_format={"type": "json_object"},
        )
    except Exception as exc:  # noqa: BLE001
        logger.warning("planner: dispatch failed: %s", exc)
        return None

    if response is None or not getattr(response, "choices", None):
        return None
    raw = response.choices[0].message.content or ""
    plan = parse_plan_response(raw)
    if plan is None:
        logger.warning("planner: response could not be parsed into a plan")
    else:
        logger.info(
            "planner: %d sections, budget=%s, language=%s, refs=%s, figures=%s",
            plan.expected_sections,
            plan.total_word_budget,
            plan.language_code,
            plan.reference_target_count,
            plan.has_figures,
        )
    return plan


# ---------------------------------------------------------------------------
# Persistence helpers — round-trip through WritingSession.settings.plan
# ---------------------------------------------------------------------------


_PLAN_KEY = "plan"


def load_plan_from_session(session_settings: Any) -> Optional[DeliverablePlan]:
    """Load a previously-persisted plan from session settings."""
    if not isinstance(session_settings, Mapping):
        return None
    raw = session_settings.get(_PLAN_KEY)
    if not isinstance(raw, Mapping):
        return None
    try:
        return DeliverablePlan.from_dict(raw)
    except (KeyError, TypeError, ValueError):
        return None


def serialise_plan_to_session(
    session_settings: Optional[Mapping[str, Any]], plan: DeliverablePlan
) -> Dict[str, Any]:
    """Return a new settings dict with ``plan`` replaced.

    Pure function — does not mutate the input. The caller assigns the
    return value back to ``writing_session.settings``.
    """
    base = dict(session_settings or {})
    base[_PLAN_KEY] = plan.to_dict()
    return base
