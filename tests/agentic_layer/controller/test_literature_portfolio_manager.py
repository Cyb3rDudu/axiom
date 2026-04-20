"""Tests for the LiteraturePortfolioManager — compliance + markdown rendering.

Agent LLM calls are not exercised here. We test the deterministic parts:
compliance scoring, markdown structure, section-referencing logic, and the
default-on/opt-out gating via `_is_enabled`.
"""

from __future__ import annotations

# Prime the codebase's fragile import graph — see
# tests/agentic_layer/test_source_quality.py for the rationale.
import api as _api_primer  # noqa: F401  # isort: skip

import types
from typing import Any, Dict, List

import pytest

from ai_researcher.agentic_layer.controller.literature_portfolio_manager import (
    LiteraturePortfolioManager,
)
from ai_researcher.agentic_layer.schemas.portfolio import (
    PortfolioEntry,
    QualitySignals,
)


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _make_entry(
    *,
    sid: str,
    apa: str = "Author, A. (2024). Title.",
    tool: str = "Axiom Local Library (RAG)",
    scientific_tier: str = "A",
    publisher_tier: str = "A",
    publication_type: str = "peer_reviewed_journal",
    peer_reviewed: bool = True,
    recency_years: int | None = 2,
) -> PortfolioEntry:
    return PortfolioEntry(
        source_id=sid,
        apa_citation=apa,
        discovery_tool=tool,
        relevance_bullets=["Relevanter Beitrag."],
        quality_bullets=["Peer-reviewte Zeitschrift."],
        quality_signals=QualitySignals(
            publication_type=publication_type,  # type: ignore[arg-type]
            peer_reviewed=peer_reviewed,
            publisher_tier=publisher_tier,  # type: ignore[arg-type]
            recency_years=recency_years,
        ),
        sections_used_in=[],
        contribution_type="theory",
        scientific_tier=scientific_tier,  # type: ignore[arg-type]
    )


def _manager() -> LiteraturePortfolioManager:
    return LiteraturePortfolioManager(controller=types.SimpleNamespace())


# ---------------------------------------------------------------------------
# enabled / opt-out
# ---------------------------------------------------------------------------


def test_is_enabled_default_on_when_no_settings() -> None:
    mission = types.SimpleNamespace(mission_settings=None)
    assert LiteraturePortfolioManager._is_enabled(mission) is True


def test_is_enabled_default_on_when_no_deliverables_key() -> None:
    mission = types.SimpleNamespace(mission_settings={"use_web_search": True})
    assert LiteraturePortfolioManager._is_enabled(mission) is True


def test_is_enabled_explicit_false_respected() -> None:
    mission = types.SimpleNamespace(
        mission_settings={"deliverables": {"literature_portfolio": False}}
    )
    assert LiteraturePortfolioManager._is_enabled(mission) is False


def test_is_enabled_explicit_true() -> None:
    mission = types.SimpleNamespace(
        mission_settings={"deliverables": {"literature_portfolio": True}}
    )
    assert LiteraturePortfolioManager._is_enabled(mission) is True


# ---------------------------------------------------------------------------
# compliance
# ---------------------------------------------------------------------------


def test_compliance_green_for_balanced_portfolio() -> None:
    entries = [_make_entry(sid=f"d{i}") for i in range(12)]
    mgr = _manager()
    report = mgr._compute_compliance(entries, language_code="de")
    assert report.source_count == 12
    assert report.source_count_ok is True
    assert report.scientific_share == 1.0
    assert report.scientific_share_ok is True
    assert report.traffic_light == "green"
    assert report.blacklist_hits == []
    assert report.advice == []


def test_compliance_yellow_for_too_few_sources() -> None:
    entries = [_make_entry(sid=f"d{i}") for i in range(6)]
    report = _manager()._compute_compliance(entries, language_code="de")
    assert report.source_count_ok is False
    assert report.scientific_share_ok is True  # all Tier A
    assert report.traffic_light == "yellow"
    assert any("Quellenanzahl" in a or "below target" in a for a in report.advice)


def test_compliance_red_for_low_scientific_share() -> None:
    entries = (
        [_make_entry(sid=f"a{i}") for i in range(4)]
        + [
            _make_entry(
                sid=f"d{i}",
                scientific_tier="D",
                publisher_tier="D",
                publication_type="industry_report",
                peer_reviewed=False,
            )
            for i in range(8)
        ]
    )
    report = _manager()._compute_compliance(entries, language_code="de")
    assert report.scientific_share == pytest.approx(4 / 12)
    assert report.scientific_share_ok is False
    assert report.traffic_light == "red"


