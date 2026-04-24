"""Tests for WritingPortfolioManager pure helpers (#61/#65).

Covers compliance rubric + markdown rendering + language resolution +
merge logic. The agent-calling + DB-persisting path is integration-level
and not exercised here.
"""

from __future__ import annotations

import sys
from pathlib import Path
from types import SimpleNamespace

_PROJECT_ROOT = Path(__file__).resolve().parents[1]
_BACKEND_ROOT = _PROJECT_ROOT / "axiom_backend"
for _p in (_BACKEND_ROOT, _PROJECT_ROOT):
    if str(_p) not in sys.path:
        sys.path.insert(0, str(_p))

import pytest  # noqa: E402

import api as _api_primer  # noqa: F401, E402

from ai_researcher.agentic_layer.controller.writing_portfolio_manager import (  # noqa: E402
    _clip_bullets,
    _fallback_quality,
    _fallback_relevance,
    _resolve_language,
    WritingPortfolioManager,
)
from ai_researcher.agentic_layer.schemas.portfolio import (  # noqa: E402
    PortfolioEntry,
    QualitySignals,
)


def _entry(tier, *, pubtier="A", recency=2, blacklist=False):
    sig = QualitySignals(
        publication_type="peer_reviewed_journal" if tier in ("A",) else "industry_report",
        peer_reviewed=tier == "A",
        publisher_tier="blacklist" if blacklist else pubtier,
        journal_name=None,
        publisher="Vahlen",
        has_doi=tier == "A",
        has_isbn=False,
        recency_years=recency,
        bias_flags=[],
    )
    return PortfolioEntry(
        source_id=f"s-{tier}-{recency}",
        apa_citation=f"Autor ({2024 - (recency or 0)}). Titel.",
        discovery_tool="Axiom Local Library (RAG)",
        relevance_bullets=["contributes to X"],
        quality_bullets=["peer-reviewed"],
        quality_signals=sig,
        sections_used_in=["einleitung"],
        contribution_type="empirical",
        scientific_tier=tier,
    )


class TestClipBullets:
    def test_clips_to_three(self):
        assert _clip_bullets(["a", "b", "c", "d", "e"]) == ["a", "b", "c"]

    def test_drops_empty_strings(self):
        assert _clip_bullets(["a", "", " ", "b"]) == ["a", "b"]

    def test_non_list_returns_empty(self):
        assert _clip_bullets(None) == []
        assert _clip_bullets("string") == []


class TestFallbackBullets:
    def test_relevance_de(self):
        rec = {"sections_used_in": ["intro"]}
        out = _fallback_relevance(rec, "de")
        assert "1 Abschnitt" in out
        assert "manuell" in out

    def test_relevance_en(self):
        rec = {"sections_used_in": []}
        out = _fallback_relevance(rec, "en")
        assert "filled in manually" in out

    def test_quality_includes_tier_and_type(self):
        rec = {
            "quality_signals": {
                "publisher_tier": "B",
                "publication_type": "industry_report",
            }
        }
        out = _fallback_quality(rec, "de")
        assert "B" in out
        assert "industry_report" in out


class TestResolveLanguage:
    def _user(self, settings=None):
        return SimpleNamespace(settings=settings or {})

    def test_default_de(self):
        assert _resolve_language(self._user(), {}) == "de"

    def test_session_overrides_user(self):
        user = self._user({"language_code": "de"})
        session = {"language_code": "en"}
        assert _resolve_language(user, session) == "en"

    def test_user_writing_settings_picked_up(self):
        user = self._user({"writing_settings": {"language_code": "en"}})
        assert _resolve_language(user, {}) == "en"

    def test_unknown_language_falls_back_to_de(self):
        user = self._user({"language_code": "fr"})
        assert _resolve_language(user, {}) == "de"


