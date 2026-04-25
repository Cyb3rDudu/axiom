"""Tests for transparent section-scoped continuation."""

from __future__ import annotations

import sys
from pathlib import Path

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402
import api as _api_primer  # noqa: F401, E402

from services.writing_continuation import (  # noqa: E402
    build_continuation_prompt,
    detect_cut_point,
    infer_language_code,
    parse_sections,
    run_continuations,
    stitch_continuation,
)


COMPLETE_5_SECTION = """\
```content-block:document
# 1. Intro

Some prose here that ends in a period.

# 2. Theory

More prose that ends cleanly.

# 3. Analysis

Detailed analysis ending properly.

# 4. Implications

Implications discussed thoroughly.

# 5. Conclusion

Final thoughts end here.
```
"""


TRUNCATED_MID_SECTION_4 = """\
```content-block:document
# 1. Intro

Some prose here that ends in a period.

# 2. Theory

More prose that ends cleanly.

# 3. Analysis

Detailed analysis ending properly.

# 4. Implications

Implications being discussed when the model ran out of tok
```
"""


MISSING_LAST_SECTION = """\
```content-block:document
# 1. Intro

Prose ending fine.

# 2. Theory

Theory content complete.

# 3. Body

Body content complete.

# 4. Analysis

Analysis complete.
```
"""


class TestParseSections:
    def test_parses_five_complete_sections(self):
        body = COMPLETE_5_SECTION.split("```content-block:document\n")[1].rsplit(
            "\n```", 1
        )[0]
        sections = parse_sections(body)
        assert [s.index for s in sections] == [1, 2, 3, 4, 5]
        assert all(s.is_complete for s in sections)

    def test_detects_incomplete_last_section(self):
        body = TRUNCATED_MID_SECTION_4.split("```content-block:document\n")[1].rsplit(
            "\n```", 1
        )[0]
        sections = parse_sections(body)
        assert sections[-1].index == 4
        assert sections[-1].is_complete is False

    def test_empty_body(self):
        assert parse_sections("") == []

    def test_no_numbered_headings(self):
        assert parse_sections("Just plain prose with # Some Heading") == []

    def test_accepts_h2_numbered_headings(self):
        """Academic papers use H1 for the document title and H2 for
        sections. The parser must pick up ## 1. / ## 2. / ... just
        like # 1. / # 2. / ...
        """
        body = (
            "# Paper Title\n\n"
            "## 1. Einleitung\n\nIntro prose.\n\n"
            "## 2. Theorie\n\nTheory prose.\n\n"
            "## 3. Schluss\n\nFinal prose.\n"
        )
        sections = parse_sections(body)
        assert [s.index for s in sections] == [1, 2, 3]
        assert [s.title for s in sections] == ["Einleitung", "Theorie", "Schluss"]

    def test_accepts_h3_numbered_headings(self):
        body = "### 1. Alpha\n\nAlpha prose.\n\n### 2. Beta\n\nBeta prose.\n"
        sections = parse_sections(body)
        assert [s.index for s in sections] == [1, 2]

    def test_mixed_heading_levels_all_captured(self):
        body = (
            "# Title\n\n"
            "## 1. Outer A\n\nOuter prose.\n\n"
            "### 1.1 Inner stuff\n\nInner prose.\n\n"
            "## 2. Outer B\n\nMore prose.\n"
        )
        sections = parse_sections(body)
        # Three numbered headings: 1. Outer A, 1.1 Inner stuff (doesn't
        # match — needs trailing dot after number), 2. Outer B.
        # Expect 1 and 2 at top level (the 1.1 pattern is distinct).
        indices = [s.index for s in sections]
        assert 1 in indices and 2 in indices

    def test_markdown_bold_trailer_is_incomplete(self):
        """The live-run bug: truncation happened mid-bold ('**Zwei Szenarien')
        and the fence balancer closed the code fence, producing a section
        ending on `**`. Previous detector accepted `**` as sentence
        terminator → continuation skipped. Real terminator check must
        strip markdown sigils first.
        """
        body = (
            "# 1. Intro\n\nComplete sentence.\n\n"
            "# 2. Body\n\nThis is fine.\n\n"
            "# 3. Conclusion\n\nPartial thought **"
        )
        sections = parse_sections(body)
        assert len(sections) == 3
        assert sections[0].is_complete is True
        assert sections[1].is_complete is True
        assert sections[2].is_complete is False, (
            "section ending on bare '**' must be incomplete"
        )

    @pytest.mark.parametrize(
        "trailer,expected",
        [
            ("finishes cleanly.", True),
            ("ends with an exclamation!", True),
            ("rhetorical question?", True),
            ("italic *emphasis*", False),  # stripped to "emphasis", no terminator
            ("bold text**", False),
            ("code `snippet`", False),
            ("sentence ends with bracketed source (BPB, 2024).", True),
            ("parenthetical without terminator (note)", False),
            ("citation mid-thought ( Müller, 2020", False),
        ],
    )
    def test_terminator_edge_cases(self, trailer, expected):
        body = f"# 1. Test\n\nLead-in prose. {trailer}"
        sections = parse_sections(body)
        assert sections[0].is_complete is expected


