"""Writing-mode Literaturportfolio orchestrator (#61/#65).

Parallel to `LiteraturePortfolioManager` but scoped to a writing-mode
draft instead of a research mission. Reuses the agent unchanged; reads
source signals from the structured registry (`draft_references` +
`citation_entries`) via `services.writing_portfolio_adapter`.

Runs as a single Sonnet-class call per generation (≤20 sources batched
by the agent itself). Persists the output on `drafts.portfolio_output`.
"""

from __future__ import annotations

import logging
from datetime import datetime
from typing import Any, Dict, List, Optional

from sqlalchemy.orm import Session

from ai_researcher.agentic_layer.controller.utils import portfolio_compliance
from ai_researcher.agentic_layer.schemas.portfolio import (
    ComplianceReport,
    PortfolioEntry,
    PortfolioOutput,
    QualitySignals,
)
from database import models

logger = logging.getLogger(__name__)


# Re-export compliance constants + functions from the shared module so
# callers (and older tests) keep working. All thresholds live in
# portfolio_compliance.py now — do not duplicate values here.
COMPLIANCE_MIN_SOURCES = portfolio_compliance.COMPLIANCE_MIN_SOURCES
COMPLIANCE_MAX_SOURCES = portfolio_compliance.COMPLIANCE_MAX_SOURCES
COMPLIANCE_SCIENTIFIC_SHARE_MIN = portfolio_compliance.COMPLIANCE_SCIENTIFIC_SHARE_MIN
RECENCY_WARNING_YEARS = portfolio_compliance.RECENCY_WARNING_YEARS


_VALID_CONTRIBUTIONS = {
    "theory", "empirical", "background", "counter_position",
    "definition", "data_source", "practice",
}


