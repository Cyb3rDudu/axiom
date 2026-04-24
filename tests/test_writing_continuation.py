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