class TestDetectCutPoint:
    def test_complete_response_returns_none(self):
        cut = detect_cut_point(COMPLETE_5_SECTION, expected_sections=5)
        assert cut is None

    def test_truncated_mid_section_reported(self):
        cut = detect_cut_point(TRUNCATED_MID_SECTION_4, expected_sections=5)
        assert cut is not None
        assert cut["last_complete_section_index"] == 3
        assert cut["last_partial_section"].index == 4
        assert cut["missing_section_indices"] == [5]
        assert "ran out of tok" in cut["partial_tail"]

    def test_missing_entire_last_section(self):
        cut = detect_cut_point(MISSING_LAST_SECTION, expected_sections=5)
        assert cut is not None
        assert cut["missing_section_indices"] == [5]
        assert cut["last_partial_section"] is None

    def test_custom_expected_sections_count(self):
        # 8-section market research report, only 4 present
        body = "```content-block:document\n# 1. A\n\nFoo.\n\n# 2. B\n\nBar.\n\n# 3. C\n\nBaz.\n\n# 4. D\n\nQux.\n```\n"
        cut = detect_cut_point(body, expected_sections=8)
        assert cut is not None
        assert cut["missing_section_indices"] == [5, 6, 7, 8]


class TestBuildContinuationPrompt:
    def _cut(self, **kwargs):
        from services.writing_continuation import SectionInfo
        default = {
            "last_complete_section_index": 3,
            "last_partial_section": SectionInfo(
                index=4, title="Analysis", start_offset=0, end_offset=0,
                words=10, is_complete=False,
            ),
            "missing_section_indices": [5],
            "partial_tail": "some tail text",
            "expected_sections": 5,
        }
        default.update(kwargs)
        return default

    def test_german_prompt_contains_abschnitt(self):
        prompt = build_continuation_prompt(self._cut(), language_code="de")
        assert "Abschnitt 4" in prompt
        assert "1–3" in prompt or "1-3" in prompt
        assert "Token-Limit" in prompt

    def test_english_prompt_contains_section(self):
        prompt = build_continuation_prompt(self._cut(), language_code="en")
        assert "Section 4" in prompt
        assert "Section 5" in prompt
        assert "token limit" in prompt

    def test_budget_hint_included(self):
        prompt = build_continuation_prompt(
            self._cut(), section_budgets={5: 320}, language_code="en"
        )
        assert "320" in prompt

    def test_partial_tail_included(self):
        prompt = build_continuation_prompt(
            self._cut(partial_tail="TAIL SENTINEL"), language_code="en"
        )
        assert "TAIL SENTINEL" in prompt


