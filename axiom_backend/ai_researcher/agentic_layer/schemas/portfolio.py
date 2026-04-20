"""
Schemas for the Literaturportfolio deliverable (KMU Akademie compliance).

The portfolio is generated after citation processing and captures, per cited
source: APA citation, discovery tool, 1-3 relevance bullets (how the source
was used), 1-3 quality bullets (scientific weight), signal-backed quality
dimensions, and the sections in which the source was cited. A compliance
report covers the KMU "≥ 50 % wissenschaftlich / 10-20 Quellen / keine
Blacklist-Treffer" rules and emits a traffic light.
"""

from __future__ import annotations

from datetime import datetime
from typing import ClassVar, List, Literal, Optional

from pydantic import BaseModel, ConfigDict, Field


PublicationType = Literal[
    "peer_reviewed_journal",
    "monograph_scientific_publisher",
    "edited_book",
    "conference_proceedings",
    "working_paper",
    "preprint",
    "industry_report",
    "whitepaper",
    "news_article",
    "blog",
    "data_portal",
    "standard",
    "legal_document",
    "web_page",
    "unknown",
]

PublisherTier = Literal["A", "B", "C", "D", "blacklist", "unknown"]

ScientificTier = Literal["A", "B", "C", "D"]

ContributionType = Literal[
    "theory",
    "empirical",
    "background",
    "counter_position",
    "definition",
    "data_source",
    "practice",
]

TrafficLight = Literal["green", "yellow", "red"]


class QualitySignals(BaseModel):
    """Pre-computed, objective signals the agent reasons over."""

    publication_type: PublicationType = "unknown"
    peer_reviewed: Optional[bool] = None
    publisher_tier: PublisherTier = "unknown"
    journal_name: Optional[str] = None
    publisher: Optional[str] = None
    has_doi: bool = False
    has_isbn: bool = False
    recency_years: Optional[int] = None
    author_credentials_note: Optional[str] = None
    bias_flags: List[str] = Field(default_factory=list)

    model_config: ClassVar[ConfigDict] = ConfigDict(extra="forbid")


class PortfolioEntry(BaseModel):
    """One row in the Literaturportfolio table."""

    source_id: str
    apa_citation: str = Field(description="Source rendered in APA 7 style.")
    discovery_tool: str = Field(
        description="Where the source was retrieved from, e.g. 'Local Library (RAG)', 'Google Scholar', 'CrossRef', 'Web Search'."
    )
    relevance_bullets: List[str] = Field(
        description="1-3 short bullets: which section(s) this source supports and what unique contribution it makes.",
        min_length=1,
        max_length=3,
    )
    quality_bullets: List[str] = Field(
        description="1-3 short bullets: scientific weight, peer-review status, currency, biases. Never invent — derive from QualitySignals.",
        min_length=1,
        max_length=3,
    )
    quality_signals: QualitySignals
    sections_used_in: List[str] = Field(default_factory=list)
    contribution_type: ContributionType = "background"
    scientific_tier: ScientificTier = "C"

    model_config: ClassVar[ConfigDict] = ConfigDict(extra="forbid")


class ComplianceReport(BaseModel):
    """KMU threshold check outcome."""

    source_count: int
    source_count_ok: bool = Field(description="True if 10 <= count <= 20")
    scientific_share: float = Field(ge=0.0, le=1.0)
    scientific_share_ok: bool = Field(description="True if scientific_share >= 0.5")
    blacklist_hits: List[str] = Field(default_factory=list)
    recency_warnings: List[str] = Field(default_factory=list)
    traffic_light: TrafficLight
    advice: List[str] = Field(default_factory=list)

    model_config: ClassVar[ConfigDict] = ConfigDict(extra="forbid")


class PortfolioOutput(BaseModel):
    """Full Literaturportfolio deliverable for a mission."""

    mission_id: str
    language_code: Literal["de", "en"] = "de"
    generated_at: datetime = Field(default_factory=datetime.utcnow)
    entries: List[PortfolioEntry] = Field(default_factory=list)
    compliance: ComplianceReport
    markdown_table: str = Field(
        description="Rendered markdown (table + compliance summary), ready to append to the final report."
    )

    model_config: ClassVar[ConfigDict] = ConfigDict(extra="forbid")
