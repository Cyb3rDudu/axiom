"""Tests for Literaturportfolio opt-out keyword detection."""

import pytest

from ai_researcher.agentic_layer.controller.utils.portfolio_optout import (
    deliverables_for_mission,
    detect_portfolio_optout,
)


@pytest.mark.parametrize(
    "text",
    [
        "ohne Literaturportfolio bitte",
        "Ohne Literatur Portfolio",
        "Diesmal kein Literaturportfolio",
        "keine Portfolio-Erstellung",
        "no portfolio for this one",
        "no literature portfolio needed",
        "skip portfolio",
        "skip the literature portfolio",
        "portfolio off",
        "disable portfolio",
        "disable literature portfolio",
    ],
)
def test_optout_detected(text: str) -> None:
    assert detect_portfolio_optout(text) is True, f"should opt out: {text!r}"


@pytest.mark.parametrize(
    "text",
    [
        "Erstelle bitte ein Portfolio-Unternehmen",  # "Portfolio" alone not enough
        "Research on investment portfolio allocation",
        "Write a literature review on corporate finance.",
        "",
        None,
        "Das Portfolio des Autors ist sehr breit.",
    ],
)
def test_no_optout(text) -> None:
    assert detect_portfolio_optout(text) is False, f"should NOT opt out: {text!r}"


def test_deliverables_default_on() -> None:
    """With no opt-out keyword and no explicit flag, default is ON."""
    d = deliverables_for_mission("Please write a comprehensive report.")
    assert d == {"literature_portfolio": True}


def test_deliverables_optout_wins() -> None:
    d = deliverables_for_mission("Please write a report, ohne Literaturportfolio.")
    assert d == {"literature_portfolio": False}


def test_explicit_false_overrides_keyword_free_request() -> None:
    d = deliverables_for_mission("Write a report.", explicit_flag=False)
    assert d == {"literature_portfolio": False}


def test_explicit_true_overrides_optout_keyword() -> None:
    """Explicit API flag beats a keyword opt-out (rare, but the flag is the
    authoritative user choice)."""
    d = deliverables_for_mission(
        "Write a report, no portfolio.",
        explicit_flag=True,
    )
    assert d == {"literature_portfolio": True}


def test_extra_sources_also_checked() -> None:
    """Opt-out may come from chat history / settings blobs, not just the
    user_request arg."""
    d = deliverables_for_mission(
        "Write a report.",
        None,
        "Earlier note: ohne Portfolio, das brauchen wir hier nicht.",
    )
    assert d == {"literature_portfolio": False}