def _build_complete_doc(per_section_words: list[int]) -> str:
    """Helper: emit a content-block:document with N numbered sections, each
    containing the requested word count, every section ending on a period."""
    body_parts = []
    for i, w in enumerate(per_section_words, start=1):
        sentence = ("lorem " * w).strip() + "."
        body_parts.append(f"# {i}. Section{i}\n\n{sentence}")
    body = "\n\n".join(body_parts)
    return f"```content-block:document\n{body}\n```\n"


class TestDetectCutPointUnderBudget:
    def test_no_trigger_when_no_budgets_provided(self):
        # Section is small but no budget signal → not flagged
        content = _build_complete_doc([50])
        cut = detect_cut_point(content, expected_sections=1)
        assert cut is None

    def test_no_trigger_when_section_meets_budget(self):
        content = _build_complete_doc([400, 400])
        cut = detect_cut_point(
            content, expected_sections=2,
            section_budgets={1: 400, 2: 400},
            total_word_budget=(800, 1000),
        )
        assert cut is None

    def test_total_underrun_triggers(self):
        # Total 200 of 1000 target → under 850 floor, fires
        content = _build_complete_doc([100, 100])
        cut = detect_cut_point(
            content, expected_sections=2,
            total_word_budget=(1000, 1200),
        )
        assert cut is not None
        assert cut["mode"] == "underbudget"
        # Last section is the expansion target
        assert cut["underbudget_section"].index == 2

    def test_last_section_underrun_triggers(self):
        # Section 1 fine (400), section 2 well below 0.6 of 800 (=200)
        content = _build_complete_doc([400, 100])
        cut = detect_cut_point(
            content, expected_sections=2,
            section_budgets={1: 400, 2: 800},
        )
        assert cut is not None
        assert cut["mode"] == "underbudget"
        assert cut["underbudget_section"].index == 2
        assert cut["underbudget_target"] == 800

    def test_small_section_target_below_threshold_skipped(self):
        # Target 150 < 200 minimum-trigger → tiny shortfall ignored
        content = _build_complete_doc([50])
        cut = detect_cut_point(
            content, expected_sections=1,
            section_budgets={1: 150},
        )
        assert cut is None

    def test_truncation_takes_precedence_over_budget(self):
        # Section ends mid-word (no terminator) → structural truncation
        # wins over the underbudget check, mode is "truncated".
        body = (
            "```content-block:document\n"
            "# 1. Einleitung\n\n"
            "Some prose ends without a terminat\n"
            "```\n"
        )
        cut = detect_cut_point(
            body, expected_sections=2,
            section_budgets={1: 400, 2: 400},
        )
        assert cut is not None
        assert cut["mode"] == "truncated"

    def test_total_target_min_via_total_budget_only(self):
        # No section_budgets, just total budget — total is 200 of 1000 → fire
        content = _build_complete_doc([200])
        cut = detect_cut_point(
            content, expected_sections=1,
            total_word_budget=(1000, 1500),
        )
        assert cut is not None
        assert cut["mode"] == "underbudget"


class TestUnderBudgetPrompt:
    def _underbudget_cut(self, is_last=True):
        from services.writing_continuation import SectionInfo
        return {
            "mode": "underbudget",
            "last_complete_section_index": 2,
            "last_partial_section": None,
            "missing_section_indices": [],
            "partial_tail": "der letzte Satz endet hier mit einem Punkt.",
            "expected_sections": 2,
            "is_last_section": is_last,
            "underbudget_section": SectionInfo(
                index=2, title="Fazit",
                start_offset=0, end_offset=0,
                words=120, is_complete=True,
            ),
            "underbudget_target": 400,
            "total_actual": 600,
            "total_target_min": 1000,
        }

    def test_german_expand_prompt_avoids_truncation_language(self):
        prompt = build_continuation_prompt(
            self._underbudget_cut(), language_code="de"
        )
        assert "abgeschnitten" not in prompt.lower(), (
            "expand-mode prompt should not say 'truncated'"
        )
        assert "Token-Limit" not in prompt
        assert "Abschnitt 2" in prompt
        assert "Fazit" in prompt
        assert "AN ORT UND STELLE" in prompt
        assert "KEINE neue Überschrift" in prompt

    def test_english_expand_prompt_avoids_truncation_language(self):
        prompt = build_continuation_prompt(
            self._underbudget_cut(), language_code="en"
        )
        assert "truncated" not in prompt.lower()
        assert "section 2" in prompt.lower()
        assert "in place" in prompt.lower()
        assert "do not emit a new heading" in prompt.lower()

    def test_expand_prompt_includes_shortfall_estimate(self):
        prompt = build_continuation_prompt(
            self._underbudget_cut(), language_code="en"
        )
        # actual=120, target=400 → shortfall 280
        assert "280" in prompt

    def test_expand_prompt_does_not_emit_new_doc_wrapper_instruction(self):
        prompt = build_continuation_prompt(
            self._underbudget_cut(), language_code="en"
        )
        assert "content-block:document" in prompt
        assert "Do NOT wrap" in prompt or "Do NOT emit" in prompt


