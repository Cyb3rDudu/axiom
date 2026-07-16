"""Tests for the deterministic required-outline enforcement (Finding 1).

These exercise ``OutlineValidator.enforce_required_outline`` — the post-LLM
check that guarantees every required briefing section (parsed from the user's
Gliederung) is present in the generated outline, **including nested level-2+
subsections**, regardless of LLM behaviour.
"""

# Prime axiom_backend's import graph.
import api as _api_primer  # noqa: F401  # isort: skip

import pytest

from ai_researcher.agentic_layer.controller.utils.outline_validator import (
    OutlineValidator,
    _normalize_title_for_match,
)
from ai_researcher.agentic_layer.schemas.planning import ReportSection


def _section(title: str, subsections=None) -> ReportSection:
    return ReportSection(
        section_id=title.lower().replace(" ", "_")[:40],
        title=title,
        description="d",
        research_strategy="research_based",
        subsections=subsections or [],
    )


def _find_subsection(outline, parent_title, child_norm) -> bool:
    """True if a child whose normalized title == ``child_norm`` is nested under
    a section matching ``parent_title``."""
    for s in outline:
        if _normalize_title_for_match(parent_title) in _normalize_title_for_match(
            s.title
        ):
            for c in s.subsections:
                if _normalize_title_for_match(c.title) == child_norm:
                    return True
    return False


# ---------------------------------------------------------------------------
# title normalization / matching (Finding 2)
# ---------------------------------------------------------------------------


def test_normalize_strips_leading_number():
    assert _normalize_title_for_match("1. Einleitung") == "einleitung"
    # Regression (Finding 2): heading marker BEFORE number must also reduce.
    assert (
        _normalize_title_for_match("## 2.1 NexMach als Unternehmung")
        == "nexmach als unternehmung"
    )
    assert _normalize_title_for_match("2.1. Branchen- und Wettbewerbsumwelt") == (
        "branchen- und wettbewerbsumwelt"
    )
    assert _normalize_title_for_match("Fazit") == "fazit"
    assert _normalize_title_for_match("einleitung.") == "einleitung"


def test_normalize_strips_double_digit_number():
    assert _normalize_title_for_match("## 10. Fazit und Ausblick") == (
        "fazit und ausblick"
    )


# ---------------------------------------------------------------------------
# enforce_required_outline
# ---------------------------------------------------------------------------


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
    # top-level count preserved, no duplicates
    assert len(out) == 3


def test_missing_nested_subsection_is_inserted():
    """REGRESSION for the production failure (Finding 1, the actual bug).

    The LLM dropped ``3.2 Branchen- und Wettbewerbsumwelt`` — a level-2
    subsection under section 3. The previous ``enforce_required_outline`` only
    handled level-1 sections and silently ignored this. It must be detected and
    inserted nested under its parent section 3, with sibling 3.1 preserved.
    """
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Theoretischer Bezugsrahmen", "level": 1},
        {"number": "3", "title": "Darstellung und Analyse", "level": 1},
        {"number": "3.1", "title": "NexMach als Unternehmung", "level": 2},
        {"number": "3.2", "title": "Branchen- und Wettbewerbsumwelt", "level": 2},
        {"number": "4", "title": "Umweltanalyse der NexMach", "level": 1},
        {"number": "5", "title": "Fazit", "level": 1},
    ]
    # Generated outline has section 3 with ONLY 3.1 (3.2 is missing).
    outline = [
        _section("1. Einleitung"),
        _section("2. Theoretischer Bezugsrahmen"),
        _section("3. Darstellung und Analyse", subsections=[
            _section("3.1 NexMach als Unternehmung"),
        ]),
        _section("4. Umweltanalyse der NexMach"),
        _section("5. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)

    assert report["inserted"] == ["Branchen- und Wettbewerbsumwelt"]
    assert report["missing_required"] == ["Branchen- und Wettbewerbsumwelt"]
    # 3.2 now nested under section 3.
    assert _find_subsection(
        out, "Darstellung und Analyse", "branchen- und wettbewerbsumwelt"
    )
    # Section 3 now has both subsections, in order.
    sec3 = [s for s in out if "darstellung" in s.title.lower()][0]
    assert [c.title for c in sec3.subsections] == [
        "3.1 NexMach als Unternehmung",
        "Branchen- und Wettbewerbsumwelt",
    ]


def test_missing_top_level_section_is_inserted():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Theorie", "level": 1},
        {"number": "3", "title": "Analyse", "level": 1},
        {"number": "4", "title": "Fazit", "level": 1},
        {"number": "5", "title": "Kritische Reflexion", "level": 1},
    ]
    outline = [
        _section("1. Einleitung"),
        _section("2. Theorie"),
        _section("3. Analyse"),
        _section("4. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)
    titles = [s.title for s in out]
    assert "Kritische Reflexion" in titles
    assert report["inserted"] == ["Kritische Reflexion"]


def test_missing_parent_and_child_both_inserted():
    """When a required parent AND its child are both missing, the parent is
    created at top level and the child nested under it (report order is
    parent-first)."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "4", "title": "Methodik", "level": 1},
        {"number": "4.1", "title": "Datenerhebung", "level": 2},
        {"number": "5", "title": "Fazit", "level": 1},
    ]
    outline = [_section("1. Einleitung"), _section("5. Fazit")]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == ["Methodik", "Datenerhebung"]  # parent first
    top_titles = [s.title for s in out]
    assert top_titles == ["1. Einleitung", "Methodik", "5. Fazit"]
    methodik = [s for s in out if s.title == "Methodik"][0]
    assert [c.title for c in methodik.subsections] == ["Datenerhebung"]


def test_extra_llm_sections_preserved_and_appended():
    """Sections the LLM added that aren't in the required outline are kept,
    appended after the required (ordered) sections."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Fazit", "level": 1},
    ]
    outline = [
        _section("1. Einleitung"),
        _section("Zusaetzliche Reflexion"),
        _section("2. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert [s.title for s in out] == [
        "1. Einleitung",
        "2. Fazit",
        "Zusaetzliche Reflexion",
    ]


def test_deep_nesting_level3_inserted():
    """A missing level-3 subsection is inserted under its level-2 parent."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "3", "title": "Analyse", "level": 1},
        {"number": "3.1", "title": "Sub", "level": 2},
        {"number": "3.1.1", "title": "Deep", "level": 3},
        {"number": "3.1.2", "title": "Deeper Missing", "level": 3},
    ]
    outline = [
        _section("3. Analyse", subsections=[
            _section("3.1 Sub", subsections=[_section("3.1.1 Deep")]),
        ]),
    ]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == ["Deeper Missing"]
    # Navigate to 3.1's subsections.
    sec3 = [s for s in out if "analyse" in s.title.lower()][0]
    sub31 = sec3.subsections[0]
    assert [c.title for c in sub31.subsections] == ["3.1.1 Deep", "Deeper Missing"]


def test_fuzzy_match_avoids_false_insertion():
    """A required title present with slightly different wording must NOT be
    inserted again (avoid duplicates)."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Zentrale Umwelteinflüsse und Managementimplikationen", "level": 1},
    ]
    outline = [_section("5. Zentrale Umwelteinflüsse")]  # shorter, present
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert len(out) == 1


def test_inserted_sections_get_research_strategy():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Conclusion", "level": 1},
    ]
    outline = [_section("1. Einleitung")]
    out, _ = v.enforce_required_outline(outline, required)
    inserted = [s for s in out if s.title == "Conclusion"][0]
    assert inserted.research_strategy == "research_based"
    assert inserted.section_id
