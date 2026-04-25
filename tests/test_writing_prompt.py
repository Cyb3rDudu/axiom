"""Tests for writing_prompt.py — writer system-prompt builder."""

from __future__ import annotations

import sys
from dataclasses import dataclass
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from services.writing_prompt import (  # noqa: E402
    build_writer_system_prompt,
    is_replace_mode_prompt,
)


@dataclass
class _StubProfile:
    in_text_rules: str


class TestReplaceModeDetector:
    @pytest.mark.parametrize(
        "prompt",
        [
            "ersetze X durch Y",
            "Bitte tausche die Quelle Müller gegen Schmidt aus.",
            "Please replace the chart with a table",
            "5 fixes: 1. fix typo 2. swap source A for B 3. add summary",
        ],
    )
    def test_triggers_on_replace_verbs(self, prompt):
        assert is_replace_mode_prompt(prompt) is True

    @pytest.mark.parametrize(
        "prompt",
        [
            "",
            None,
            "Schreibe einen Aufsatz über China",
            "Add a new section about renewable energy",
        ],
    )
    def test_does_not_trigger_otherwise(self, prompt):
        assert is_replace_mode_prompt(prompt or "") is False

    def test_lookahead_caps_search_window(self):
        prompt = "x" * 600 + " ersetze nochmal"
        # Default lookahead 500: replace verb is past it → no trigger
        assert is_replace_mode_prompt(prompt) is False
        # Wider window catches it
        assert is_replace_mode_prompt(prompt, lookahead=1000) is True


class TestBaseSegment:
    def test_always_includes_axiom_persona(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert "You are Axiom" in p

    def test_always_includes_math_delimiter_rule(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert "$formula$" in p
        assert "$$formula$$" in p

    def test_block_formatting_section_present(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert "content-block:BLOCK_TYPE" in p
        assert "content-block:section" in p


class TestCitationSegment:
    def test_numbered_default(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert "8-character Citation IDs" in p
        assert "in_text_rules" not in p

    def test_author_year_with_profile_uses_profile_rules(self):
        profile = _StubProfile(in_text_rules="USE_KMU_APA7_RULES_HERE")
        p = build_writer_system_prompt(
            citation_mode="author_year",
            citation_profile=profile,
        )
        assert "USE_KMU_APA7_RULES_HERE" in p
        # Numbered fallback should NOT also be present
        assert "8-character Citation IDs" not in p

    def test_author_year_without_profile_falls_back_to_numbered(self):
        # If the caller forgot to resolve a profile, builder must not
        # emit an author-year block with no rules — falls back instead.
        p = build_writer_system_prompt(
            citation_mode="author_year",
            citation_profile=None,
        )
        assert "8-character Citation IDs" in p


class TestStructuredBibliographyBlock:
    def test_absent_when_flag_off(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            structured_bibliography_enabled=False,
        )
        assert "STRUCTURED BIBLIOGRAPHY" not in p

    def test_present_when_flag_on(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            structured_bibliography_enabled=True,
        )
        assert "STRUCTURED BIBLIOGRAPHY" in p
        assert "content-block:references" in p
        assert "entry_key" in p


class TestReplaceModeBlock:
    def test_present_when_prompt_triggers(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            user_prompt="ersetze X durch Y",
        )
        assert "REPLACE-MODE" in p

    def test_absent_when_prompt_does_not_trigger(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            user_prompt="schreibe einen Aufsatz",
        )
        assert "REPLACE-MODE" not in p


class TestCustomPromptSegment:
    def test_appended_when_present(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            custom_prompt="Custom: emit only German prose.",
        )
        assert "ADDITIONAL USER INSTRUCTIONS" in p
        assert "Custom: emit only German prose." in p

    def test_absent_when_empty_or_whitespace(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            custom_prompt="   ",
        )
        assert "ADDITIONAL USER INSTRUCTIONS" not in p


class TestExternalContextSegment:
    def test_no_context_emits_no_external_block(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            external_context="",
        )
        assert "No external information was gathered" in p

    def test_filtered_context_emits_fallback(self):
        ctx = "Note: 5 sources were deemed not relevant and excluded"
        p = build_writer_system_prompt(
            citation_mode="numbered",
            external_context=ctx,
        )
        assert "filtered as not relevant" in p

    def test_present_context_emits_available_block(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            external_context="Search Result 1: …",
        )
        assert "external information from enabled tools" in p
        assert "filtered as not relevant" not in p


class TestSegmentOrdering:
    def test_structured_biblio_appears_before_replace_mode(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            structured_bibliography_enabled=True,
            user_prompt="ersetze X durch Y",
        )
        assert p.index("STRUCTURED BIBLIOGRAPHY") < p.index("REPLACE-MODE")

    def test_custom_prompt_appears_before_external_context(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            custom_prompt="Custom hint",
            external_context="Search Result 1: …",
        )
        # External-context trailer sentence is unique to its segment.
        assert p.index("ADDITIONAL USER INSTRUCTIONS") < p.index(
            "external information from enabled tools"
        )

    def test_base_appears_before_citation(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert p.index("You are Axiom") < p.index("CITATION INSTRUCTIONS")


class TestPlanBudgetSegment:
    def test_absent_when_no_plan_provided(self):
        p = build_writer_system_prompt(citation_mode="numbered")
        assert "WORD-BUDGET" not in p
        assert "WORTBUDGET" not in p

    def test_present_with_section_budgets_de(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            section_budgets={
                1: (400, "Einleitung"),
                2: (600, "Hauptteil"),
                3: (400, "Fazit"),
            },
            total_word_budget=(2960, 3300),
            language_code="de",
        )
        assert "WORTBUDGET-VORGABE" in p
        assert "1. Einleitung: ~400 Wörter" in p
        assert "2. Hauptteil: ~600 Wörter" in p
        assert "3. Fazit: ~400 Wörter" in p
        assert "2960–3300 Wörter" in p
        # Anti-cheating clause must reference deterministic backend count
        assert "deterministisch" in p
        # No self-declared word-count line allowed
        assert "Wortbilanz" in p

    def test_present_with_section_budgets_en(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            section_budgets={1: (400, "Intro"), 2: (600, "Body")},
            total_word_budget=(2000, 2500),
            language_code="en",
        )
        assert "WORD-BUDGET CONTRACT" in p
        assert "1. Intro: ~400 words" in p
        assert "2000–2500 words" in p
        assert "deterministically" in p

    def test_total_only_no_sections(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            total_word_budget=(1000, 1500),
            language_code="en",
        )
        assert "WORD-BUDGET CONTRACT" in p
        assert "1000–1500 words" in p
        assert "PER-SECTION BUDGET" not in p

    def test_sections_only_no_total(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            section_budgets={1: (400, "Intro")},
            language_code="en",
        )
        assert "WORD-BUDGET CONTRACT" in p
        assert "PER-SECTION BUDGET" in p
        assert "TOTAL WORD BUDGET" not in p

    def test_appears_before_structured_bibliography(self):
        p = build_writer_system_prompt(
            citation_mode="numbered",
            structured_bibliography_enabled=True,
            section_budgets={1: (400, "Intro")},
            language_code="en",
        )
        assert p.index("WORD-BUDGET CONTRACT") < p.index("STRUCTURED BIBLIOGRAPHY")