class TestInferLanguageCode:
    def test_german_document_classified_de(self):
        text = (
            "# 1. Einleitung\n\nDie Rolle Chinas in der Weltwirtschaft "
            "zwischen Angebot und Nachfrage ist für die deutsche Wirtschaft "
            "zentral, nicht zuletzt aufgrund der Abbildungen und Diagramme "
            "aus den Quellen."
        )
        assert infer_language_code(text) == "de"

    def test_english_document_classified_en(self):
        text = (
            "# 1. Introduction\n\nThe role of China in the world economy "
            "is central for the German economy, between supply and demand, "
            "with figures and diagrams drawn from the sources."
        )
        assert infer_language_code(text) == "en"

    def test_scoped_to_document_block_ignoring_refs_json(self):
        """The live-run bug: refs block JSON has English field names
        (entry_key, reference_type, authors, title) that pollute a
        German document's English-marker count."""
        response = (
            "```content-block:references\n"
            '[\n'
            '  {"entry_key": "x-2024", "title": "Some Title",\n'
            '   "reference_type": "web", "url": "https://example.com",\n'
            '   "authors": [{"family": "X", "given": "Y"}]},\n'
            '  {"entry_key": "y-2024", "title": "Another Title",\n'
            '   "reference_type": "document", "publisher": "Vahlen",\n'
            '   "authors": [{"family": "Müller", "given": "P."}]}\n'
            "]\n"
            "```\n\n"
            "```content-block:document\n"
            "# 1. Einleitung\n\n"
            "Die Rolle Chinas in der Weltwirtschaft zwischen Angebot und "
            "Nachfrage ist für die deutsche Wirtschaft zentral aufgrund "
            "der Abbildungen und Diagramme aus den Quellen.\n"
            "```"
        )
        assert infer_language_code(response) == "de"

    def test_empty_defaults_to_english(self):
        assert infer_language_code("") == "en"
        assert infer_language_code(None) == "en"


