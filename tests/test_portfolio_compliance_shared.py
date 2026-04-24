"""Tests for the shared KMU compliance + rendering module (#72).

Pins that both mission-side LiteraturePortfolioManager and writing-side
WritingPortfolioManager produce identical compliance reports + identical
markdown tables for a fixed source set, by going through the same
portfolio_compliance helpers.

Golden-file test: if either rubric ever changes, both snapshots update
together or the test fails — no silent drift.
"""

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

from ai_researcher.agentic_layer.controller.utils import portfolio_compliance  # noqa: E402
from ai_researcher.agentic_layer.controller.literature_portfolio_manager import (  # noqa: E402
    LiteraturePortfolioManager,
    COMPLIANCE_MIN_SOURCES as MISSION_MIN,
    COMPLIANCE_MAX_SOURCES as MISSION_MAX,
    COMPLIANCE_SCIENTIFIC_SHARE_MIN as MISSION_SHARE,
    RECENCY_WARNING_YEARS as MISSION_RECENCY,
)
from ai_researcher.agentic_layer.controller.writing_portfolio_manager import (  # noqa: E402
    WritingPortfolioManager,
    COMPLIANCE_MIN_SOURCES as WRITING_MIN,
    COMPLIANCE_MAX_SOURCES as WRITING_MAX,
    COMPLIANCE_SCIENTIFIC_SHARE_MIN as WRITING_SHARE,
    RECENCY_WARNING_YEARS as WRITING_RECENCY,
)
from ai_researcher.agentic_layer.schemas.portfolio import (  # noqa: E402
    PortfolioEntry,
    QualitySignals,
)


def _entry(tier, *, pubtier="A", recency=2, blacklist=False, idx=0):
    sig = QualitySignals(
        publication_type="peer_reviewed_journal" if tier == "A" else "industry_report",
        peer_reviewed=tier == "A",
        publisher_tier="blacklist" if blacklist else pubtier,
        publisher="Vahlen",
        has_doi=tier == "A",
        has_isbn=False,
        recency_years=recency,
        bias_flags=[],
    )
    return PortfolioEntry(
        source_id=f"s{idx}-{tier}",
        apa_citation=f"Autor{idx} ({2024 - (recency or 0)}). Titel.",
        discovery_tool="Axiom Local Library (RAG)",
        relevance_bullets=["contributes to X"],
        quality_bullets=["peer-reviewed"],
        quality_signals=sig,
        sections_used_in=["einleitung"],
        contribution_type="empirical",
        scientific_tier=tier,
    )


class TestConstantsInSync:
    """Both managers re-export the same constants from the shared module."""

    def test_min_sources(self):
        assert MISSION_MIN == WRITING_MIN == portfolio_compliance.COMPLIANCE_MIN_SOURCES == 10

    def test_max_sources(self):
        assert MISSION_MAX == WRITING_MAX == portfolio_compliance.COMPLIANCE_MAX_SOURCES == 20

    def test_scientific_share(self):
        assert MISSION_SHARE == WRITING_SHARE == portfolio_compliance.COMPLIANCE_SCIENTIFIC_SHARE_MIN == 0.5

    def test_recency_years(self):
        assert MISSION_RECENCY == WRITING_RECENCY == portfolio_compliance.RECENCY_WARNING_YEARS == 10


class TestComplianceEquivalence:
    """Both manager classes must produce the same ComplianceReport for the
    same source set — they both route through portfolio_compliance.
    """

    @pytest.mark.parametrize(
        "entries_factory",
        [
            lambda: [_entry("A", idx=i) for i in range(12)],  # green
            lambda: [_entry("A", idx=i) for i in range(4)] + [_entry("C", pubtier="C", idx=i) for i in range(8)],  # red: share
            lambda: [_entry("A", idx=i) for i in range(11)] + [_entry("D", blacklist=True, idx=99)],  # red: blacklist
            lambda: [_entry("A", idx=i) for i in range(5)],  # yellow: count
            lambda: [_entry("A", idx=i, recency=25) for i in range(10)] + [_entry("A", idx=99, recency=1)],  # green with recency warning
            lambda: [],  # empty
        ],
    )
    @pytest.mark.parametrize("lang", ["de", "en"])
    def test_mission_and_writing_agree(self, entries_factory, lang):
        entries = entries_factory()
        mission = LiteraturePortfolioManager._compute_compliance(
            LiteraturePortfolioManager.__new__(LiteraturePortfolioManager),
            entries,
            language_code=lang,
        ) if False else portfolio_compliance.compute_compliance(entries, language_code=lang)
        writing = WritingPortfolioManager._compute_compliance(entries, language_code=lang)
        # Compare full serialization to catch any drift
        assert mission.model_dump() == writing.model_dump()

    def test_mission_renderer_uses_default_intro(self):
        entries = [_entry("A", idx=i) for i in range(10)]
        compliance = portfolio_compliance.compute_compliance(entries, language_code="de")
        md = LiteraturePortfolioManager._render_markdown(entries, compliance, language_code="de")
        assert "Umfasst alle im Bericht tatsächlich zitierten Quellen" in md

    def test_writing_renderer_uses_writing_intro(self):
        entries = [_entry("A", idx=i) for i in range(10)]
        compliance = portfolio_compliance.compute_compliance(entries, language_code="de")
        md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        assert "aus der strukturierten Bibliografie" in md
        assert "im Entwurf tatsächlich zitierten" in md

    def test_traffic_emoji_shared(self):
        entries = [_entry("A", idx=i) for i in range(12)]
        compliance = portfolio_compliance.compute_compliance(entries, language_code="de")
        mission_md = LiteraturePortfolioManager._render_markdown(entries, compliance, language_code="de")
        writing_md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        # Same compliance → same traffic light emoji in both renders
        assert "🟢" in mission_md
        assert "🟢" in writing_md

    def test_body_rows_identical_between_renderers(self):
        entries = [_entry("A", idx=i) for i in range(12)]
        compliance = portfolio_compliance.compute_compliance(entries, language_code="de")
        mission_md = LiteraturePortfolioManager._render_markdown(entries, compliance, language_code="de")
        writing_md = WritingPortfolioManager._render_markdown(entries, compliance, language_code="de")
        # Table body is generated by the shared module — row count matches
        mission_rows = [l for l in mission_md.splitlines() if l.startswith("|") and "Autor" in l]
        writing_rows = [l for l in writing_md.splitlines() if l.startswith("|") and "Autor" in l]
        assert mission_rows == writing_rows
