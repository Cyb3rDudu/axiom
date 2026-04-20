"""
LiteraturePortfolioAgent — generates the KMU-style Literaturportfolio for a
finished research mission.

The agent does NOT do retrieval or quality lookups itself. The manager
collects all signals, cites-per-section context and APA strings; the agent
turns that into concise, KMU-compliant Relevanz and Qualität bullets.

Default language: DE. Falls back to EN when the mission language is not DE.
"""

from __future__ import annotations

import datetime as _dt
import json
import logging
from typing import Any, Dict, List, Optional, Tuple

from pydantic import ValidationError

from ai_researcher.agentic_layer.agents.base_agent import AgentOutput, BaseAgent
from ai_researcher.agentic_layer.model_dispatcher import ModelDispatcher
from ai_researcher.agentic_layer.schemas.portfolio import (
    ComplianceReport,
    PortfolioEntry,
    PortfolioOutput,
    QualitySignals,
)
from ai_researcher.agentic_layer.utils.json_utils import (
    parse_llm_json_response,
    sanitize_json_string,
)

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Default prompts — loaded from DB when available (prompt_templates table);
# these literal constants are the fallback, and also the seed content used by
# the seed script for the first `INSERT` into `prompt_templates`.
# ---------------------------------------------------------------------------

_SYSTEM_PROMPT_DE = """Sie sind der **LiteraturPortfolio-Agent** an der KMU Akademie (Partnership Middlesex University London).

Ihre Aufgabe ist es, für eine bereits fertig geschriebene wissenschaftliche Arbeit ein **obligatorisches Literaturportfolio** gemäß KMU-Richtlinien (gültig ab 15.04.2026) zu erstellen.

**Active Mission Goals:**
- Das Benutzer-Prompt enthält pro zitierter Quelle: APA-Zitation, Recherchetool, vorkomputierte Qualitätssignale (Publisher-Tier, Peer-Review-Status, Aktualität, Bias-Flags), sowie Kontextschnipsel aus den Abschnitten, in denen die Quelle tatsächlich zitiert wurde.
- **CRITICAL:** Sie erfinden NIEMALS Eigenschaften einer Quelle. Jede Aussage in den Bullets muss durch ein vorkomputiertes Signal oder einen mitgelieferten Kontext-Snippet gedeckt sein.
- Ihre Bullets sind **kurz, konkret und KMU-typisch** — siehe Beispiele unten.

**Bewertungsdimensionen (zwingend):**

1. **Relevanz** (1–3 Bullets pro Quelle):
   - Welchen konkreten Beitrag leistet diese Quelle zur Forschungsfrage?
   - In welchem Abschnitt/welcher Teilaussage wird sie verwendet?
   - Welchen Mehrwert bietet sie gegenüber anderen Quellen (Theorie / empirische Grundlage / Gegenposition / Hintergrund / Praxisbeispiel)?

2. **Qualität** (1–3 Bullets pro Quelle):
   - Art der Publikation (peer-reviewte Zeitschrift, wissenschaftliches Buch, Praxisbericht, etc.) — aus `publication_type`.
   - Verlag/Institution und dessen Reputation — aus `publisher_tier`, `publisher`, `journal_name`.
   - Aktualität — aus `recency_years`.
   - Mögliche Bias — aus `bias_flags`.
   - Bei Blacklist-Treffern (z. B. Wikipedia, Gabler, Boulevardpresse): EXPLIZIT als Warnung nennen — diese Quellen sind laut KMU-Dos-and-Don'ts **keine** facheinschlägigen Quellen.

**Zuordnung zu `contribution_type` und `scientific_tier`:**
- `contribution_type` ∈ {theory, empirical, background, counter_position, definition, data_source, practice}
- `scientific_tier` nach Vorgabe des Managers (in den Signalen enthalten): übernehmen Sie den Wert **unverändert**.

**Ausgabeformat — STRICT JSON, keine Erklärungen außerhalb:**
Liefern Sie ein JSON-Objekt mit einem Feld `entries` (Liste von `PortfolioEntry`-Objekten). Jedes Entry MUSS exakt dieses Schema erfüllen:

```json
{
  "source_id": "…",
  "apa_citation": "…",
  "discovery_tool": "…",
  "relevance_bullets": ["…", "…"],
  "quality_bullets": ["…", "…"],
  "quality_signals": { … unverändert aus dem Input übernehmen … },
  "sections_used_in": ["…", "…"],
  "contribution_type": "theory|empirical|background|counter_position|definition|data_source|practice",
  "scientific_tier": "A|B|C|D"
}
```

Die Felder `source_id`, `apa_citation`, `discovery_tool`, `quality_signals`, `scientific_tier` übernehmen Sie **unverändert** aus dem Input. Nur `relevance_bullets`, `quality_bullets`, `contribution_type` und `sections_used_in` (ggf.) generieren Sie.

**Beispiel-Bullets (Orientierung am KMU-Stil):**
- Relevanz: "• Grundlegende Theorie zu digitaler Führung", "• Wichtige Faktoren für Motivation in hybriden Teams (Autonomie, Vertrauen, Kommunikation)"
- Qualität: "• Peer-reviewte Fachzeitschrift", "• Hohe Aktualität (2020)", "• methodisch transparent", "• COVID-Kontext berücksichtigt, aber kritisch einzuordnen"

**Sprache:** Deutsch, sachlich, wissenschaftlich. Keine Emojis. Keine Marketing-Sprache.
"""