class TestStitchContinuation:
    def test_appends_continuation_into_document_block(self):
        cut = detect_cut_point(TRUNCATED_MID_SECTION_4, expected_sections=5)
        continuation = "ens to run with. Here's the rest.\n\n# 5. Conclusion\n\nFinal.\n"
        stitched = stitch_continuation(TRUNCATED_MID_SECTION_4, continuation, cut)
        assert "content-block:document" in stitched
        # Only ONE document block after stitch
        assert stitched.count("```content-block:document") == 1
        # Original content preserved
        assert "Detailed analysis ending properly" in stitched
        # Continuation spliced in
        assert "ens to run with" in stitched
        assert "# 5. Conclusion" in stitched

    def test_strips_continuation_wrapper_if_llm_emits_one(self):
        cut = detect_cut_point(TRUNCATED_MID_SECTION_4, expected_sections=5)
        wrapped = (
            "```content-block:document\n"
            "ens.\n\n# 5. Conclusion\n\nDone.\n"
            "```"
        )
        stitched = stitch_continuation(TRUNCATED_MID_SECTION_4, wrapped, cut)
        assert stitched.count("```content-block:document") == 1

    def test_joiner_avoids_word_fusion_after_alphanum_cut(self):
        cut = detect_cut_point(TRUNCATED_MID_SECTION_4, expected_sections=5)
        # Continuation starts with a word character; body ends with "tok"
        stitched = stitch_continuation(TRUNCATED_MID_SECTION_4, "ens.", cut)
        assert "tok ens." in stitched or "tokens." in stitched  # either a space or no joiner; just check we don't have "tokens" collapsed weirdly
        assert "tokens.ens" not in stitched

    def test_empty_continuation_noop(self):
        cut = detect_cut_point(TRUNCATED_MID_SECTION_4, expected_sections=5)
        assert stitch_continuation(TRUNCATED_MID_SECTION_4, "", cut) == TRUNCATED_MID_SECTION_4

    def test_preserves_references_block_outside_document(self):
        content = (
            "```content-block:references\n[{\"entry_key\": \"x\"}]\n```\n\n"
            "```content-block:document\n# 1. Intro\n\nFoo\n```"
        )
        cut = detect_cut_point(content, expected_sections=1)
        # Cut detector won't return None here because "Foo" doesn't end in
        # sentence terminator; force-stitch and confirm refs block preserved
        if cut:
            stitched = stitch_continuation(content, "continuation text.", cut)
            assert "content-block:references" in stitched
            assert '"entry_key": "x"' in stitched


# ---------------------------------------------------------------------------
# Worst-shortfall selection (mid-body splicing prerequisite)
# ---------------------------------------------------------------------------


class TestWorstShortfallSelection:
    """detect_cut_point must pick the section with the largest absolute
    word shortfall (not the last section, not the lowest index by default).
    Ties broken by lowest index."""

    def test_worst_section_picked_over_last(self):
        # sec 3 shortfall=700, sec 5 shortfall=300 → sec 3 wins
        content = _build_complete_doc([400, 600, 100, 800, 100])
        cut = detect_cut_point(
            content, expected_sections=5,
            section_budgets={1: 400, 2: 600, 3: 800, 4: 800, 5: 400},
        )
        assert cut is not None
        assert cut["mode"] == "underbudget"
        assert cut["underbudget_section"].index == 3
        assert cut["underbudget_target"] == 800
        assert cut["is_last_section"] is False

    def test_tie_broken_by_lowest_index(self):
        # sec 1 and 2 both shortfall=300 → sec 1 wins
        content = _build_complete_doc([100, 100, 800])
        cut = detect_cut_point(
            content, expected_sections=3,
            section_budgets={1: 400, 2: 400, 3: 800},
        )
        assert cut is not None
        assert cut["underbudget_section"].index == 1

    def test_last_section_underbudget_keeps_tail_append(self):
        # Only sec 5 underbudget → last → tail-append
        content = _build_complete_doc([400, 600, 800, 800, 100])
        cut = detect_cut_point(
            content, expected_sections=5,
            section_budgets={1: 400, 2: 600, 3: 800, 4: 800, 5: 400},
        )
        assert cut is not None
        assert cut["underbudget_section"].index == 5
        assert cut["is_last_section"] is True

    def test_small_target_sections_excluded_from_selection(self):
        # sec 1 has target<200 (below trigger); sec 2 has 600 → sec 2 picks
        content = _build_complete_doc([20, 100, 800])
        cut = detect_cut_point(
            content, expected_sections=3,
            section_budgets={1: 100, 2: 600, 3: 800},  # 1 < threshold
        )
        assert cut is not None
        assert cut["underbudget_section"].index == 2

    def test_total_budget_only_falls_back_to_last(self):
        # No section budgets → total drives detection → last section is target
        content = _build_complete_doc([100, 100])
        cut = detect_cut_point(
            content, expected_sections=2,
            total_word_budget=(1000, 1500),
        )
        assert cut is not None
        assert cut["underbudget_section"].index == 2
        assert cut["is_last_section"] is True


# ---------------------------------------------------------------------------
# Mid-body splice: stitch_continuation insertion before next heading
# ---------------------------------------------------------------------------