class WritingPortfolioManager:
    """Generate + persist a per-draft PortfolioOutput."""

    def __init__(self, model_dispatcher: Any, db: Session):
        self.model_dispatcher = model_dispatcher
        self.db = db

    # ----- public entry point -----

    async def run_if_enabled(
        self,
        *,
        draft: models.Draft,
        writing_session: models.WritingSession,
        user: models.User,
        trigger: str = "manual",
    ) -> Optional[PortfolioOutput]:
        """Generate a PortfolioOutput for the draft and persist it.

        Gates:
          - structured_bibliography_enabled(user.settings) must be True.
          - portfolio_optout keyword must NOT appear in the session title
            or the session's own portfolio_enabled flag.
          - draft must have ≥1 structured reference (entry_key IS NOT NULL).

        Returns None when any gate fails; the caller treats that as a
        no-op, not an error.
        """
        from services.feature_flags import structured_bibliography_enabled
        from services.writing_telemetry import record_portfolio_generation
        from ai_researcher.agentic_layer.controller.utils.portfolio_optout import (
            detect_portfolio_optout,
        )

        if not structured_bibliography_enabled(user.settings):
            logger.info("WritingPortfolioManager: flag off for user %s", user.id)
            record_portfolio_generation(
                trigger=trigger, outcome="skipped_flag",
                draft_id=draft.id, user_id=user.id,
            )
            return None

        # Per-session opt-out: the session's `settings.portfolio_enabled`
        # flag wins when explicitly set. Absent → fall back to the
        # keyword detector on the chat title (#68).
        session_settings = writing_session.settings if isinstance(writing_session.settings, dict) else {}
        explicit_flag = session_settings.get("portfolio_enabled") if session_settings else None
        if explicit_flag is False:
            logger.info("WritingPortfolioManager: disabled on session %s", writing_session.id)
            record_portfolio_generation(
                trigger=trigger, outcome="skipped_optout",
                draft_id=draft.id, user_id=user.id,
            )
            return None

        # Keyword check on chat title as secondary signal
        chat_row = (
            self.db.query(models.Chat)
            .filter(models.Chat.id == writing_session.chat_id)
            .first()
        )
        chat_title = chat_row.title if chat_row else ""
        if explicit_flag is None and detect_portfolio_optout(chat_title):
            logger.info(
                "WritingPortfolioManager: opt-out keyword in chat title for session %s",
                writing_session.id,
            )
            record_portfolio_generation(
                trigger=trigger, outcome="skipped_optout",
                draft_id=draft.id, user_id=user.id,
            )
            return None

        refs = (
            self.db.query(models.Reference)
            .filter(
                models.Reference.draft_id == draft.id,
                models.Reference.entry_key.isnot(None),
            )
            .all()
        )
        if not refs:
            logger.info(
                "WritingPortfolioManager: no structured refs for draft %s",
                draft.id,
            )
            record_portfolio_generation(
                trigger=trigger, outcome="skipped_empty",
                draft_id=draft.id, user_id=user.id,
            )
            return None

        occurrences_by_ref: Dict[str, List[models.CitationEntry]] = {}
        for ce in (
            self.db.query(models.CitationEntry)
            .filter(models.CitationEntry.draft_id == draft.id)
            .all()
        ):
            occurrences_by_ref.setdefault(ce.reference_id, []).append(ce)

        language_code = _resolve_language(user, session_settings)
        source_records = self._build_source_records(
            refs=refs,
            occurrences_by_ref=occurrences_by_ref,
            draft_body=draft.content or "",
            session_settings=session_settings,
        )

        # Call the agent — same contract as the mission-side path
        from ai_researcher.agentic_layer.agents.literature_portfolio_agent import (
            LiteraturePortfolioAgent,
        )
        agent = LiteraturePortfolioAgent(
            model_dispatcher=self.model_dispatcher,
            controller=None,
            language_code=language_code,
        )
        mission_goal = (draft.title or chat_title or "").strip() or "Writing session draft"
        try:
            agent_result, _model_details, _scratch = await agent.run(
                mission_id=f"draft:{draft.id}",
                mission_goal=mission_goal,
                source_records=source_records,
                language_code=language_code,
            )
        except Exception as exc:  # noqa: BLE001
            logger.error(
                "WritingPortfolioManager: agent run failed for draft %s: %s",
                draft.id,
                exc,
                exc_info=True,
            )
            agent_result = {"entries": []}

        entries = self._merge_agent_output(
            agent_entries=(agent_result or {}).get("entries", []),
            source_records=source_records,
            language_code=language_code,
        )
        compliance = self._compute_compliance(entries, language_code=language_code)
        markdown_table = self._render_markdown(entries, compliance, language_code=language_code)

        output = PortfolioOutput(
            mission_id=f"draft:{draft.id}",
            language_code=language_code,  # type: ignore[arg-type]
            generated_at=datetime.utcnow(),
            entries=entries,
            compliance=compliance,
            markdown_table=markdown_table,
        )

        self._persist(draft.id, output)
        logger.info(
            "WritingPortfolioManager: portfolio persisted for draft %s (trigger=%s, entries=%d, traffic=%s)",
            draft.id,
            trigger,
            len(entries),
            compliance.traffic_light,
        )
        record_portfolio_generation(
            trigger=trigger,
            outcome="generated",
            traffic_light=compliance.traffic_light,
            draft_id=draft.id,
            user_id=user.id,
            source_count=len(entries),
        )
        return output

    # ----- source record building -----

    @staticmethod
    def _build_source_records(
        *,
        refs: List[models.Reference],
        occurrences_by_ref: Dict[str, List[models.CitationEntry]],
        draft_body: str,
        session_settings: Dict[str, Any],
    ) -> List[Dict[str, Any]]:
        from services.writing_portfolio_adapter import reference_to_source_record

        records: List[Dict[str, Any]] = []
        for ref in refs:
            record = reference_to_source_record(
                ref,
                occurrences_by_ref.get(ref.id, []),
                draft_body,
                session_settings=session_settings,
            )
            records.append(record)
        return records

    # ----- merge + compliance + render (mirrors mission-side logic) -----

    @staticmethod
    def _merge_agent_output(
        *,
        agent_entries: List[Dict[str, Any]],
        source_records: List[Dict[str, Any]],
        language_code: str,
    ) -> List[PortfolioEntry]:
        agent_by_id = {
            e.get("source_id"): e for e in agent_entries
            if isinstance(e, dict) and e.get("source_id")
        }
        merged: List[PortfolioEntry] = []
        for rec in source_records:
            sid = rec["source_id"]
            agent_entry = agent_by_id.get(sid, {})
            relevance = _clip_bullets(agent_entry.get("relevance_bullets")) or [
                _fallback_relevance(rec, language_code)
            ]
            quality = _clip_bullets(agent_entry.get("quality_bullets")) or [
                _fallback_quality(rec, language_code)
            ]
            contribution = agent_entry.get("contribution_type") or "background"
            if contribution not in _VALID_CONTRIBUTIONS:
                contribution = "background"
            try:
                merged.append(
                    PortfolioEntry(
                        source_id=sid,
                        apa_citation=rec["apa_citation"],
                        discovery_tool=rec["discovery_tool"],
                        relevance_bullets=relevance,
                        quality_bullets=quality,
                        quality_signals=QualitySignals(**rec["quality_signals"]),
                        sections_used_in=rec.get("sections_used_in") or agent_entry.get("sections_used_in", []),
                        contribution_type=contribution,
                        scientific_tier=rec["scientific_tier"],
                    )
                )
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    "WritingPortfolioManager: dropping source %s: %s", sid, exc
                )
        return merged

    @staticmethod
    def _compute_compliance(
        entries: List[PortfolioEntry], *, language_code: str
    ) -> ComplianceReport:
        """Thin wrapper kept for test/back-compat; delegates to shared module."""
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
        """Writing-mode renderer uses a writing-specific intro line but
        otherwise shares the table layout + compliance summary with the
        mission-side manager via `portfolio_compliance.render_markdown`."""
        de = language_code.startswith("de")
        intro = (
            "_Automatisch aus der strukturierten Bibliografie erstellt. Umfasst alle im Entwurf tatsächlich zitierten Quellen._"
            if de
            else "_Automatically generated from the structured bibliography. Covers every source actually cited in the draft._"
        )
        return portfolio_compliance.render_markdown(
            entries, compliance, language_code=language_code, intro=intro
        )

    # ----- persistence -----

    def _persist(self, draft_id: str, output: PortfolioOutput) -> None:
        """Write PortfolioOutput JSON to drafts.portfolio_output.

        Sync ORM — the chat-task path + API endpoint both call us with a
        SQLAlchemy Session already in scope. Errors bubble up so the
        endpoint can return 500; background callers swallow them.
        """
        draft = self.db.query(models.Draft).filter(models.Draft.id == draft_id).first()
        if draft is None:
            logger.warning("WritingPortfolioManager: draft %s vanished before persist", draft_id)
            return
        draft.portfolio_output = output.model_dump(mode="json")
        draft.updated_at = datetime.utcnow()
        self.db.commit()


