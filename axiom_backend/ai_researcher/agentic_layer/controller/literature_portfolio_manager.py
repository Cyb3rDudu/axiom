"""
Orchestrates Literaturportfolio generation for a finished mission.

Called from `report_generator.process_citations` right before the mission is
marked completed. Uses the citation set already computed by that function,
plus the in-memory Note list, to produce a `PortfolioOutput` — the
`markdown_table` field is appended to the final report; the full object is
persisted to `missions.literature_portfolio_output` (JSONB).

The manager purposefully keeps all policy (tier thresholds, blacklist
handling, compliance rules) here and not inside the agent — the agent only
does KMU-style prose. That way the rubric is testable and deterministic.
"""

from __future__ import annotations

import logging
from datetime import datetime
from typing import Any, Callable, Dict, Iterable, List, Optional, Set, Tuple

from ai_researcher.agentic_layer.controller.utils import portfolio_compliance
from ai_researcher.agentic_layer.schemas.notes import Note
from ai_researcher.agentic_layer.schemas.portfolio import (
    ComplianceReport,
    PortfolioEntry,
    PortfolioOutput,
    QualitySignals,
)
from ai_researcher.agentic_layer.services.source_quality import (
    assign_scientific_tier,
    compute_quality_signals,
    discovery_tool_label,
    extract_apa_citation,
)

logger = logging.getLogger(__name__)


# Compliance thresholds live in portfolio_compliance.py (#72). Re-exported
# here as module-level constants so existing callers + tests keep working
# — do not duplicate values below.
COMPLIANCE_MIN_SOURCES = portfolio_compliance.COMPLIANCE_MIN_SOURCES
COMPLIANCE_MAX_SOURCES = portfolio_compliance.COMPLIANCE_MAX_SOURCES
COMPLIANCE_SCIENTIFIC_SHARE_MIN = portfolio_compliance.COMPLIANCE_SCIENTIFIC_SHARE_MIN
RECENCY_WARNING_YEARS = portfolio_compliance.RECENCY_WARNING_YEARS