class TestMidBodySplice:
    """stitch_continuation must insert continuation prose BEFORE the next
    section's heading when cut targets a non-last section."""

    def _doc_with_three_sections(self):
        return (
            "```content-block:document\n"
            "# 1. Intro\nFirst section ends.\n\n"
            "# 2. Body\nSecond section ends.\n\n"
            "# 3. Fazit\nThird section ends.\n"
            "```\n"
        )

    def _mid_cut(self, target_idx: int):
        from services.writing_continuation import SectionInfo
        return {
            "mode": "underbudget",
            "underbudget_section": SectionInfo(
                index=target_idx, title=f"Section{target_idx}",
                start_offset=0, end_offset=0, words=10, is_complete=True,
            ),
            "underbudget_target": 800,
            "is_last_section": False,
            "partial_tail": "Second section ends.",
        }

    def test_splice_lands_before_next_heading(self):
        original = self._doc_with_three_sections()
        cut = self._mid_cut(target_idx=2)
        stitched = stitch_continuation(
            original, "EXTRA BODY PROSE.", cut
        )
        assert "EXTRA BODY PROSE." in stitched
        # Order: section 2's prose → EXTRA → section 3's heading
        i_existing = stitched.index("Second section ends.")
        i_extra = stitched.index("EXTRA BODY PROSE.")
        i_next_head = stitched.index("# 3. Fazit")
        assert i_existing < i_extra < i_next_head

    def test_next_section_heading_not_duplicated(self):
        original = self._doc_with_three_sections()
        cut = self._mid_cut(target_idx=2)
        stitched = stitch_continuation(original, "EXTRA.", cut)
        assert stitched.count("# 3. Fazit") == 1

    def test_target_section_heading_not_duplicated(self):
        original = self._doc_with_three_sections()
        cut = self._mid_cut(target_idx=2)
        stitched = stitch_continuation(original, "EXTRA.", cut)
        assert stitched.count("# 2. Body") == 1

    def test_subsequent_sections_preserved_intact(self):
        original = self._doc_with_three_sections()
        cut = self._mid_cut(target_idx=2)
        stitched = stitch_continuation(original, "EXTRA.", cut)
        # Section 3's prose still there
        assert "Third section ends." in stitched

    def test_last_section_underbudget_falls_back_to_tail_append(self):
        original = self._doc_with_three_sections()
        from services.writing_continuation import SectionInfo
        cut = {
            "mode": "underbudget",
            "underbudget_section": SectionInfo(
                index=3, title="Fazit",
                start_offset=0, end_offset=0, words=10, is_complete=True,
            ),
            "is_last_section": True,
            "partial_tail": "Third section ends.",
        }
        stitched = stitch_continuation(original, "FAZIT EXTRA.", cut)
        # Tail-append: extra appears AFTER last section's existing prose,
        # BEFORE the closing ``` fence
        i_existing = stitched.index("Third section ends.")
        i_extra = stitched.index("FAZIT EXTRA.")
        assert i_existing < i_extra
        # Closing fence still present
        assert stitched.endswith("```\n")

    def test_truncated_mode_still_uses_tail_append(self):
        # Even with target_index set, mode=truncated forces tail-append
        original = self._doc_with_three_sections()
        from services.writing_continuation import SectionInfo
        cut = {
            "mode": "truncated",
            "last_complete_section_index": 2,
            "last_partial_section": SectionInfo(
                index=3, title="Fazit", start_offset=0, end_offset=0,
                words=5, is_complete=False,
            ),
            "missing_section_indices": [],
            "partial_tail": "Third section ends.",
        }
        stitched = stitch_continuation(original, "MORE PROSE.", cut)
        # Continuation appears at the end, not before any heading
        assert stitched.index("MORE PROSE.") > stitched.index("Third section ends.")
        # Heading order preserved
        assert stitched.index("# 1. Intro") < stitched.index("# 2. Body") < stitched.index("# 3. Fazit")

    def test_splice_preserves_figure_markdown_before_next_heading(self):
        # Realistic case: section 2 has a figure markdown at its tail
        original = (
            "```content-block:document\n"
            "# 1. Intro\nfirst.\n\n"
            "# 2. Body\nbody prose.\n\n"
            "![Abb 1](/api/documents/images/x/y.png)\n"
            "*Abbildung 1: Caption.*\n\n"
            "# 3. Fazit\nlast prose.\n"
            "```\n"
        )
        from services.writing_continuation import SectionInfo
        cut = {
            "mode": "underbudget",
            "underbudget_section": SectionInfo(
                index=2, title="Body", start_offset=0, end_offset=0,
                words=20, is_complete=True,
            ),
            "is_last_section": False,
            "partial_tail": "*Abbildung 1: Caption.*",
        }
        stitched = stitch_continuation(original, "EXTRA INSIGHT.", cut)
        # Figure + caption stay BEFORE the # 3 heading. The expansion
        # lands AFTER the figure (between caption and # 3).
        assert "Abbildung 1" in stitched
        assert stitched.index("Abbildung 1") < stitched.index("EXTRA INSIGHT.") < stitched.index("# 3. Fazit")


