"""Deterministic word-budget pipeline tests (Priorities 1–4).

Covers:
  - extract_word_budget: total + per-section extraction, decoy immunity.
  - PlanningAgent._distribute_word_budgets: per-section budgets projected onto
    ReportSection objects (target_words_min/max/budget_source), parent-with-
    subsections distribution.
  - writing_manager._trim_to_word_budget: post-generation guard trims at a
    sentence boundary and respects the hard cap.

These are the deterministic guardrails against a 47.000-word blowup like the
c00de8dd production mission.
"""

# Prime axiom_backend's import graph (matches the outline_validator test).
import api as _api_primer  # noqa: F401  # isort: skip

import pytest

from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
    extract_word_budget,
)
from ai_researcher.agentic_layer.schemas.planning import ReportSection
from ai_researcher.agentic_layer.agents.planning_agent import PlanningAgent
from ai_researcher.agentic_layer.controller.writing_manager import (
    _trim_to_word_budget,
)


# ---------------------------------------------------------------------------
# extract_word_budget
# ---------------------------------------------------------------------------

_BUDGET_BRIEFING = (
    "Die Hausarbeit umfasst ca. 3.000 W\u00f6rter.\n\n"
    "# Fallunternehmen\n"
    "Jahresumsatz: rund 19 Mio. Euro.\n"
    "Unternehmensgr\u00f6\u00dfe: 85 Mitarbeitende.\n"
    "Umsatzleistung: rund 470.000 Euro pro Mitarbeitendem.\n\n"
    "# Verbindliche Gliederung\n\n"
    "## 1. Einleitung\n"
    "Umfang: ungef\u00e4hr 230 bis 270 W\u00f6rter\n\n"
    "## 2. Theorie\n\n"
    "### 2.1 Unternehmung\n"
    "Umfang: ca. 180 bis 220 W\u00f6rter\n\n"
    "## 3. Analyse\n"
    "Umfang: 1.100 bis 1.200 W\u00f6rter\n\n"
    "# Quellenanforderungen\n"
    "Verwende 13 bis 16 Quellen.\n"
)


def test_extract_word_budget_total():
    wb = extract_word_budget(_BUDGET_BRIEFING)
    assert wb.total == (2700, 3300, 3000)


def test_extract_word_budget_sections():
    wb = extract_word_budget(_BUDGET_BRIEFING)
    assert wb.sections == {"1": (230, 270), "2.1": (180, 220), "3": (1100, 1200)}


def test_extract_word_budget_no_decoy_numbers():
    wb = extract_word_budget(_BUDGET_BRIEFING)
    all_nums = set()
    if wb.total:
        all_nums.update(wb.total[:2])
    all_nums.update(v for rng in wb.sections.values() for v in rng)
    for bad in (19, 470000, 85, 13, 16):
        assert bad not in all_nums, f"decoy number {bad} leaked into budget"


def test_extract_word_budget_german_dative_woertern():
    wb = extract_word_budget("Liegt der Textumfang bei ungef\u00e4hr 3.000 W\u00f6rtern?")
    assert wb.total == (2700, 3300, 3000)


# ---------------------------------------------------------------------------
# PlanningAgent._distribute_word_budgets
# ---------------------------------------------------------------------------

def _build_outline_for_budget():
    return [
        ReportSection(section_id="s1", title="Einleitung", description="x",
                      research_strategy="research_based"),
        ReportSection(section_id="s2", title="Theoretischer Bezugsrahmen", description="x",
                      research_strategy="synthesize_from_subsections",
                      subsections=[
                          ReportSection(section_id="s2_1",
                                        title="NexMach als marktwirtschaftliche Unternehmung",
                                        description="x", research_strategy="research_based"),
                          ReportSection(section_id="s2_2",
                                        title="NexMach als offenes soziales System",
                                        description="x", research_strategy="research_based"),
                      ]),
        ReportSection(section_id="s3", title="Darstellung und Analyse der Unternehmensumwelt",
                      description="x", research_strategy="research_based"),
        ReportSection(section_id="s6", title="Fazit", description="x",
                      research_strategy="research_based"),
    ]