class LiteraturePortfolioManager:
    """Builds a Literaturportfolio for a mission, using its context + citation set."""

    def __init__(self, controller: Any):
        self.controller = controller

    # ----- public entry point -----

    async def run_if_enabled(
        self,
        mission_id: str,
        full_draft: str,
        used_doc_ids: Set[str],
        doc_metadata_source: Dict[str, Note],
        *,
        log_queue: Optional[Any] = None,
        update_callback: Optional[Callable[..., Any]] = None,
    ) -> Optional[PortfolioOutput]:
        """Generate and persist a PortfolioOutput if the mission has the
        deliverable enabled. Returns None otherwise."""
        mission = self.controller.context_manager.get_mission_context(mission_id)
        if mission is None:
            logger.warning("PortfolioManager: mission %s not found", mission_id)
            return None

        if not self._is_enabled(mission):
            logger.info("PortfolioManager: disabled for mission %s (opt-out)", mission_id)
            return None

        if not used_doc_ids:
            logger.info("PortfolioManager: no cited sources for mission %s — skipping", mission_id)
            return None

        language_code = getattr(mission, "language_code", None) or "de"
        if hasattr(mission, "mission_settings") and isinstance(mission.mission_settings, dict):
            language_code = mission.mission_settings.get("language_code") or language_code
        language_code = language_code[:2].lower() if language_code else "de"
        if language_code not in {"de", "en"}:
            logger.info("PortfolioManager: falling back to 'en' for language %s", language_code)
            language_code = "en"

        source_records = self._build_source_records(
            used_doc_ids=used_doc_ids,
            doc_metadata_source=doc_metadata_source,
            mission_settings=getattr(mission, "mission_settings", None) or {},
            full_draft=full_draft,
            report_content=mission.report_content or {},
        )

        # Call agent for KMU-style prose (Relevanz/Qualität bullets).
        # Imported lazily to avoid triggering the agents package init on
        # lightweight call paths (e.g. unit tests of manager internals).
        from ai_researcher.agentic_layer.agents.literature_portfolio_agent import (
            LiteraturePortfolioAgent,
        )
        agent = LiteraturePortfolioAgent(
            model_dispatcher=self.controller.model_dispatcher,
            controller=self.controller,
            language_code=language_code,
        )
        try:
            agent_result, _model_details, _scratch = await agent.run(
                mission_id=mission_id,
                mission_goal=mission.user_request,
                source_records=source_records,
                language_code=language_code,
                log_queue=log_queue,
                update_callback=update_callback,
            )
        except Exception as exc:  # noqa: BLE001
            logger.error("PortfolioManager: agent run failed: %s", exc, exc_info=True)
            agent_result = {"entries": []}

        entries = self._merge_agent_output(
            agent_entries=(agent_result or {}).get("entries", []),
            source_records=source_records,
            language_code=language_code,
        )

        compliance = self._compute_compliance(entries, language_code=language_code)
        markdown_table = self._render_markdown(entries, compliance, language_code=language_code)

        output = PortfolioOutput(
            mission_id=mission_id,
            language_code=language_code,  # type: ignore[arg-type]
            generated_at=datetime.utcnow(),
            entries=entries,
            compliance=compliance,
            markdown_table=markdown_table,
        )

        await self._persist(mission_id, output)
        return output

    # ----- helpers -----

    @staticmethod
    def _is_enabled(mission: Any) -> bool:
        """Default ON. Opt-out via mission_settings.deliverables.literature_portfolio."""
        settings = getattr(mission, "mission_settings", None)
        if not isinstance(settings, dict):
            return True
        deliverables = settings.get("deliverables")
        if not isinstance(deliverables, dict):
            return True
        flag = deliverables.get("literature_portfolio")
        if flag is None:
            return True
        return bool(flag)

    def _build_source_records(
        self,
        *,
        used_doc_ids: Set[str],
        doc_metadata_source: Dict[str, Note],
        mission_settings: Dict[str, Any],
        full_draft: str,
        report_content: Dict[str, str],
    ) -> List[Dict[str, Any]]:
        """For each cited source, build a dict to hand to the agent.

        The agent receives the APA citation, discovery tool, quality signals,
        pre-assigned scientific tier, the section IDs where the source is
        cited, and small contextual snippets (the sentences around the
        citation placeholder) so it can write informed relevance bullets.
        """
        records: List[Dict[str, Any]] = []
        for source_id in sorted(used_doc_ids):
            note = doc_metadata_source.get(source_id)
            if note is None:
                # Build a degraded record so compliance still reflects it.
                records.append(
                    {
                        "source_id": source_id,
                        "apa_citation": f"Unknown Source ({source_id})",
                        "discovery_tool": "Unknown",
                        "quality_signals": QualitySignals().model_dump(),
                        "scientific_tier": "C",
                        "sections_used_in": [],
                        "section_context_snippets": [],
                    }
                )
                continue

            signals = compute_quality_signals(note)
            tier = assign_scientific_tier(signals)
            sections_used_in = self._sections_referencing(
                source_id=source_id, report_content=report_content
            )
            records.append(
                {
                    "source_id": source_id,
                    "apa_citation": extract_apa_citation(note),
                    "discovery_tool": discovery_tool_label(note, mission_settings),
                    "quality_signals": signals.model_dump(),
                    "scientific_tier": tier,
                    "sections_used_in": sections_used_in,
                    "section_context_snippets": self._snippets_around(
                        source_id=source_id,
                        sections_used_in=sections_used_in,
                        report_content=report_content,
                    ),
                }
            )
        return records

    @staticmethod
    def _sections_referencing(source_id: str, report_content: Dict[str, str]) -> List[str]:
        """Return section IDs whose content contains the bare source_id
        (citation placeholder) — cheap but reliable, since the writer uses
        `[source_id]` markers."""
        needle = source_id
        hits: List[str] = []
        for section_id, text in report_content.items():
            if text and needle in text:
                hits.append(section_id)
        return hits

    @staticmethod
    def _snippets_around(
        *,
        source_id: str,
        sections_used_in: Iterable[str],
        report_content: Dict[str, str],
        window: int = 180,
    ) -> List[Dict[str, str]]:
        snippets: List[Dict[str, str]] = []
        for section_id in sections_used_in:
            text = report_content.get(section_id) or ""
            idx = text.find(source_id)
            if idx == -1:
                continue
            start = max(0, idx - window)
            end = min(len(text), idx + window)
            snippets.append({"section_id": section_id, "snippet": text[start:end]})
        return snippets

    def _merge_agent_output(
        self,
        *,
        agent_entries: List[Dict[str, Any]],
        source_records: List[Dict[str, Any]],
        language_code: str,
    ) -> List[PortfolioEntry]:
        """Combine the agent's prose (relevance/quality bullets, contribution_type)
        with the manager-computed signals. We trust the manager's tier, APA
        string, and signals; we trust the agent's bullets."""
        agent_by_id: Dict[str, Dict[str, Any]] = {
            e.get("source_id"): e for e in agent_entries if isinstance(e, dict) and e.get("source_id")
        }
        merged: List[PortfolioEntry] = []
        for rec in source_records:
            sid = rec["source_id"]
            agent_entry = agent_by_id.get(sid, {})

            relevance = agent_entry.get("relevance_bullets") or [
                self._fallback_relevance_bullet(rec, language_code)
            ]
            quality = agent_entry.get("quality_bullets") or [
                self._fallback_quality_bullet(rec, language_code)
            ]
            contribution = agent_entry.get("contribution_type") or "background"
            # Clip list lengths to schema (1-3)
            relevance = [b for b in relevance if isinstance(b, str) and b.strip()][:3] or [
                self._fallback_relevance_bullet(rec, language_code)
            ]
            quality = [b for b in quality if isinstance(b, str) and b.strip()][:3] or [
                self._fallback_quality_bullet(rec, language_code)
            ]

            try:
                entry = PortfolioEntry(
                    source_id=sid,
                    apa_citation=rec["apa_citation"],
                    discovery_tool=rec["discovery_tool"],
                    relevance_bullets=relevance,
                    quality_bullets=quality,
                    quality_signals=QualitySignals(**rec["quality_signals"]),
                    sections_used_in=rec.get("sections_used_in") or agent_entry.get("sections_used_in", []),
                    contribution_type=contribution if contribution in {
                        "theory", "empirical", "background", "counter_position",
                        "definition", "data_source", "practice",
                    } else "background",
                    scientific_tier=rec["scientific_tier"],
                )
                merged.append(entry)
            except Exception as exc:  # noqa: BLE001
                logger.warning("PortfolioManager: dropping source %s — %s", sid, exc)
        return merged

    @staticmethod
    def _fallback_relevance_bullet(rec: Dict[str, Any], language_code: str) -> str:
        sections = rec.get("sections_used_in") or []
        n = len(sections)
        if language_code.startswith("de"):
            return (
                f"In {n} Abschnitt(en) zitiert; konkreter Beitrag bitte manuell ergänzen."
                if n
                else "Konkreter Beitrag bitte manuell ergänzen."
            )
        return (
            f"Cited in {n} section(s); specific contribution to be filled in manually."
            if n
            else "Specific contribution to be filled in manually."
        )

    @staticmethod
    def _fallback_quality_bullet(rec: Dict[str, Any], language_code: str) -> str:
        sig = rec.get("quality_signals") or {}
        tier = sig.get("publisher_tier") or "unknown"
        ptype = sig.get("publication_type") or "unknown"
        if language_code.startswith("de"):
            return f"Publisher-Tier: {tier}; Publikationstyp: {ptype}"
        return f"Publisher tier: {tier}; publication type: {ptype}"

    # ----- compliance + markdown (delegated to shared module, #72) -----

    def _compute_compliance(
        self, entries: List[PortfolioEntry], *, language_code: str
    ) -> ComplianceReport:
        """Thin wrapper; delegates to portfolio_compliance.compute_compliance."""
        return portfolio_compliance.compute_compliance(
            entries, language_code=language_code
        )

    @staticmethod
    def _render_markdown(
        entries: List[PortfolioEntry],
        compliance: ComplianceReport,
        *,
        language_code: str,
    ) -> str:
        """Mission-side renderer uses the default intro from the shared
        module ('Automatisch erstellt. Umfasst alle im Bericht …'); the
        writing-mode manager passes a writing-specific intro through the
        same helper."""
        return portfolio_compliance.render_markdown(
            entries, compliance, language_code=language_code
        )

    # ----- persistence -----

    async def _persist(self, mission_id: str, output: PortfolioOutput) -> None:
        """Write the PortfolioOutput JSON to the missions table. Silently
        swallow DB errors — portfolio generation must never fail the mission."""
        try:
            from database import async_crud
            from database.async_database import get_async_db

            async with get_async_db() as db:
                await async_crud.update_mission_literature_portfolio_output(
                    db,
                    mission_id=mission_id,
                    portfolio_output=output.model_dump(mode="json"),
                )
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "PortfolioManager: failed to persist portfolio for %s: %s",
                mission_id,
                exc,
            )
