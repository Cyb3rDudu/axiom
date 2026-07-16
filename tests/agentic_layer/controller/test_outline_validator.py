"""Tests for the deterministic required-outline enforcement.

These exercise ``OutlineValidator.enforce_required_outline`` — the post-LLM
check that guarantees every required briefing section (parsed from the user's
Gliederung) is present in the generated outline, **including nested level-2+
subsections**, regardless of how the LLM emitted them (flat or nested), and
that freshly-inserted parents get the correct research strategy.
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


def _find_sec(outline, title_frag):
    """Return the first section whose normalized title contains ``title_frag``."""
    for s in outline:
        if title_frag in _normalize_title_for_match(s.title):
            return s
        r = _find_sec(s.subsections, title_frag)
        if r:
            return r
    return None


def _subtitles(outline, parent_frag):
    s = _find_sec(outline, parent_frag)
    return [c.title for c in s.subsections] if s else None


# ---------------------------------------------------------------------------
# title normalization (regression: heading marker before number)
# ---------------------------------------------------------------------------


def test_normalize_strips_leading_number():
    assert _normalize_title_for_match("1. Einleitung") == "einleitung"
    # Regression: heading marker BEFORE number must also reduce.
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
    assert len(out) == 3


def test_missing_nested_subsection_is_inserted():
    """REGRESSION (original finding 1): the LLM dropped ``3.2 Branchen- und
    Wettbewerbsumwelt``, a level-2 subsection. Must be inserted nested under its
    parent section 3, with sibling 3.1 preserved."""
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
    assert _subtitles(out, "darstellung") == [
        "3.1 NexMach als Unternehmung",
        "Branchen- und Wettbewerbsumwelt",
    ]


def test_flat_top_level_subsections_are_not_duplicated():
    """REGRESSION (review finding 1): when the planner emits 3, 3.1, 3.3 FLAT at
    top-level (instead of nesting them under 3), the flat 3.1/3.3 must be
    claimed and moved into section 3 — NOT re-inserted as duplicates while their
    flat originals linger at the top.

    Previously the matcher only looked inside the matched parent's existing
    subsections, so flat 3.1/3.3 were treated as missing, re-inserted under 3,
    and their flat originals appended again as extras (Makroumwelt appeared
    twice).
    """
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Theoretischer Bezugsrahmen", "level": 1},
        {"number": "3", "title": "Darstellung und Analyse", "level": 1},
        {"number": "3.1", "title": "Makroumwelt", "level": 2},
        {"number": "3.2", "title": "Branchen- und Wettbewerbsumwelt", "level": 2},
        {"number": "3.3", "title": "Stakeholder- und Netzwerkumwelt", "level": 2},
        {"number": "4", "title": "Umweltanalyse der NexMach", "level": 1},
        {"number": "5", "title": "Fazit", "level": 1},
    ]
    # 3, 3.1, 3.3 are all emitted flat at top-level (3.2 missing entirely).
    outline = [
        _section("1. Einleitung"),
        _section("2. Theoretischer Bezugsrahmen"),
        _section("3. Darstellung und Analyse"),
        _section("3.1 Makroumwelt"),
        _section("3.3 Stakeholder- und Netzwerkumwelt"),
        _section("4. Umweltanalyse der NexMach"),
        _section("5. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)

    # Only the genuinely-missing 3.2 is inserted; flat 3.1/3.3 are NOT duplicated.
    assert report["inserted"] == ["Branchen- und Wettbewerbsumwelt"]
    # Flat 3.1 and 3.3 moved into section 3.
    assert _subtitles(out, "darstellung") == [
        "3.1 Makroumwelt",
        "Branchen- und Wettbewerbsumwelt",
        "3.3 Stakeholder- und Netzwerkumwelt",
    ]
    # No leftover flat subsections at the top level.
    top_norm = [_normalize_title_for_match(s.title) for s in out]
    assert "makroumwelt" not in top_norm
    assert "stakeholder- und netzwerkumwelt" not in top_norm
    # Top level has exactly the 5 top-level sections (1,2,3,4,5).
    assert len(out) == 5


def test_mixed_nesting_flat_and_nested_subsections():
    """3.1 nested under 3, 3.3 flat at top-level — both must end up nested under
    section 3 with only the missing 3.2 inserted."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Theoretischer Bezugsrahmen", "level": 1},
        {"number": "3", "title": "Darstellung und Analyse", "level": 1},
        {"number": "3.1", "title": "Makroumwelt", "level": 2},
        {"number": "3.2", "title": "Branchen- und Wettbewerbsumwelt", "level": 2},
        {"number": "3.3", "title": "Stakeholder- und Netzwerkumwelt", "level": 2},
        {"number": "4", "title": "Umweltanalyse der NexMach", "level": 1},
        {"number": "5", "title": "Fazit", "level": 1},
    ]
    outline = [
        _section("1. Einleitung"),
        _section("2. Theoretischer Bezugsrahmen"),
        _section("3. Darstellung und Analyse", subsections=[
            _section("3.1 Makroumwelt"),
        ]),
        _section("3.3 Stakeholder- und Netzwerkumwelt"),  # flat
        _section("4. Umweltanalyse der NexMach"),
        _section("5. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == ["Branchen- und Wettbewerbsumwelt"]
    assert _subtitles(out, "darstellung") == [
        "3.1 Makroumwelt",
        "Branchen- und Wettbewerbsumwelt",
        "3.3 Stakeholder- und Netzwerkumwelt",
    ]
    assert len(out) == 5  # no flat 3.3 leftover


def test_inserted_parent_uses_synthesize_strategy():
    """REGRESSION (review finding 2): enforce_required_outline runs AFTER the
    final validate_and_correct, so it must itself set the right research
    strategy for inserted sections. A parent WITH subsections must use
    'synthesize_from_subsections' (the validator's own rule for such sections),
    not the default 'research_based'."""
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
    methodik = _find_sec(out, "methodik")
    assert methodik.research_strategy == "synthesize_from_subsections"
    # Leaf insert stays research_based.
    assert _find_sec(out, "datenerhebung").research_strategy == "research_based"


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
    assert [s.title for s in out] == ["1. Einleitung", "Methodik", "5. Fazit"]
    methodik = [s for s in out if s.title == "Methodik"][0]
    assert [c.title for c in methodik.subsections] == ["Datenerhebung"]


def test_extra_llm_sections_preserved_top_level():
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


def test_extra_nested_subsection_preserved_under_parent():
    """An extra subsection nested under a matched parent that isn't in the
    required outline must stay nested under that parent (not be hoisted to top
    level or dropped)."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "3", "title": "Darstellung", "level": 1},
        {"number": "3.1", "title": "Sub", "level": 2},
        {"number": "3.2", "title": "Missing", "level": 2},
        {"number": "5", "title": "Fazit", "level": 1},
    ]
    outline = [
        _section("1. Einleitung"),
        _section("3. Darstellung", subsections=[
            _section("3.1 Sub"),
            _section("3.4 Extra Analysis"),  # extra, not required
        ]),
        _section("5. Fazit"),
    ]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == ["Missing"]
    assert _subtitles(out, "darstellung") == [
        "3.1 Sub",
        "Missing",
        "3.4 Extra Analysis",
    ]


def test_deep_nesting_level3_inserted():
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
    sec3 = [s for s in out if "analyse" in s.title.lower()][0]
    sub31 = sec3.subsections[0]
    assert [c.title for c in sub31.subsections] == ["3.1.1 Deep", "Deeper Missing"]


def test_number_disambiguation_parent_vs_child_same_word():
    """``3 Darstellung`` vs ``3.1 Darstellung der Methodik`` must not be confused:
    required ``3`` must claim the number-3 section, and ``3.1`` the number-3.1
    section, nested under it."""
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "3", "title": "Darstellung", "level": 1},
        {"number": "3.1", "title": "Darstellung der Methodik", "level": 2},
    ]
    outline = [_section("3. Darstellung"), _section("3.1 Darstellung der Methodik")]
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert out[0].title == "3. Darstellung"
    assert _subtitles(out, "darstellung") == ["3.1 Darstellung der Methodik"]


def test_fuzzy_match_avoids_false_insertion():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Zentrale Umwelteinflüsse und Managementimplikationen", "level": 1},
    ]
    outline = [_section("5. Zentrale Umwelteinflüsse")]  # shorter, present
    out, report = v.enforce_required_outline(outline, required)
    assert report["inserted"] == []
    assert len(out) == 1


def test_inserted_sections_get_section_id():
    v = OutlineValidator(mission_id=None, controller=None)
    required = [
        {"number": "1", "title": "Einleitung", "level": 1},
        {"number": "2", "title": "Conclusion", "level": 1},
    ]
    outline = [_section("1. Einleitung")]
    out, _ = v.enforce_required_outline(outline, required)
    inserted = [s for s in out if s.title == "Conclusion"][0]
    assert inserted.section_id