_REQUIRED_OUTLINE = [
    {"number": "1", "title": "Einleitung", "level": 1},
    {"number": "2", "title": "Theoretischer Bezugsrahmen", "level": 1},
    {"number": "2.1", "title": "NexMach als marktwirtschaftliche Unternehmung", "level": 2},
    {"number": "2.2", "title": "NexMach als offenes soziales System", "level": 2},
    {"number": "3", "title": "Darstellung und Analyse der Unternehmensumwelt", "level": 1},
    {"number": "6", "title": "Fazit", "level": 1},
]

_MISSION_WORD_BUDGET = {
    "total_word_budget": {"min": 2700, "target": 3000, "max": 3300},
    "section_word_budgets": {"1": [230, 270], "2": [550, 650], "3": [450, 550], "6": [180, 220]},
    "budget_source": "test",
}


def test_distribute_word_budgets_assigns_explicit_per_section():
    outline = _build_outline_for_budget()
    PlanningAgent._distribute_word_budgets(None, outline, _MISSION_WORD_BUDGET, _REQUIRED_OUTLINE)
    by_title = {s.title: s for s in outline}
    assert by_title["Einleitung"].target_words_max == 270
    assert by_title["Einleitung"].target_words_min == 230
    assert by_title["Darstellung und Analyse der Unternehmensumwelt"].target_words_max == 550
    assert by_title["Fazit"].target_words_max == 220


def test_distribute_word_budgets_distributes_parent_across_subsections():
    """A parent with an explicit budget but subsections lacking one must reserve
    a short intro slice and split the remainder across the subsections (so the
    parent intro + children together stay within the chapter budget)."""
    outline = _build_outline_for_budget()
    PlanningAgent._distribute_word_budgets(None, outline, _MISSION_WORD_BUDGET, _REQUIRED_OUTLINE)
    parent = next(s for s in outline if s.title == "Theoretischer Bezugsrahmen")
    # Parent now reserves a SHORT intro slice (~15% of 650, capped), not the full 650.
    assert parent.target_words_max is not None
    assert parent.target_words_max <= 120, (
        f"parent intro should be a short reserved slice (<=120), got {parent.target_words_max}"
    )
    # Each subsection inherited a share of the remainder.
    for sub in parent.subsections:
        assert sub.target_words_max is not None
        assert sub.target_words_min is not None
        assert sub.budget_source and "distributed from parent" in sub.budget_source


def test_distribute_word_budgets_budget_source_set():
    outline = _build_outline_for_budget()
    PlanningAgent._distribute_word_budgets(None, outline, _MISSION_WORD_BUDGET, _REQUIRED_OUTLINE)
    by_title = {s.title: s for s in outline}
    assert by_title["Einleitung"].budget_source
    assert "briefing" in by_title["Einleitung"].budget_source