def test_compliance_red_for_blacklist_hit() -> None:
    entries = [_make_entry(sid=f"d{i}") for i in range(11)]
    entries.append(
        _make_entry(
            sid="wiki",
            apa="Wikipedia. (2025). Globalisation. https://en.wikipedia.org/wiki/Globalisation",
            scientific_tier="D",
            publisher_tier="blacklist",
            publication_type="web_page",
            peer_reviewed=False,
        )
    )
    report = _manager()._compute_compliance(entries, language_code="de")
    assert "Wikipedia" in report.blacklist_hits[0]
    assert report.traffic_light == "red"


def test_compliance_recency_warning() -> None:
    entries = [
        _make_entry(sid="fresh", recency_years=3),
        _make_entry(sid="old1", recency_years=25),
        _make_entry(sid="old2", recency_years=40),
    ]
    report = _manager()._compute_compliance(entries, language_code="de")
    assert len(report.recency_warnings) == 2


# ---------------------------------------------------------------------------
# markdown rendering
# ---------------------------------------------------------------------------


def test_markdown_contains_kmu_headers_de() -> None:
    entries = [_make_entry(sid="d1")]
    report = _manager()._compute_compliance(entries, language_code="de")
    md = LiteraturePortfolioManager._render_markdown(
        entries, report, language_code="de"
    )
    assert "## Literaturportfolio" in md
    assert "Quellenangabe (lt. Literaturverzeichnis)" in md
    assert "Recherchetool" in md
    assert "Relevanz" in md
    assert "Qualität" in md
    assert "Compliance-Ampel" in md
    # Traffic light emoji present
    assert any(e in md for e in ("🟢", "🟡", "🔴"))


def test_markdown_contains_english_headers() -> None:
    entries = [_make_entry(sid="d1")]
    report = _manager()._compute_compliance(entries, language_code="en")
    md = LiteraturePortfolioManager._render_markdown(
        entries, report, language_code="en"
    )
    assert "## Literature Portfolio" in md
    assert "Source (as in bibliography)" in md
    assert "Compliance traffic light" in md


def test_markdown_escapes_pipes_in_citations() -> None:
    entry = _make_entry(
        sid="tricky",
        apa="Weird | Source | Name with pipes",
    )
    report = _manager()._compute_compliance([entry], language_code="de")
    md = LiteraturePortfolioManager._render_markdown(
        [entry], report, language_code="de"
    )
    # Escaped pipes in the table cell should not break the column count.
    body_line = [ln for ln in md.splitlines() if "tricky" in ln or "Weird" in ln]
    assert body_line, "citation row missing"
    # Count unescaped pipes on the row — should be exactly 5 (table delimiters)
    row = body_line[0]
    # Replace escaped pipes temporarily
    unescaped = row.replace(r"\|", "")
    assert unescaped.count("|") == 5


# ---------------------------------------------------------------------------
# section referencing
# ---------------------------------------------------------------------------


def test_sections_referencing_finds_exact_ids() -> None:
    report_content = {
        "sec_1": "Some background context. [docA] further discussion.",
        "sec_2": "Pure prose with no citations.",
        "sec_3": "Direct empirical finding [docA]. Comparison with [docB].",
    }
    hits_a = LiteraturePortfolioManager._sections_referencing(
        "docA", report_content=report_content
    )
    hits_b = LiteraturePortfolioManager._sections_referencing(
        "docB", report_content=report_content
    )
    assert set(hits_a) == {"sec_1", "sec_3"}
    assert hits_b == ["sec_3"]


def test_snippets_around_returns_window() -> None:
    text = "A" * 300 + "[doc_xyz]" + "B" * 300
    snippets = LiteraturePortfolioManager._snippets_around(
        source_id="doc_xyz",
        sections_used_in=["sec_1"],
        report_content={"sec_1": text},
        window=50,
    )
    assert len(snippets) == 1
    assert snippets[0]["section_id"] == "sec_1"
    assert "[doc_xyz]" in snippets[0]["snippet"]
    # Total snippet length is clipped to about 2*window + placeholder
    assert len(snippets[0]["snippet"]) <= 50 + len("[doc_xyz]") + 50 + 10