# ---------------------------------------------------------------------------
# Module-level helpers
# ---------------------------------------------------------------------------


def _clip_bullets(bullets: Any) -> List[str]:
    if not isinstance(bullets, list):
        return []
    return [b for b in bullets if isinstance(b, str) and b.strip()][:3]


def _fallback_relevance(rec: Dict[str, Any], language_code: str) -> str:
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


def _fallback_quality(rec: Dict[str, Any], language_code: str) -> str:
    sig = rec.get("quality_signals") or {}
    tier = sig.get("publisher_tier") or "unknown"
    ptype = sig.get("publication_type") or "unknown"
    if language_code.startswith("de"):
        return f"Publisher-Tier: {tier}; Publikationstyp: {ptype}"
    return f"Publisher tier: {tier}; publication type: {ptype}"


def _resolve_language(user: models.User, session_settings: Dict[str, Any]) -> str:
    """Default de; look at session settings then user settings."""
    for source in (session_settings, (user.settings if isinstance(user.settings, dict) else {})):
        if not isinstance(source, dict):
            continue
        code = source.get("language_code") or (
            source.get("writing_settings", {}).get("language_code")
            if isinstance(source.get("writing_settings"), dict)
            else None
        )
        if code:
            code = str(code)[:2].lower()
            if code in {"de", "en"}:
                return code
    return "de"