class TestComputeCompliance:
    def test_green_when_all_criteria_met(self):
        entries = [_entry("A") for _ in range(12)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert report.traffic_light == "green"
        assert report.source_count_ok
        assert report.scientific_share_ok
        assert report.scientific_share == 1.0

    def test_red_when_too_few_sources(self):
        # 5 A-tier: count fails, share fine → yellow (count but not red)
        entries = [_entry("A") for _ in range(5)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert report.source_count_ok is False
        # Not blacklist, share ok → yellow
        assert report.traffic_light == "yellow"

    def test_red_when_scientific_share_below_50(self):
        # 7 A, 5 C → 7/12 = 58% scientific, 12 sources → green
        # Make it 4 A, 8 C → 33% → red
        entries = [_entry("A") for _ in range(4)] + [_entry("C", pubtier="C") for _ in range(8)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert report.scientific_share < 0.5
        assert report.traffic_light == "red"

    def test_red_when_blacklist_hit(self):
        entries = [_entry("A") for _ in range(11)] + [_entry("D", blacklist=True)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert report.blacklist_hits
        assert report.traffic_light == "red"

    def test_recency_warning_does_not_make_red(self):
        entries = [_entry("A", recency=1) for _ in range(10)] + [_entry("A", recency=25)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert len(report.recency_warnings) == 1
        # Source count = 11, share 100%, no blacklist → green even with old entry
        assert report.traffic_light == "green"

    def test_empty_entries(self):
        report = WritingPortfolioManager._compute_compliance([], language_code="de")
        assert report.source_count == 0
        assert report.scientific_share == 0.0
        assert report.traffic_light == "red"  # 0 < min, share 0 < 50%

    def test_advice_in_german(self):
        entries = [_entry("A") for _ in range(5)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        assert any("Quellenanzahl" in a for a in report.advice)

    def test_advice_in_english(self):
        entries = [_entry("A") for _ in range(5)]
        report = WritingPortfolioManager._compute_compliance(entries, language_code="en")
        assert any("Source count" in a for a in report.advice)


class TestRenderMarkdown:
    def test_german_heading(self):
        entries = [_entry("A") for _ in range(10)]
        compliance = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        assert md.startswith("## Literaturportfolio")
        assert "Quellenangabe" in md
        assert "Recherchetool" in md

    def test_english_heading(self):
        entries = [_entry("A") for _ in range(10)]
        compliance = WritingPortfolioManager._compute_compliance(entries, language_code="en")
        md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="en")
        assert md.startswith("## Literature Portfolio")
        assert "Discovery tool" in md

    def test_traffic_emoji_present(self):
        entries = [_entry("A") for _ in range(12)]
        compliance = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        assert "🟢" in md

    def test_empty_pipes_are_escaped_in_cells(self):
        entries = [_entry("A")]
        # Inject a pipe into apa_citation to stress-test escaping
        entries[0].apa_citation = "Titel with | pipe"
        compliance = WritingPortfolioManager._compute_compliance(entries, language_code="de")
        md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        assert "\\|" in md  # escaped pipe survived


class TestMergeAgentOutput:
    def test_agent_bullets_used_when_present(self):
        recs = [
            {
                "source_id": "s1",
                "apa_citation": "x",
                "discovery_tool": "tool",
                "quality_signals": QualitySignals().model_dump(),
                "scientific_tier": "B",
                "sections_used_in": ["intro"],
            }
        ]
        agent = [
            {
                "source_id": "s1",
                "relevance_bullets": ["relevant because X"],
                "quality_bullets": ["good quality"],
                "contribution_type": "theory",
            }
        ]
        out = WritingPortfolioManager._merge_agent_output(
            agent_entries=agent, source_records=recs, language_code="de"
        )
        assert len(out) == 1
        assert out[0].relevance_bullets == ["relevant because X"]
        assert out[0].contribution_type == "theory"

    def test_falls_back_when_agent_silent(self):
        recs = [
            {
                "source_id": "s1",
                "apa_citation": "x",
                "discovery_tool": "tool",
                "quality_signals": QualitySignals().model_dump(),
                "scientific_tier": "B",
                "sections_used_in": ["intro"],
            }
        ]
        out = WritingPortfolioManager._merge_agent_output(
            agent_entries=[], source_records=recs, language_code="de"
        )
        assert len(out) == 1
        assert out[0].relevance_bullets  # non-empty fallback
        assert out[0].contribution_type == "background"

    def test_invalid_contribution_type_defaults(self):
        recs = [
            {
                "source_id": "s1",
                "apa_citation": "x",
                "discovery_tool": "t",
                "quality_signals": QualitySignals().model_dump(),
                "scientific_tier": "B",
                "sections_used_in": [],
            }
        ]
        agent = [
            {
                "source_id": "s1",
                "relevance_bullets": ["x"],
                "quality_bullets": ["y"],
                "contribution_type": "nonsense",
            }
        ]
        out = WritingPortfolioManager._merge_agent_output(
            agent_entries=agent, source_records=recs, language_code="de"
        )
        assert out[0].contribution_type == "background"