# ---------------------------------------------------------------------------
# Integration: run_continuations multi-section underbudget recovery
# ---------------------------------------------------------------------------


class _StubResponse:
    def __init__(self, content: str, finish_reason: str = "stop"):
        self.choices = [
            type(
                "Choice",
                (),
                {
                    "message": type("Msg", (), {"content": content})(),
                    "finish_reason": finish_reason,
                },
            )()
        ]


class _ScriptedDispatcher:
    """Returns canned continuation responses in order."""

    def __init__(self, responses: list[str]):
        self._responses = list(responses)
        self.calls: list[dict] = []

    async def dispatch(self, *, messages, agent_mode=None, **kwargs):
        self.calls.append({"messages": messages, "agent_mode": agent_mode})
        if not self._responses:
            return _StubResponse(""), {}
        text = self._responses.pop(0)
        return _StubResponse(text, finish_reason="stop"), {"provider": "stub"}


class TestRunContinuationsUnderBudgetRecovery:
    """Live-scenario integration: planner sets per-section budgets, writer
    produces a complete-but-thin draft, helper picks worst-shortfall
    section, mid-body splices an expansion, re-detects, repeats until
    on-budget or budget exhausted."""

    def _initial_thin_draft(self):
        # Sec 3 is the worst (target 800, actual 100 → shortfall 700).
        # Sec 2 next worst (target 600, actual 200 → shortfall 400).
        body = (
            "# 1. Einleitung\n" + ("lorem " * 280).strip() + ".\n\n"
            "# 2. Theorie\n" + ("lorem " * 200).strip() + ".\n\n"
            "# 3. Analyse\n" + ("lorem " * 100).strip() + ".\n\n"
            "# 4. Diskussion\n" + ("lorem " * 700).strip() + ".\n\n"
            "# 5. Fazit\n" + ("lorem " * 350).strip() + "."
        )
        return f"```content-block:document\n{body}\n```\n"

    def test_picks_worst_section_first_then_re_detects(self):
        budgets = {1: 400, 2: 600, 3: 800, 4: 800, 5: 400}
        # Two responses: one big enough to lift sec 3 over threshold,
        # one for sec 2 (next worst).
        sec3_expansion = ("expanded analysis " * 350).strip() + "."
        sec2_expansion = ("expanded theory " * 250).strip() + "."
        dispatcher = _ScriptedDispatcher([sec3_expansion, sec2_expansion])

        import asyncio
        final_content, telemetry = asyncio.run(
            run_continuations(
                initial_content=self._initial_thin_draft(),
                finish_reason="stop",
                base_messages=[{"role": "system", "content": "stub"}],
                dispatcher=dispatcher,
                agent_mode="simplified_writing",
                max_attempts=2,
                expected_sections=5,
                section_budgets=budgets,
                total_word_budget=(2960, 3300),
                language_code="de",
            )
        )

        # Both expansions should have fired (total of 2 calls)
        assert len(dispatcher.calls) == 2
        # Telemetry: initial mode is underbudget (no truncation upstream)
        assert telemetry["initial_mode"] == "underbudget"
        # Both expansions land in the body; sec 3's lands BEFORE
        # sec 4 heading; sec 2's lands BEFORE sec 3 heading
        assert "expanded analysis" in final_content
        assert "expanded theory" in final_content
        # Section heading order preserved (no duplicates)
        for h in ("# 1. Einleitung", "# 2. Theorie", "# 3. Analyse",
                  "# 4. Diskussion", "# 5. Fazit"):
            assert final_content.count(h) == 1, f"heading dup: {h}"

    def test_outcome_underbudget_resolved_when_budget_met(self):
        # Tight setup: only sec 3 underbudget; one expansion clears total + section
        budgets = {1: 400, 2: 600, 3: 800, 4: 800, 5: 400}
        body = (
            "# 1. Einleitung\n" + ("lorem " * 400).strip() + ".\n\n"
            "# 2. Theorie\n" + ("lorem " * 500).strip() + ".\n\n"
            "# 3. Analyse\n" + ("lorem " * 100).strip() + ".\n\n"
            "# 4. Diskussion\n" + ("lorem " * 800).strip() + ".\n\n"
            "# 5. Fazit\n" + ("lorem " * 400).strip() + "."
        )
        initial = f"```content-block:document\n{body}\n```\n"
        # Initial total = 2200 words; 0.85 × 2960 = 2516 → underbudget total
        # Expanding sec 3 by 800 words → total 3000, sec 3 = 900 → all on-budget
        big_expansion = ("expanded prose " * 800).strip() + "."
        dispatcher = _ScriptedDispatcher([big_expansion])

        import asyncio
        final_content, telemetry = asyncio.run(
            run_continuations(
                initial_content=initial,
                finish_reason="stop",
                base_messages=[{"role": "system", "content": "stub"}],
                dispatcher=dispatcher,
                agent_mode="simplified_writing",
                max_attempts=2,
                expected_sections=5,
                section_budgets=budgets,
                total_word_budget=(2960, 3300),
                language_code="de",
            )
        )
        assert telemetry["outcome"] == "underbudget_resolved"

    def test_outcome_underbudget_unfilled_after_exhaustion(self):
        budgets = {1: 400, 2: 600, 3: 800, 4: 800, 5: 400}
        # Both expansions are tiny — nowhere near budget
        dispatcher = _ScriptedDispatcher(["tiny.", "tinier."])

        import asyncio
        _, telemetry = asyncio.run(
            run_continuations(
                initial_content=self._initial_thin_draft(),
                finish_reason="stop",
                base_messages=[{"role": "system", "content": "stub"}],
                dispatcher=dispatcher,
                agent_mode="simplified_writing",
                max_attempts=2,
                expected_sections=5,
                section_budgets=budgets,
                total_word_budget=(2960, 3300),
                language_code="de",
            )
        )
        assert telemetry["outcome"] == "underbudget_unfilled"
        assert telemetry["last_observed_mode"] == "underbudget"

    def test_warning_copy_matches_underbudget_failure(self):
        # Truncation warning would say "Token-Limit"; underbudget should NOT
        budgets = {1: 400, 2: 600, 3: 800, 4: 800, 5: 400}
        dispatcher = _ScriptedDispatcher(["tiny.", "tinier."])

        import asyncio
        final_content, _ = asyncio.run(
            run_continuations(
                initial_content=self._initial_thin_draft(),
                finish_reason="stop",
                base_messages=[{"role": "system", "content": "stub"}],
                dispatcher=dispatcher,
                agent_mode="simplified_writing",
                max_attempts=2,
                expected_sections=5,
                section_budgets=budgets,
                total_word_budget=(2960, 3300),
                language_code="de",
            )
        )
        assert "Token-Limit" not in final_content
        # Underbudget warning should be present at the bottom
        assert "Wortbudget" in final_content or "geplanten" in final_content
