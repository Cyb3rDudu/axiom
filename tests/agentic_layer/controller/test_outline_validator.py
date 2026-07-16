"""Tests for the deterministic required-outline enforcement (Finding 1).

These exercise ``OutlineValidator.enforce_required_outline`` — the post-LLM
check that guarantees every required briefing section (parsed from the user's
Gliederung) is present in the generated outline, regardless of LLM behaviour.
"""

# Prime axiom_backend's import graph.
import api as _api_primer  # noqa: F401  # isort: skip

import pytest

from ai_researcher.agentic_layer.controller.utils.outline_validator import (
    OutlineValidator,
    _normalize_title_for_match,
)
from ai_researcher.agentic_layer.schemas.planning import ReportSection


def _section(title: str) -> ReportSection:
    return ReportSection(
        section_id=title.lower().replace(" ", "_")[:40],
        title=title,
        description="d",
        research_strategy="research_based",
    )


# ---------------------------------------------------------------------------
# title normalization / matching
# ---------------------------------------------------------------------------


def test_normalize_strips_leading_number():
    assert _normalize_title_for_match("1. Einleitung") == "einleitung"
    assert _normalize_title_for_match("## 2.1 NexMach als Unternehmung") == "nexmach als unternehmung"
    assert _normalize_title_for_match("Fazit") == "fazit"


def test_no_required_outline_is_noop():
    v = OutlineValidator(mission_id=None, controller=None)
    outline = [_section("Einleitung")]
    out, report = v.enforce_required_outline(outline, [])
    assert out == outline
    assert report["required_count"] == 0


def test_all_present_no_changes():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Analyse", "level": 1},
        {"number": "3", "title": "Fazit", "level": 1},
    ]
    outline = [_section("1. Einleitung"), _section("2. Analyse"), _section("3. Fazit")]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert report["missing_required"] == []
    assert {s.title for s in out} == {"1. Einleitung", "2. Analyse", "3. Fazit"}


def test_missing_required_section_is_inserted():
    """Regression for the production failure: section 3.2 (Branchen- und
    Wettbewerbsumwelt) was dropped by the LLM. The check must re-insert any
    missing required top-level section deterministically."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Theorie", "level": 1},
        {"number": "3", "title": "Analyse", "level": 1},
        {"number": "4", "title": "Fazit", "level": 1},
        {"number": "5", "title": "Kritische Reflexion", "level": 1},
    ]
    # Generated outline is MISSING 'Kritische Reflexion'.
    outline = [_section("1. Einleitung"), _section("2. Theorie"), _section("3. Analyse"), _section("4. Fazit")]
    out, report = v.enforce_required_outline(outline, required)

    titles = [s.title for s in out]
    assert "Kritische Reflexion" in titles
    assert report["inserted"] == ["Kritische Reflexion"]
    assert report["missing_required"] == ["Kritische Reflexion"]
    assert report["matched"] == ["Einleitung", "Theorie", "Analyse", "Fazit"]


def test_fuzzy_match_avoids_false_insertion():
    """A required title that appears with slightly different wording should NOT
    be inserted again (avoid duplicates)."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [{"number": "1", "title": "Zentrale Umwelteinflüsse und Managementimplikationen", "level": 1}]
    outline = [_section("5. Zentrale Umwelteinflüsse")]  # shorter, present
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert len(out) == 1


def test_inserted_sections_get_research_strategy():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [{"number": "1", "title": "Einleitung", "level": 1}, {"number": "2", "title": "Conclusion", "level": 1}]
    outline = [_section("1. Einleitung")]  # Conclusion missing
    out, _ = v.enforce_required_outline(outline, required)
    inserted = [s for s in out if s.title == "Conclusion"][0]
    assert inserted.research_strategy == "research_based"
    assert inserted.section_id  # has an id