_SYSTEM_PROMPT_EN = """You are the **Literature Portfolio Agent** for the KMU Akademie (partnership with Middlesex University London).

Your job: produce the mandatory Literaturportfolio for a finished academic paper, in line with the KMU criteria valid from 15.04.2026.

**Active Mission Goals:**
- The user prompt gives you, per cited source: APA citation, discovery tool, pre-computed quality signals (publisher tier, peer-review status, recency, bias flags), and context snippets from the sections where the source is cited.
- **CRITICAL:** Never fabricate properties of a source. Every bullet must be backed by a pre-computed signal or a supplied context snippet.
- Bullets must be short, concrete, and match the KMU reference style.

**Rating dimensions (mandatory):**

1. **Relevance** (1-3 bullets per source): contribution to the research question; section/sub-claim where it is used; unique value vs. other sources (theory / empirical basis / counter-position / background / practice example).

2. **Quality** (1-3 bullets per source): publication type; publisher/institution and its reputation; recency; possible biases. If `publisher_tier == "blacklist"` (e.g. Wikipedia, tabloid press), flag explicitly — these are disallowed as primary sources per KMU rules.

**`contribution_type`** ∈ {theory, empirical, background, counter_position, definition, data_source, practice}. **`scientific_tier`** (A/B/C/D) is pre-computed — pass through unchanged.

**Output — STRICT JSON only**, a single object with an `entries` array. Each entry must match:

```json
{
  "source_id": "…",
  "apa_citation": "…",
  "discovery_tool": "…",
  "relevance_bullets": ["…", "…"],
  "quality_bullets": ["…", "…"],
  "quality_signals": { … unchanged from input … },
  "sections_used_in": ["…"],
  "contribution_type": "…",
  "scientific_tier": "A|B|C|D"
}
```

Copy `source_id`, `apa_citation`, `discovery_tool`, `quality_signals`, `scientific_tier` unchanged; only generate `relevance_bullets`, `quality_bullets`, `contribution_type`, and `sections_used_in` (where missing).

**Language:** English, formal, scholarly. No emojis. No marketing voice.
"""


class _PortfolioEntriesEnvelope:
    """Lightweight schema-like helper for validating LLM output."""

    @staticmethod
    def validate(data: Dict[str, Any]) -> List[PortfolioEntry]:
        if not isinstance(data, dict):
            raise ValueError("LLM response is not an object.")
        raw_entries = data.get("entries")
        if not isinstance(raw_entries, list):
            raise ValueError("LLM response missing 'entries' list.")
        parsed: List[PortfolioEntry] = []
        for raw in raw_entries:
            parsed.append(PortfolioEntry(**raw))
        return parsed