def test_distribute_word_budgets_mixed_explicit_implicit_children():
    """Review finding 4: with two explicit-budget children and one implicit
    child, the implicit child must receive a share of the REMAINDER (parent max
    minus intro minus explicit children), not a naive parent/n split. The
    invariant parent_intro + sum(child_max) <= parent_max must hold.

    Reproduction before the fix:
      parent 550-650, intro 97, A=220, B=220, C=184 -> sum_max=721 > 650.
    """
    parent = ReportSection(
        section_id="s2", title="Chapter", description="x",
        research_strategy="synthesize_from_subsections",
        target_words_min=550, target_words_max=650,
        budget_source="briefing Umfang: 550-650 Wörter",
        subsections=[
            ReportSection(section_id="a", title="A", description="x",
                          research_strategy="research_based",
                          target_words_min=200, target_words_max=220,
                          budget_source="briefing Umfang: 200-220"),
            ReportSection(section_id="b", title="B", description="x",
                          research_strategy="research_based",
                          target_words_min=200, target_words_max=220,
                          budget_source="briefing Umfang: 200-220"),
            ReportSection(section_id="c", title="C", description="x",
                          research_strategy="research_based"),  # implicit
        ],
    )
    budget = {"total_word_budget": None,
              "section_word_budgets": {"2": [550, 650]}, "budget_source": "t"}
    required = [{"number": "2", "title": "Chapter", "level": 1}]
    PlanningAgent._distribute_word_budgets(None, [parent], budget, required)

    intro_max = parent.target_words_max
    a = parent.subsections[0].target_words_max
    b = parent.subsections[1].target_words_max
    c = parent.subsections[2].target_words_max
    sum_max = intro_max + a + b + c
    # The hard invariant.
    assert sum_max <= 650, (
        f"parent budget invariant violated: intro({intro_max})+A({a})+B({b})"
        f"+C({c}) = {sum_max} > parent max 650"
    )
    # The implicit child got a budget (not None) and its source records the
    # explicit-children subtraction.
    assert c is not None
    assert "distributed from parent" in parent.subsections[2].budget_source
    assert "explicit children" in parent.subsections[2].budget_source


def test_distribute_word_budgets_noop_without_budget():
    outline = _build_outline_for_budget()
    PlanningAgent._distribute_word_budgets(None, outline, {}, None)
    for s in outline:
        assert s.target_words_max is None
        assert s.target_words_min is None


# ---------------------------------------------------------------------------
# writing_manager._trim_to_word_budget
# ---------------------------------------------------------------------------

_TRIM_TEXT = (
    "Ein Satz. Noch ein Satz. Und ein dritter Satz der etwas laenger ist "
    "als die anderen. Letzter Satz hier drinnen."
)


def test_trim_no_change_when_under_limit():
    out = _trim_to_word_budget(_TRIM_TEXT, 100)
    assert out == _TRIM_TEXT


def test_trim_cuts_at_sentence_boundary_when_over_limit():
    out = _trim_to_word_budget(_TRIM_TEXT, 8)
    assert len(out.split()) <= 9
    assert out.rstrip().endswith(("…", "..."))


def test_trim_respects_hard_cap():
    out = _trim_to_word_budget(_TRIM_TEXT, 3)
    assert len(out.split()) <= 4  # ellipsis adds at most one token


def test_trim_empty_and_zero():
    assert _trim_to_word_budget("", 100) == ""
    assert _trim_to_word_budget(_TRIM_TEXT, 0) == _TRIM_TEXT


# ---------------------------------------------------------------------------
# writing_manager.is_empty_or_placeholder_content (review finding 5)
# ---------------------------------------------------------------------------
# Parent synthesis must recognise the structured '[QUELLE ERFORDERLICH]' gap
# marker (and the legacy/error placeholders) so it does not synthesise a
# chapter intro on top of placeholder stubs.

from ai_researcher.agentic_layer.controller.writing_manager import (  # noqa: E402
    is_empty_or_placeholder_content,
)


def test_placeholder_detects_source_gap_marker():
    assert is_empty_or_placeholder_content(
        "[QUELLE ERFORDERLICH] F\u00fcr den Abschnitt \u201eEinleitung\u201c "
        "konnte keine ausreichende Literaturfundstelle recherchiert werden."
    )


def test_placeholder_detects_legacy_phrase_and_errors():
    assert is_empty_or_placeholder_content("No information found to write this section.")
    assert is_empty_or_placeholder_content("[Error: LLM call failed]")


def test_placeholder_does_not_flag_real_content():
    assert not is_empty_or_placeholder_content(
        "NexMach erzielte im Gesch\u00e4ftsjahr 2023 einen Jahresumsatz von "
        "rund 40 Mio. Euro [doc_abc123]."
    )
    assert not is_empty_or_placeholder_content("Kurzer echter Absatz.")


def test_placeholder_detects_empty():
    assert is_empty_or_placeholder_content("")
    assert is_empty_or_placeholder_content("   \n  ")
