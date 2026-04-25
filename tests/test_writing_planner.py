"""Tests for writing_planner.py — deliverable planner pre-pass."""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any, List

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.writing_planner import (  # noqa: E402
    DeliverablePlan,
    PlanSection,
    load_plan_from_session,
    parse_plan_response,
    plan_deliverable,
    serialise_plan_to_session,
)


_SAMPLE_PLAN_JSON = {
    "sections": [
        {"index": 1, "title": "Einleitung", "target_words": 400},
        {"index": 2, "title": "Hauptteil", "target_words": 1200},
        {"index": 3, "title": "Schluss", "target_words": 400},
    ],
    "total_word_budget": [1800, 2200],
    "language_code": "de",
    "reference_target_count": [10, 15],
    "has_figures": True,
}


class TestPlanFromDict:
    def test_round_trip(self):
        plan = DeliverablePlan.from_dict(_SAMPLE_PLAN_JSON)
        assert plan.expected_sections == 3
        assert plan.section_budgets == {1: 400, 2: 1200, 3: 400}
        assert plan.total_word_budget == (1800, 2200)
        assert plan.language_code == "de"
        assert plan.reference_target_count == (10, 15)
        assert plan.has_figures is True

    def test_to_dict_round_trip(self):
        plan = DeliverablePlan.from_dict(_SAMPLE_PLAN_JSON)
        roundtrip = DeliverablePlan.from_dict(plan.to_dict())
        assert roundtrip == plan


class TestParsePlanResponse:
    def test_parses_clean_json(self):
        plan = parse_plan_response(json.dumps(_SAMPLE_PLAN_JSON))
        assert plan is not None
        assert plan.expected_sections == 3

    def test_parses_json_inside_markdown_fence(self):
        wrapped = f"```json\n{json.dumps(_SAMPLE_PLAN_JSON)}\n```"
        plan = parse_plan_response(wrapped)
        assert plan is not None
        assert plan.language_code == "de"

    def test_returns_none_on_malformed_json(self):
        assert parse_plan_response("{this is not json") is None

    def test_returns_none_on_empty_input(self):
        assert parse_plan_response("") is None
        assert parse_plan_response(None) is None  # type: ignore[arg-type]

    def test_returns_none_when_sections_missing(self):
        bad = dict(_SAMPLE_PLAN_JSON)
        bad.pop("sections")
        assert parse_plan_response(json.dumps(bad)) is None

    def test_returns_none_when_sections_empty(self):
        bad = dict(_SAMPLE_PLAN_JSON)
        bad["sections"] = []
        assert parse_plan_response(json.dumps(bad)) is None


class _StubDispatcher:
    def __init__(self, response_content: str):
        self._content = response_content
        self.calls: List[Any] = []

    async def dispatch(self, *, messages, agent_mode=None, response_format=None, **kwargs):
        self.calls.append({"messages": messages, "agent_mode": agent_mode})
        choice = SimpleNamespace(
            message=SimpleNamespace(content=self._content),
            finish_reason="stop",
        )
        response = SimpleNamespace(choices=[choice])
        return response, {"provider": "stub", "model": "stub"}


class _FailingDispatcher:
    async def dispatch(self, **_kwargs):
        raise RuntimeError("dispatcher offline")


class TestPlanDeliverable:
    def test_returns_plan_for_valid_response(self):
        dispatcher = _StubDispatcher(json.dumps(_SAMPLE_PLAN_JSON))
        plan = asyncio.run(
            plan_deliverable(
                prompt="Schreibe eine Hausarbeit über China",
                existing_draft_body="",
                dispatcher=dispatcher,
            )
        )
        assert plan is not None
        assert plan.expected_sections == 3
        assert dispatcher.calls and dispatcher.calls[0]["agent_mode"] == "writing_planner"

    def test_returns_none_when_dispatcher_raises(self):
        dispatcher = _FailingDispatcher()
        plan = asyncio.run(
            plan_deliverable(
                prompt="Some prompt",
                existing_draft_body="",
                dispatcher=dispatcher,
            )
        )
        assert plan is None

    def test_returns_none_for_empty_prompt(self):
        dispatcher = _StubDispatcher(json.dumps(_SAMPLE_PLAN_JSON))
        plan = asyncio.run(
            plan_deliverable(prompt="", existing_draft_body="", dispatcher=dispatcher)
        )
        assert plan is None
        # Dispatcher should NOT have been called
        assert dispatcher.calls == []

    def test_truncates_long_draft_in_user_prompt(self):
        dispatcher = _StubDispatcher(json.dumps(_SAMPLE_PLAN_JSON))
        long_draft = "x" * 10_000
        asyncio.run(
            plan_deliverable(
                prompt="Revise the draft",
                existing_draft_body=long_draft,
                dispatcher=dispatcher,
            )
        )
        user_msg = dispatcher.calls[0]["messages"][1]["content"]
        # Excerpt cap is 1500 head + 1500 tail + separator → < 4 KB
        assert len(user_msg) < 5000


class TestSessionPersistence:
    def test_load_returns_none_for_missing_plan(self):
        assert load_plan_from_session(None) is None
        assert load_plan_from_session({}) is None
        assert load_plan_from_session({"plan": "not a dict"}) is None

    def test_round_trip_through_session_settings(self):
        plan = DeliverablePlan.from_dict(_SAMPLE_PLAN_JSON)
        settings = serialise_plan_to_session({"existing": "value"}, plan)
        assert settings["existing"] == "value"
        assert settings["plan"]["language_code"] == "de"

        loaded = load_plan_from_session(settings)
        assert loaded == plan

    def test_serialise_does_not_mutate_input(self):
        original = {"existing": "value"}
        plan = DeliverablePlan.from_dict(_SAMPLE_PLAN_JSON)
        serialise_plan_to_session(original, plan)
        assert "plan" not in original