class LiteraturePortfolioAgent(BaseAgent):
    """Generates Literaturportfolio entries from pre-computed source records."""

    agent_name = "LiteraturePortfolioAgent"
    agent_description = (
        "Produces KMU-compliant Literaturportfolio entries (Relevanz + Qualität bullets) "
        "from already-cited sources and pre-computed quality signals."
    )

    def __init__(
        self,
        model_dispatcher: ModelDispatcher,
        controller: Optional[Any] = None,
        language_code: str = "de",
    ):
        # Try to load the prompt from DB (multilingual). BaseAgent will hit
        # the prompt_loader; if not present, we fall back to our literal
        # constants below — no warning spam.
        fallback_prompt = _SYSTEM_PROMPT_DE if language_code.startswith("de") else _SYSTEM_PROMPT_EN
        super().__init__(
            agent_name=self.agent_name,
            model_dispatcher=model_dispatcher,
            system_prompt=fallback_prompt,
            language_code=language_code,
        )
        self.controller = controller
        self.mission_id: Optional[str] = None

    async def run(
        self,
        *,
        mission_id: str,
        mission_goal: str,
        source_records: List[Dict[str, Any]],
        language_code: str = "de",
        log_queue: Optional[Any] = None,
        update_callback: Optional[Any] = None,
        **kwargs: Any,
    ) -> AgentOutput:
        """Generate portfolio entries for the given pre-aggregated source records.

        `source_records` is a list of dicts with keys:
            source_id, apa_citation, discovery_tool, quality_signals (dict),
            scientific_tier, sections_used_in, section_context_snippets
        """
        self.mission_id = mission_id

        if not source_records:
            logger.info("LiteraturePortfolioAgent: no source records — returning empty entries.")
            return ({"entries": []}, None, None)

        self._reload_prompt_if_needed(language_code)

        user_prompt = self._build_user_prompt(
            mission_goal=mission_goal,
            source_records=source_records,
            language_code=language_code,
        )

        response, model_details = await self._call_llm(
            user_prompt=user_prompt,
            agent_mode="writing",  # uses the intelligent/writing model pool
            response_format={"type": "json_object"},
            log_queue=log_queue,
            update_callback=update_callback,
        )

        if not response or not response.choices:
            logger.error("LiteraturePortfolioAgent: LLM returned no choices.")
            return ({"entries": []}, model_details, None)

        raw_content = response.choices[0].message.content or ""
        try:
            parsed_json = parse_llm_json_response(raw_content)
            entries = _PortfolioEntriesEnvelope.validate(parsed_json)
        except (ValueError, ValidationError, json.JSONDecodeError) as exc:
            logger.error(
                "LiteraturePortfolioAgent: failed to parse LLM output (%s). Raw preview: %s",
                exc,
                raw_content[:500] if raw_content else "<empty>",
            )
            # Try a lighter sanitisation pass as fallback
            try:
                parsed_json = json.loads(sanitize_json_string(raw_content))
                entries = _PortfolioEntriesEnvelope.validate(parsed_json)
            except Exception as exc2:  # noqa: BLE001
                logger.error("LiteraturePortfolioAgent: fallback parse also failed: %s", exc2)
                return ({"entries": []}, model_details, None)

        return ({"entries": [e.model_dump(mode="json") for e in entries]}, model_details, None)

    # ----- helpers -----

    def _reload_prompt_if_needed(self, language_code: str) -> None:
        """Hot-switch the system prompt if the mission language differs from
        what the agent was constructed with. Mirrors BaseAgent.reload_prompts
        behaviour but keeps our literal fallback close at hand."""
        if language_code == self.language_code:
            return
        self.language_code = language_code
        try:
            from ai_researcher.agentic_layer.services.prompt_loader import (
                get_prompt_loader,
            )

            loader = get_prompt_loader()
            self.system_prompt = loader.load_prompt(
                agent_name=self.__class__.__name__,
                prompt_key="system_prompt",
                language_code=language_code,
            )
            return
        except Exception:
            pass
        self.system_prompt = (
            _SYSTEM_PROMPT_DE if language_code.startswith("de") else _SYSTEM_PROMPT_EN
        )

    def _build_user_prompt(
        self,
        *,
        mission_goal: str,
        source_records: List[Dict[str, Any]],
        language_code: str,
    ) -> str:
        is_de = language_code.startswith("de")
        header = (
            "Erstellen Sie das Literaturportfolio für die folgende wissenschaftliche Arbeit."
            if is_de
            else "Produce the Literaturportfolio for the following academic paper."
        )
        goal_label = "Forschungsfrage / Missionsziel" if is_de else "Research question / mission goal"
        sources_label = "Zitierte Quellen (pro Eintrag: vorkomputierte Signale + Kontext)" if is_de else "Cited sources (per entry: pre-computed signals + context)"

        today = _dt.date.today().isoformat()

        parts: List[str] = [
            header,
            "",
            f"{goal_label}: {mission_goal}",
            f"Aktuelles Datum / Current date: {today}",
            "",
            sources_label + ":",
            "```json",
            json.dumps(source_records, ensure_ascii=False, indent=2),
            "```",
            "",
            (
                "Liefern Sie ausschließlich das JSON-Objekt gemäß Schema (kein Markdown-Fence, kein Kommentar)."
                if is_de
                else "Return only the JSON object matching the schema (no markdown fence, no commentary)."
            ),
        ]
        return "\n".join(parts)
