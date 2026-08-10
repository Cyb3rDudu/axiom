import logging
import re
from typing import Dict, Any, Optional, List, Callable, Tuple
import queue
import json
from datetime import datetime

from ai_researcher.config import THOUGHT_PAD_CONTEXT_LIMIT
from ai_researcher.agentic_layer.async_context_manager import ExecutionLogEntry
from ai_researcher.agentic_layer.schemas.analysis import RequestAnalysisOutput
from ai_researcher.agentic_layer.utils.json_format_helper import (
    get_json_schema_format,
    get_json_object_format,
    enhance_messages_for_json_object,
    should_retry_with_json_object,
    get_initial_format_mode,
    mark_format_unsupported,
)
from ai_researcher.agentic_layer.utils.json_utils import sanitize_json_string

logger = logging.getLogger(__name__)

# Localized UI status strings appended to chat responses.
# These are controller-level messages (not LLM-generated), so we use a simple dict.
# Falls back to English for unsupported languages.
_UI_STRINGS = {
    "research_starting": {
        "en": "Great! I'll now start the research process with the approved questions. You can monitor the progress in the research tabs.",
        "de": "Sehr gut! Ich starte jetzt den Rechercheprozess mit den genehmigten Fragen. Sie können den Fortschritt in den Recherche-Tabs verfolgen.",
        "fr": "Parfait ! Je lance maintenant le processus de recherche avec les questions approuvées. Vous pouvez suivre la progression dans les onglets de recherche.",
        "es": "¡Perfecto! Ahora iniciaré el proceso de investigación con las preguntas aprobadas. Puede seguir el progreso en las pestañas de investigación.",
        "pt": "Ótimo! Vou iniciar agora o processo de pesquisa com as perguntas aprovadas. Pode acompanhar o progresso nos separadores de investigação.",
    },
    "questions_intro": {
        "en": "Here are some initial research questions to guide our investigation:",
        "de": "Hier sind einige erste Forschungsfragen zur Orientierung unserer Untersuchung:",
        "fr": "Voici quelques questions de recherche initiales pour guider notre investigation :",
        "es": "Aquí hay algunas preguntas de investigación iniciales para guiar nuestra investigación:",
        "pt": "Aqui estão algumas perguntas de pesquisa iniciais para orientar nossa investigação:",
    },
    "questions_prompt": {
        "en": "Would you like to refine these, or shall we proceed?",
        "de": "Möchten Sie diese anpassen, oder sollen wir fortfahren?",
        "fr": "Souhaitez-vous les affiner, ou devons-nous poursuivre ?",
        "es": "¿Le gustaría refinarlas o procedemos?",
        "pt": "Gostaria de refiná-las ou devemos prosseguir?",
    },
    # P2: acknowledgment shown when a user submits a COMPLETE structured
    # briefing (Leitfrage + Gliederung + scope + deliverable). The mission is
    # staged like an open one (Leitfragen become the displayed questions) but
    # NOT auto-started — the user launches it via the settings menu, the Start
    # button, or a chat "start" message, exactly like an open mission.
    "complete_briefing_ack": {
        "en": "I've understood your assignment.",
        "de": "Ich habe deinen Auftrag verstanden.",
        "fr": "J'ai bien compris votre commande.",
        "es": "He entendido su asignación.",
        "pt": "Entendi a sua atribuição.",
    },
    "refine_error": {
        "en": "Sorry, I had trouble refining the questions. Please try again or proceed with the current ones.",
        "de": "Entschuldigung, bei der Überarbeitung der Fragen ist ein Fehler aufgetreten. Bitte versuchen Sie es erneut oder fahren Sie mit den aktuellen Fragen fort.",
        "fr": "Désolé, j'ai eu des difficultés à affiner les questions. Veuillez réessayer ou poursuivre avec les questions actuelles.",
        "es": "Lo siento, tuve problemas para refinar las preguntas. Por favor, intente de nuevo o continúe con las actuales.",
        "pt": "Desculpe, tive problemas ao refinar as perguntas. Por favor, tente novamente ou continue com as atuais.",
    },
    "ready_to_research": {
        "en": "I'm ready to start the research. Please let me know if you have any specific questions you'd like me to focus on, or I can proceed with a general investigation.",
        "de": "Ich bin bereit, mit der Recherche zu beginnen. Lassen Sie mich wissen, ob Sie bestimmte Fragen haben, auf die ich mich konzentrieren soll, oder ich kann mit einer allgemeinen Untersuchung fortfahren.",
        "fr": "Je suis prêt à commencer la recherche. Dites-moi si vous avez des questions spécifiques sur lesquelles vous souhaitez que je me concentre, ou je peux procéder à une investigation générale.",
        "es": "Estoy listo para comenzar la investigación. Hágame saber si tiene preguntas específicas en las que desea que me enfoque, o puedo proceder con una investigación general.",
        "pt": "Estou pronto para iniciar a pesquisa. Diga-me se tem perguntas específicas nas quais gostaria que eu me concentrasse, ou posso prosseguir com uma investigação geral.",
    },
}


def _get_ui_string(key: str, lang_code: str = "en") -> str:
    """Get a localized UI string, falling back to English."""
    strings = _UI_STRINGS.get(key, {})
    return strings.get(lang_code, strings.get("en", ""))


def _detect_language(controller, mission_id: Optional[str] = None, llm_response: Optional[str] = None) -> str:
    """Detect the conversation language.

    Priority:
    1. Mission language_code (explicitly set by user for this research)
    2. Simple heuristic on the LLM response (mirrors the user's chat language)
    3. User profile language
    4. English fallback
    """
    # 1. Try mission language
    if mission_id:
        try:
            mc = controller.context_manager.get_mission_context(mission_id)
            if mc and mc.metadata:
                lang = mc.metadata.get("language_code")
                if lang:
                    return lang
        except Exception:
            pass
    # 2. Heuristic: detect from LLM response (which mirrors user's chat language)
    if llm_response:
        # Check for common German/French/Spanish/Portuguese words in the response
        lower = llm_response[:300].lower()
        if any(w in lower for w in ["ich ", "die ", "der ", "und ", "für ", "dass ", "wird ", "nicht ", "eine ", "kann "]):
            return "de"
        if any(w in lower for w in ["je ", "les ", "des ", "une ", "pour ", "dans ", "qui ", "est "]):
            return "fr"
        if any(w in lower for w in ["las ", "los ", "una ", "para ", "que ", "con ", "del "]):
            return "es"
        if any(w in lower for w in [" os ", " uma ", " para ", " que ", " com ", " das "]):
            return "pt"
    # 3. Try user preference
    try:
        from ai_researcher.user_context import get_current_user
        user = get_current_user()
        if user and hasattr(user, 'language_code') and user.language_code:
            return user.language_code
    except Exception:
        pass
    return "en"


class UserInteractionManager:
    """
    Manages user interactions, including message handling, request analysis,
    and question refinement.
    """
    
    def __init__(self, controller):
        """
        Initialize the UserInteractionManager with a reference to the AgentController.
        
        Args:
            controller: The AgentController instance
        """
        self.controller = controller
        
    async def analyze_request_type(
        self,
        mission_id: str,
        user_request: str,
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, ExecutionLogEntry], None]] = None
    ) -> Optional[RequestAnalysisOutput]:
        """
        Analyzes the user request to determine type, tone, and audience using an LLM call,
        considering any pre-existing goals for the mission.
        """
        logger.info(f"Analyzing request type for mission {mission_id}, considering existing goals...")

        # Fetch existing goals
        active_goals = self.controller.context_manager.get_active_goals(mission_id)
        goals_context = "\nExisting Mission Goals (Consider these, especially user preferences):\n---\n"
        
        # Extract goal texts for both prompt and structured input
        goal_texts = []
        if active_goals:
            for goal in active_goals:
                goal_text = None
                if hasattr(goal, 'text'):
                    goal_text = goal.text
                elif isinstance(goal, str):
                    goal_text = goal
                else:
                    goal_text = str(goal)
                    
                goals_context += f"- {goal_text}\n"
                goal_texts.append(goal_text)
        else:
            goals_context += "No existing goals found.\n"
        goals_context += "---\n"

        # Define the prompt for the analysis LLM call, now including goals
        analysis_prompt = f"""
Analyze the following user research request, considering any existing mission goals provided, to determine its primary type, the appropriate tone for the output, the likely target audience, the requested length, the requested format, and any preferred source types.

**CRITICAL:** If the 'Existing Mission Goals' specify a particular tone, audience, length, format, output style, or source type preference (e.g., "write in 5th grader tone", "target audience: general public", "output: short summary", "format: bullet points", "use academic literature sources"), **you MUST prioritize that user preference** in your determination below, even if the raw 'User Research Request' text suggests something different (e.g., a formal academic topic).

Existing Mission Goals:
---
{goals_context}

User Research Request:
---
{user_request}
---

Instructions:
**CRITICAL PRIORITIZATION:**
- If 'Existing Mission Goals' contain specific instructions about the desired TONE (e.g., "write like a 5th grader", "use formal language"), you **MUST** select that exact `target_tone`.
- If 'Existing Mission Goals' contain specific instructions about the intended AUDIENCE (e.g., "explain for the general public", "target audience: experts"), you **MUST** select that exact `target_audience`.
- If 'Existing Mission Goals' contain specific instructions about the desired LENGTH (e.g., "brief summary", "comprehensive report"), you **MUST** select that exact `requested_length`.
- If 'Existing Mission Goals' contain specific instructions about the desired FORMAT (e.g., "bullet points", "full paper"), you **MUST** select that exact `requested_format`.
- If 'Existing Mission Goals' or 'User Research Request' contain specific instructions about PREFERRED SOURCE TYPES (e.g., "use academic literature", "prioritize legal sources", "focus on state law"), you **MUST** extract and include these in `preferred_source_types`.
- These explicit user preferences from the goals **OVERRIDE** any interpretation based solely on the 'User Research Request' text.

1.  **Classify Request Type:** Determine the most appropriate classification for the request. The value should be a concise string. Examples include:
    - "Academic Literature Review"
    - "Informal Explanation"
    - "General Web Search Summary"
    - "Technical Comparison"
    - "Creative Writing"
    - "Code Generation"
    - "Data Analysis"
    *You are not limited to these examples. Provide the most accurate classification based on the request.*
2.  **Determine Target Tone:** Determine the most appropriate tone for the final output, prioritizing any tone specified in the 'Existing Mission Goals'. The value should be a concise string. Examples include:
    - "Formal Academic"
    - "Neutral/Objective"
    - "Informal/Conversational"
    - "Technical"
    - "Creative"
    - "5th Grader" (Example of a specific user goal)
    *You are not limited to these examples. Provide the most accurate tone, especially if specified by the user.*
3.  **Identify Target Audience:** Determine the most likely intended audience, prioritizing any audience specified in the 'Existing Mission Goals'. The value should be a concise string. Examples include:
    - "Researchers/Experts"
    - "General Public"
    - "Technical Team"
    - "Students"
    - "Specific Stakeholder" (e.g., "Marketing Department")
    *You are not limited to these examples. Provide the most accurate audience, especially if specified by the user.*
4.  **Determine Requested Length:** Determine the most appropriate length for the final output, prioritizing any length specified in the 'Existing Mission Goals'. The value should be a concise string. Examples include:
    - "Short Summary"
    - "Comprehensive Report"
    - "Brief Paragraph"
    - "Extended Analysis"
    - "Concise Overview"
    *You are not limited to these examples. Provide the most accurate length, especially if specified by the user.*
5.  **Determine Requested Format:** Determine the most appropriate format for the final output, prioritizing any format specified in the 'Existing Mission Goals'. The value should be a concise string. Examples include:
    - "Full Paper"
    - "Bullet Points"
    - "Summary Paragraph"
    - "Q&A Format"
    - "Structured Report"
    - "Comparative Table"
    *You are not limited to these examples. Provide the most accurate format, especially if specified by the user.*
6.  **Identify Preferred Source Types:** Identify any specific source types the user wants to prioritize or focus on. The value should be a concise string. Examples include:
    - "Academic Literature"
    - "Legal Sources"
    - "State Law"
    - "News Articles"
    - "Government Reports"
    - "Scientific Journals"
    - "Industry Publications"
    *You are not limited to these examples. Provide the most accurate source type preferences, especially if specified by the user.*
7.  **Provide Reasoning:** Briefly explain your choices for type, tone, audience, length, format, and preferred source types, referencing specific goals if they influenced your decision.

Output ONLY a single JSON object conforming EXACTLY to the RequestAnalysisOutput schema. Ensure your choices reflect any user preferences found in the 'Existing Mission Goals'. The values for all fields should be strings, but do not have to be chosen from the examples provided above if a different string is more accurate.
```json
{{
  "request_type": "...", // A string describing the request type (can be custom)
  "target_tone": "...", // A string describing the target tone (PRIORITIZE user preference from goals, can be custom)
  "target_audience": "...", // A string describing the target audience (PRIORITIZE user preference from goals, can be custom)
  "requested_length": "...", // A string describing the requested length (PRIORITIZE user preference from goals, can be custom)
  "requested_format": "...", // A string describing the requested format (PRIORITIZE user preference from goals, can be custom)
  "preferred_source_types": "...", // A string describing preferred source types (PRIORITIZE user preference from goals/request, can be custom)
  "analysis_reasoning": "Brief justification for the choices."
}}
```
"""
        # Create a structured message that includes both the user request and goal texts
        messages = [
            {"role": "system", "content": "Consider these active goals when analyzing the request: " + json.dumps({"active_goals": goal_texts})},
            {"role": "user", "content": analysis_prompt}
        ]
        # Start with json_schema format, with fallback to json_object, then no response_format
        from ai_researcher.agentic_layer.utils.json_format_helper import (
            should_retry_with_json_object,
            should_retry_without_response_format
        )
        analysis_result: Optional[RequestAnalysisOutput] = None
        model_details = None
        _provider, _model = self.controller.model_dispatcher.get_provider_and_model_for_mode("planning")
        format_mode = get_initial_format_mode(_provider, _model)
        max_format_attempts = 3  # Try json_schema, then json_object, then no response_format
        log_status = "failure"
        error_msg = None

        for format_attempt in range(max_format_attempts):
            try:
                # Prepare response format based on current mode
                if format_mode == "json_object":
                    response_format = get_json_object_format()
                    current_messages = enhance_messages_for_json_object(messages, RequestAnalysisOutput)
                    logger.info(f"Request analysis: Using json_object format due to schema compatibility issue")
                elif format_mode == "none":
                    response_format = None  # No response format
                    current_messages = enhance_messages_for_json_object(messages, RequestAnalysisOutput)
                    logger.info(f"Request analysis: Using no response_format (prompt-only JSON mode)")
                else:
                    response_format = get_json_schema_format(
                        pydantic_model=RequestAnalysisOutput,
                        schema_name="request_analysis_output"
                    )
                    current_messages = messages
                # Use a model suitable for analysis and instruction following (planning model is good)
                async with self.controller.maybe_semaphore:
                    response, model_details = await self.controller.model_dispatcher.dispatch(
                        messages=current_messages,
                        response_format=response_format,
                        agent_mode="planning",  # Use planning model for structured output and instruction following
                        mission_id=mission_id,  # Pass mission_id for cost tracking
                        log_queue=log_queue,  # Pass log_queue for UI updates
                        update_callback=update_callback  # Pass update_callback for cost tracking
                    )

                if response and response.choices and response.choices[0].message.content:
                    raw_json = response.choices[0].message.content
                    try:
                        sanitized_json = sanitize_json_string(raw_json)
                        analysis_result = RequestAnalysisOutput.model_validate_json(sanitized_json)
                        logger.info(f"Request analysis successful for mission {mission_id}: Type={analysis_result.request_type}, Tone={analysis_result.target_tone}, Audience={analysis_result.target_audience}")
                        log_status = "success"
                        error_msg = None
                        break  # Success, exit retry loop
                    except (json.JSONDecodeError, ValueError) as e:
                        logger.error(f"Failed to parse/validate request analysis JSON for mission {mission_id}: {e}\nRaw: {raw_json}")
                        log_status = "failure"
                        error_msg = f"JSON Parse/Validation Error: {e}"
                        break  # JSON parsing error, don't retry
                else:
                    logger.error(f"Request analysis LLM call failed or returned empty content for mission {mission_id}.")
                    log_status = "failure"
                    error_msg = "LLM call failed or returned empty content."
                    break  # Empty response, don't retry

            except Exception as e:
                logger.error(f"Error during request analysis LLM call for mission {mission_id}: {e}", exc_info=True)

                # Check if we should retry with different format
                if format_mode == "json_schema" and should_retry_with_json_object(e):
                    logger.info(f"Request analysis: Detected json_schema compatibility issue, retrying with json_object format")
                    if _provider and _model:
                        mark_format_unsupported(_provider, _model, "json_schema")
                    format_mode = "json_object"
                    continue  # Retry with json_object format
                elif format_mode == "json_object" and should_retry_without_response_format(e):
                    logger.info(f"Request analysis: Detected json_object also not supported, retrying without response_format")
                    if _provider and _model:
                        mark_format_unsupported(_provider, _model, "json_object")
                    format_mode = "none"
                    continue  # Retry without response_format

                log_status = "failure"
                error_msg = f"Exception during analysis: {e}"
                break  # Non-recoverable error, exit loop

        # Log the analysis step
        await self.controller.context_manager.log_execution_step(
            mission_id=mission_id,
            agent_name="AgentController",
            action="Analyze Request Type",
            input_summary=f"User Request: {user_request[:60]}..., Goals Provided: {len(active_goals) if active_goals else 0}",
            output_summary=(f"Type: {analysis_result.request_type}, Tone: {analysis_result.target_tone}, Audience: {analysis_result.target_audience}" if analysis_result else "Analysis failed.") if log_status == "success" else error_msg,
            status=log_status,
            error_message=error_msg,
            full_input={"user_request": user_request, "active_goals": goal_texts},
            full_output=analysis_result.model_dump() if analysis_result else None,
            model_details=model_details,
            log_queue=log_queue,
            update_callback=update_callback
        )

        # Update stats (Now handled by ContextManager)
        if model_details:
            await self.controller.context_manager.update_mission_stats(mission_id, model_details, log_queue, update_callback)

        return analysis_result

    @staticmethod
    def _resolve_awaiting_clarification(metadata: dict, user_message: str) -> Optional[dict]:
        """Decide whether a user follow-up resolves an open case-assumption conflict.

        Pure/testable (no controller access). Returns the metadata update to
        persist on resolution, or None when there is no pending conflict or the
        follow-up did not resolve it.

        The follow-up is parsed as an AUTHORITATIVE override (review finding 1):
        every assumption field it explicitly mentions REPLACES the original's
        values for that field (no text concatenation — concatenation could
        never resolve a conflict because both figures would remain). On
        resolution we clear BOTH ``awaiting_clarification`` AND
        ``case_assumption_conflicts`` and persist the resolved assumptions +
        the correction overlay so the planner sees the corrected figures.
        """
        pending = metadata.get("awaiting_clarification")
        if not pending:
            return None
        full_briefing = metadata.get("full_briefing") or ""
        from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
            resolve_case_assumptions as _resolve,
            apply_corrections_to_briefing as _apply_corr,
        )
        merged, still_conflicting = _resolve(full_briefing, user_message)
        if still_conflicting:
            return None
        resolved = {
            "turnovers": [{"value": v, "raw": r} for v, r in merged.turnovers],
            "per_employees": [{"value": v, "raw": r} for v, r in merged.per_employees],
            "headcounts": [{"value": v, "raw": r} for v, r in merged.headcounts],
        }
        # Review finding 2: persist a CANONICAL corrected briefing — the stale
        # figures replaced inline by the corrected values — so the planner reads
        # consistent figures in the verbatim text itself, not just in an overlay.
        corrected_briefing = _apply_corr(full_briefing, merged)
        update = {
            "awaiting_clarification": None,
            "case_assumption_conflicts": [],
            "case_assumption_corrections": {
                "resolved_assumptions": resolved,
                "correction_text": user_message,
            },
        }
        # Only store the corrected briefing when it actually differs from the
        # original (a correction that changed nothing leaves the text intact).
        if corrected_briefing != full_briefing:
            update["full_briefing_corrected"] = corrected_briefing
        return update

    async def handle_user_message(
        self,
        user_message: str,
        chat_history: List[Tuple[str, str]],
        chat_id: str,
        mission_id: Optional[str] = None,
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, Any], None]] = None,
        use_web_search: Optional[bool] = True,
        document_group_id: Optional[str] = None,
        auto_create_document_group: Optional[bool] = False,
        citation_profile_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Handles a user message using the MessengerAgent.
        Returns a dictionary containing the agent's response and potential actions.
        """
        logger.info(f"Handling user message via MessengerAgent: '{user_message[:50]}...'")

        # Look up the chat's mode so the MessengerAgent can gate its intent set.
        # chat_type='chat' disables start_research / refine_questions entirely
        # (see docs/plans/CHAT_MODES_AND_STRUCTURED_BRIEFING.md).
        chat_mode = "research"
        if chat_id:
            try:
                from database.database import get_db
                from database import models as _models
                _db = next(get_db())
                try:
                    _chat_row = (
                        _db.query(_models.Chat.chat_type)
                        .filter(_models.Chat.id == chat_id)
                        .first()
                    )
                    if _chat_row and _chat_row.chat_type in ("chat", "research"):
                        chat_mode = _chat_row.chat_type
                finally:
                    _db.close()
            except Exception as _e:
                logger.debug(f"chat_mode lookup failed for chat_id={chat_id}: {_e}")

        # Pre-detect structured briefing on the raw user message so we can
        # preserve the full body + Leitfragen when a mission is later created.
        from ai_researcher.agentic_layer.controller.utils.briefing_detector import (
            detect_structured_briefing,
            extract_leitfragen,
            classify_assignment,
        )
        is_structured_briefing = (
            chat_mode == "research"
            and detect_structured_briefing(user_message)
        )
        briefing_leitfragen: List[str] = (
            extract_leitfragen(user_message) if is_structured_briefing else []
        )
        # P2: full assignment classification. `classification` drives the
        # complete-briefing direct-start path further below. Computed once here
        # so both the structured-passthrough and the start_research branch see
        # the same signals.
        classification = (
            classify_assignment(user_message)
            if chat_mode == "research"
            else {"specificity": "open", "briefing_style": "open",
                  "primary_question": None, "questions": []}
        )
        if is_structured_briefing:
            logger.info(
                "Detected structured briefing in user message (chat_id=%s): "
                "specificity=%s, %d Leitfragen extracted",
                chat_id,
                classification.get("specificity"),
                len(briefing_leitfragen),
            )

        # Fetch relevant context if needed (e.g., current mission status, plan summary)
        mission_context_summary = None
        if mission_id:
            context = self.controller.context_manager.get_mission_context(mission_id)
            if context:
                mission_context_summary = f"Current Mission: Status={context.status}"
                if context.plan:
                    mission_context_summary += f", Goal='{context.plan.mission_goal[:50]}...'"

                # CRITICAL: Update mission settings with current UI settings BEFORE processing
                # This ensures "okay start" uses the latest settings from the UI
                if context.status in ['planning', 'paused', 'pending']:
                    logger.info(f"Updating mission {mission_id} with current UI settings before processing message")

                    # Get existing metadata
                    existing_metadata = context.metadata or {}

                    # Build tool selection
                    tool_selection = {
                        "web_search": use_web_search,
                        "local_rag": document_group_id is not None
                    }

                    # Get user settings for comprehensive_settings
                    from ai_researcher.user_context import get_current_user
                    from datetime import datetime
                    current_user = get_current_user()
                    user_settings = current_user.settings if current_user and hasattr(current_user, 'settings') else {}

                    # Update comprehensive_settings with current UI settings
                    comprehensive_settings = existing_metadata.get("comprehensive_settings", {})
                    comprehensive_settings.update({
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,
                        "document_group_id": document_group_id,
                        "settings_updated_from_chat": True,
                        "chat_update_time": datetime.now().isoformat()
                    })

                    # Update metadata with current settings
                    existing_metadata.update({
                        "tool_selection": tool_selection,
                        "document_group_id": document_group_id,
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,
                        "comprehensive_settings": comprehensive_settings
                    })

                    # Save the updated metadata
                    await self.controller.context_manager.update_mission_metadata(mission_id, existing_metadata)
                    logger.info(f"Updated mission {mission_id} settings - Web Search: {use_web_search}, Doc Group: {document_group_id}, Auto-save: {auto_create_document_group}")

        # Fetch current scratchpad
        current_scratchpad = self.controller.context_manager.get_scratchpad(mission_id) if mission_id else None
        # Fetch Active Goals & Thoughts
        active_goals = self.controller.context_manager.get_active_goals(mission_id) if mission_id else None
        active_thoughts = self.controller.context_manager.get_recent_thoughts(mission_id, limit=THOUGHT_PAD_CONTEXT_LIMIT) if mission_id else None

        # Resolve awaiting_clarification state (review finding 1+2): when the
        # user sends a follow-up to a mission blocked on case-assumption
        # conflicts, re-check whether the conflicts are resolved. The decision
        # logic lives in the pure/testable helper _resolve_awaiting_clarification()
        # (returns the metadata update or None); we only persist the result here.
        if mission_id:
            try:
                _mc = self.controller.context_manager.get_mission_context(mission_id)
                _md = (getattr(_mc, "metadata", None) or {}) if _mc else {}
                _update = self._resolve_awaiting_clarification(_md, user_message or "")
                if _update:
                    await self.controller.context_manager.update_mission_metadata(mission_id, _update)
                    logger.info(
                        "Mission %s: case-assumption conflicts RESOLVED via "
                        "follow-up (structured override). Cleared "
                        "awaiting_clarification + case_assumption_conflicts; "
                        "persisted resolved assumptions + correction overlay.",
                        mission_id,
                    )
                elif _md.get("awaiting_clarification"):
                    logger.info(
                        "Mission %s: follow-up did not resolve all conflicts; "
                        "staying blocked.",
                        mission_id,
                    )
            except Exception as _clar_err:
                logger.warning("awaiting_clarification re-check failed: %s", _clar_err)

        try:
            # Don't apply semaphore for chat operations to allow concurrent chats
            # The semaphore should only be used for heavy research operations
            # that might overwhelm the LLM API with many parallel requests
            
            # Call the MessengerAgent's run method directly without semaphore
            agent_output, model_details, scratchpad_update = await self.controller.messenger_agent.run(
                user_message=user_message,
                chat_history=chat_history,
                mission_context_summary=mission_context_summary,
                active_goals=active_goals,
                active_thoughts=active_thoughts,
                agent_scratchpad=current_scratchpad,
                mission_id=mission_id,
                log_queue=log_queue,
                update_callback=update_callback,
                chat_mode=chat_mode,
            )

            # Update scratchpad if the agent provided an update and mission_id exists
            if scratchpad_update and mission_id:
                await self.controller.context_manager.update_scratchpad(mission_id, scratchpad_update)
                logger.info(f"Updated scratchpad after MessengerAgent interaction for mission {mission_id}.")

            if not agent_output:
                raise ValueError("MessengerAgent returned None")

            # Log the interaction with proper None handling
            agent_response = agent_output.get('response', '') or ''  # Ensure we have a string
            agent_action = agent_output.get('action')
            await self.controller.context_manager.log_execution_step(
                mission_id=mission_id or "N/A",  # Use N/A if no mission context
                agent_name=self.controller.messenger_agent.agent_name,
                action="Handle User Message",
                input_summary=f"User: {user_message[:60]}...",
                output_summary=f"Agent: {agent_response[:60]}... Action: {agent_action}",
                status="success",
                full_input={"user_message": user_message, "history_len": len(chat_history), "context_summary": mission_context_summary},
                full_output=agent_output,
                model_details=model_details,
            )

            # Intercept and Handle Actions
            action = agent_output.get("action")
            request_content = agent_output.get("request")  # This holds the feedback/goal text
            original_user_message_for_goal = user_message  # Capture the original message
            formatting_preferences = agent_output.get("formatting_preferences")  # Extract formatting preferences if present

                # Handle start_research action - Use existing mission or create new one
            if action == "start_research" and request_content:
                logger.info(f"Handling 'start_research' action. Request: '{request_content[:60]}...'")
                
                # Prepare tool selection based on user settings
                use_local_rag = document_group_id is not None
                tool_selection = {
                    "web_search": use_web_search,
                    "local_rag": use_local_rag
                }
                logger.info(f"Tool selection for research: {tool_selection}, document_group_id: {document_group_id}")
                
                # Use existing mission if provided, otherwise create a new one
                if mission_id:
                    logger.info(f"Using existing mission ID: {mission_id}")
                    mission_context = self.controller.context_manager.get_mission_context(mission_id)
                    if not mission_context:
                        logger.warning(f"Mission {mission_id} not found, creating new mission")
                        detected_lang = _detect_language(self.controller, llm_response=user_message)
                        mission_context = await self.controller.context_manager.start_mission(user_request=request_content, chat_id=chat_id, language_code=detected_lang)
                        mission_id = mission_context.mission_id
                        logger.info(f"Created new mission with ID: {mission_id} (detected language: {detected_lang})")
                    
                    # Get user settings to build comprehensive_settings
                    from ai_researcher.user_context import get_current_user
                    from datetime import datetime  # Ensure datetime is available in this scope
                    current_user = get_current_user()
                    user_settings = current_user.settings if current_user and hasattr(current_user, 'settings') else {}
                    
                    # Build comprehensive_settings like in the missions endpoint
                    research_params = user_settings.get("research_parameters", {})
                    research_params["auto_create_document_group"] = auto_create_document_group
                    
                    ai_settings = user_settings.get("ai_endpoints", {})
                    model_config = {
                        "fast_provider": ai_settings.get("fast_llm_provider"),
                        "fast_model": ai_settings.get("fast_llm_model"),
                        "mid_provider": ai_settings.get("mid_llm_provider"),
                        "mid_model": ai_settings.get("mid_llm_model"),
                        "intelligent_provider": ai_settings.get("intelligent_llm_provider"),
                        "intelligent_model": ai_settings.get("intelligent_llm_model"),
                        "verifier_provider": ai_settings.get("verifier_llm_provider"),
                        "verifier_model": ai_settings.get("verifier_llm_model"),
                    }
                    
                    search_settings = user_settings.get("search", {})
                    web_fetch_settings = user_settings.get("web_fetch", {})
                    
                    comprehensive_settings = {
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,
                        "document_group_id": document_group_id,
                        "document_group_name": None,  # Will be populated if/when group is created
                        "model_config": model_config,
                        "research_params": research_params,
                        "search_provider": search_settings.get("provider"),
                        "web_fetch_settings": web_fetch_settings,
                        "all_user_settings": user_settings,
                        "settings_captured_at": datetime.now().isoformat(),
                        "start_method": "chat_interface_existing"
                    }
                    
                    # Update mission metadata with comprehensive settings
                    existing_metadata = mission_context.metadata or {}
                    metadata_update = {
                        "tool_selection": tool_selection,
                        "document_group_id": document_group_id,
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,  # CRITICAL: Store at top level for frontend
                        "research_params": {
                            "auto_create_document_group": auto_create_document_group
                        },
                        "comprehensive_settings": comprehensive_settings
                    }
                    if citation_profile_id:
                        metadata_update["citation_profile_id"] = citation_profile_id
                        # Also store in mission_settings for the settings dialog
                        if "mission_settings" not in existing_metadata:
                            existing_metadata["mission_settings"] = {}
                        existing_metadata["mission_settings"]["citation_profile_id"] = citation_profile_id
                    existing_metadata.update(metadata_update)
                    await self.controller.context_manager.update_mission_metadata(mission_id, existing_metadata)
                else:
                    # Create mission if no existing mission_id
                    detected_lang = _detect_language(self.controller, llm_response=user_message)
                    mission_context = await self.controller.context_manager.start_mission(user_request=request_content, chat_id=chat_id, language_code=detected_lang)
                    mission_id = mission_context.mission_id
                    logger.info(f"Created new mission with ID: {mission_id} (detected language: {detected_lang})")
                    
                    # Get user settings to build comprehensive_settings
                    from ai_researcher.user_context import get_current_user
                    from datetime import datetime  # Ensure datetime is available in this scope
                    current_user = get_current_user()
                    user_settings = current_user.settings if current_user and hasattr(current_user, 'settings') else {}
                    
                    # Build comprehensive_settings like in the missions endpoint
                    research_params = user_settings.get("research_parameters", {})
                    research_params["auto_create_document_group"] = auto_create_document_group
                    
                    ai_settings = user_settings.get("ai_endpoints", {})
                    model_config = {
                        "fast_provider": ai_settings.get("fast_llm_provider"),
                        "fast_model": ai_settings.get("fast_llm_model"),
                        "mid_provider": ai_settings.get("mid_llm_provider"),
                        "mid_model": ai_settings.get("mid_llm_model"),
                        "intelligent_provider": ai_settings.get("intelligent_llm_provider"),
                        "intelligent_model": ai_settings.get("intelligent_llm_model"),
                        "verifier_provider": ai_settings.get("verifier_llm_provider"),
                        "verifier_model": ai_settings.get("verifier_llm_model"),
                    }
                    
                    search_settings = user_settings.get("search", {})
                    web_fetch_settings = user_settings.get("web_fetch", {})
                    
                    comprehensive_settings = {
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,
                        "document_group_id": document_group_id,
                        "document_group_name": None,  # Will be populated if/when group is created
                        "model_config": model_config,
                        "research_params": research_params,
                        "search_provider": search_settings.get("provider"),
                        "web_fetch_settings": web_fetch_settings,
                        "all_user_settings": user_settings,
                        "settings_captured_at": datetime.now().isoformat(),
                        "start_method": "chat_interface"
                    }
                    
                    # Set initial metadata with comprehensive settings
                    new_mission_metadata = {
                        "tool_selection": tool_selection,
                        "document_group_id": document_group_id,
                        "use_web_search": use_web_search,
                        "use_local_rag": document_group_id is not None,
                        "auto_create_document_group": auto_create_document_group,  # CRITICAL: Store at top level for frontend
                        "research_params": {
                            "auto_create_document_group": auto_create_document_group
                        },
                        "comprehensive_settings": comprehensive_settings
                    }
                    if citation_profile_id:
                        new_mission_metadata["citation_profile_id"] = citation_profile_id
                        if "mission_settings" not in new_mission_metadata:
                            new_mission_metadata["mission_settings"] = {}
                        new_mission_metadata["mission_settings"]["citation_profile_id"] = citation_profile_id
                    await self.controller.context_manager.update_mission_metadata(mission_id, new_mission_metadata)
                
                # Now check if there were formatting preferences in the agent output
                if formatting_preferences:
                    goal_id = await self.controller.context_manager.add_goal(
                        mission_id=mission_id,
                        text=formatting_preferences,
                        source_agent=self.controller.messenger_agent.agent_name
                    )
                    logger.info(f"Added formatting preferences as goal '{goal_id}': '{formatting_preferences}'")

                # --- Structured-briefing handling (P1 + P2) ---
                # `classification` was computed at the top of handle_user_message.
                #   specificity == 'complete'  -> direct start (P2)
                #   specificity == 'structured' -> honour briefing in Planning (P1),
                #                                 but keep the question/approval loop
                #   specificity == 'open'       -> current behaviour (generate questions)
                # See docs/plans/STRUCTURED_BRIEFING_DIRECT_START.md
                briefing_style = classification.get("briefing_style", "open")
                is_complete = classification.get("specificity") == "complete"
                primary_q = classification.get("primary_question")
                sub_qs: List[str] = classification.get("questions") or briefing_leitfragen or []

                questions = None
                model_details = None

                if briefing_style == "structured":
                    # P1: persist the full briefing + primary Leitfrage + structured
                    # outline so downstream agents (Planning) honour it instead of
                    # the distilled one-liner.
                    # NOTE (review finding 2): we deliberately do NOT stage
                    # initial_questions here. Conflicts are checked below and, when
                    # present, must block the mission WITHOUT leaving questions
                    # persisted (otherwise the normal approve/start flow can still
                    # fire and the hard stop is cosmetic). initial_questions is
                    # persisted in a SEPARATE update AFTER the conflict check.
                    metadata_update = {
                        "briefing_style": "structured",
                        "full_briefing": user_message,
                        "primary_leitfrage": primary_q,
                    }
                    # Structured Gliederung (Finding 1): store the parsed outline as
                    # (number, title, level) records with number-free titles, so the
                    # planner can reproduce the user's hierarchy deterministically
                    # instead of being tempted to invent its own / duplicate numbers.
                    outline_records = classification.get("outline") or []
                    if outline_records:
                        metadata_update["structured_outline"] = outline_records
                    # Deterministic word budget (Priority 1 of the metadata plan):
                    # store total + per-section budgets so the planner and writer
                    # can enforce them instead of blowing past 'ca. 3.000 Wörter'.
                    word_budget = classification.get("word_budget") or {}
                    if word_budget.get("total_word_budget") or word_budget.get("section_word_budgets"):
                        metadata_update["word_budget"] = word_budget
                    # Staged-output directive (Priority 5): if the briefing asks
                    # for a planning-only first deliverable ("Gib zunächst noch
                    # keinen vollständigen Fließtext aus"), flag it so the
                    # planner produces the staged deliverable (outline/theses/
                    # source matrix) and the mission does NOT jump straight to a
                    # full draft. Specificity is already 'structured' (not
                    # 'complete') in that case, so the approval loop stays.
                    output_stage = classification.get("output_stage")
                    if output_stage == "planning_only":
                        metadata_update["output_stage"] = "planning_only"
                        metadata_update["staged_first_output"] = (
                            "Deliver ONLY the planning artefacts first: Hauptthese, "
                            "Unterthesen, kommentierte Gliederung mit Wortbudget, "
                            "Quellenmatrix, Liste der benötigten Praxisquellen, "
            "vorläufige Auswahl der zentralen Faktoren, offene Quellenlücken. "
                            "Do NOT write the full Fließtext/Hausarbeit yet — await "
                            "user confirmation."
                        )
                    # Plausibility conflicts (Priority 6): store them and add a
                    # goal so the agent surfaces them to the user rather than
                    # silently picking one figure.
                    case_conflicts = classification.get("case_assumption_conflicts") or []
                    if case_conflicts:
                        metadata_update["case_assumption_conflicts"] = case_conflicts
                    await self.controller.context_manager.update_mission_metadata(mission_id, metadata_update)

                    # Surface conflicts as a goal so the agent flags them in its
                    # reply (the user must resolve e.g. 19 Mio. vs 40 Mio. Euro
                    # before the mission can produce a consistent Hausarbeit).
                    if case_conflicts:
                        conflict_text = "Widersprüchliche Fallannahmen erkannt (bitte vom Nutzer auflösen lassen): "
                        conflict_text += " | ".join(case_conflicts)
                        await self.controller.context_manager.add_goal(
                            mission_id=mission_id,
                            text=conflict_text,
                            source_agent="BriefingDetector",
                        )
                        # HARD STOP (review finding 2): conflicts must block the
                        # mission from proceeding to question display / planning.
                        # Persist an awaiting_clarification flag so /missions/{id}/start
                        # and prepare_mission_start() can reject the start, and
                        # return a clarification response now — no initial_questions
                        # are staged, so the normal approve/start flow cannot fire.
                        await self.controller.context_manager.update_mission_metadata(
                            mission_id, {"awaiting_clarification": case_conflicts}
                        )
                        lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                        bullet_conflicts = "\n".join(f"- {c}" for c in case_conflicts)
                        clarify = (
                            "⚠️ Bevor ich starte, müssen wir einige widersprüchliche "
                            "Fallannahmen klären:\n\n"
                            f"{bullet_conflicts}\n\n"
                            "Bitte korrigiere oder bestätige die relevanten Zahlen, "
                            "damit die Hausarbeit auf einer konsistenten Grundlage beruht. "
                            "(z. B. „Jahresumsatz ist 40 Mio. Euro, die 470.000 € pro "
                            "Mitarbeitendem sind die bewusste Fallannahme.“)"
                        )
                        agent_output["response"] = clarify
                        agent_output["mission_id"] = mission_id
                        questions = []  # explicit: do NOT stage questions
                        logger.warning(
                            "Mission %s blocked: %d case-assumption conflict(s) — "
                            "awaiting user clarification before start.",
                            mission_id, len(case_conflicts),
                        )
                        return agent_output

                    # No conflicts (review finding 2): NOW it is safe to stage the
                    # user's Leitfragen as initial_questions. This happens AFTER the
                    # conflict hard-stop above, so a blocked mission never has
                    # questions persisted and the normal approve/start flow cannot
                    # fire while awaiting_clarification is set.
                    if sub_qs:
                        await self.controller.context_manager.update_mission_metadata(
                            mission_id, {"initial_questions": sub_qs}
                        )

                    # NOTE: we deliberately do NOT overwrite mission_context.user_request
                    # with the full briefing (Finding 3). user_request is the short
                    # mission label/name and is used by prepare_mission_start() to name
                    # the auto-save document group ("R: <title>"). Overwriting it with
                    # the whole briefing produced group names like
                    # "R: Du unterstützt mich bei der Konzeption und sp...".
                    # The full briefing is already available to agents via the
                    # ``full_briefing`` metadata field consumed by the planning agent.

                    await self.controller.context_manager.add_goal(
                        mission_id=mission_id,
                        text="briefing_style=structured — use the user's Leitfragen and outline verbatim; do not invent alternative questions",
                        source_agent="BriefingDetector",
                    )
                    logger.info(
                        "Structured briefing (specificity=%s) for mission %s; primary_leitfrage=%s, %d sub-questions, %d outline sections",
                        classification.get("specificity"), mission_id,
                        bool(primary_q), len(sub_qs), len(outline_records),
                    )

                    # COMPLETE assignment -> behave EXACTLY like an open research
                    # mission, but WITHOUT the generic question-generation step:
                    #   * take the user's own Leitfragen/sub-questions as the displayed
                    #     questions (stored as initial_questions),
                    #   * reply "I've understood the assignment",
                    #   * leave the mission in the normal pre-start state so the user
                    #     can edit settings, click Start, or send a chat "start"
                    #     message — identical lifecycle to an open mission.
                    # This replaces the earlier auto-start hack, which bypassed the
                    # Settings menu and the canonical /missions/{id}/start prep
                    # (prepare_mission_start + apply_auto_optimization), causing both
                    # missing R: document groups and far less agentic activity.
                    if is_complete:
                        final_qs = ([primary_q] if primary_q else []) + sub_qs
                        if not final_qs:
                            # Degenerate: classified complete but nothing to use as
                            # questions. Fall back to generating.
                            logger.warning(
                                "Complete briefing for mission %s had no usable Leitfragen; "
                                "falling back to question generation.", mission_id
                            )
                        else:
                            # Use the user's Leitfragen as the questions to display.
                            # Persist them so /missions/{id}/start and the chat
                            # "approve_questions"/"start" flow can pick them up.
                            await self.controller.context_manager.update_mission_metadata(mission_id, {
                                "initial_questions": final_qs,
                            })
                            # Acknowledge the assignment (do NOT auto-start). Tell the
                            # user how to launch, mirroring the open-mission prompt.
                            lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                            ack_lines = [_get_ui_string("complete_briefing_ack", lang)]
                            if primary_q:
                                ack_lines.append(f"\n**Leitfrage:** {primary_q}")
                            if sub_qs:
                                ack_lines.append(f"**Unterfragen:** {len(sub_qs)}")
                            ack_lines.append(_get_ui_string("questions_prompt", lang))
                            agent_output["response"] = "\n".join(ack_lines)
                            agent_output["questions"] = final_qs
                            agent_output["mission_id"] = mission_id
                            questions = final_qs  # skip the ResearchAgent generation below
                            logger.info(
                                "Complete briefing for mission %s staged as questions (no auto-start); "
                                "user will start via settings menu / Start button / chat.",
                                mission_id,
                            )
                            return agent_output

                    # structured-but-not-complete: use the user's sub-questions for display.
                    if sub_qs:
                        questions = sub_qs

                # Generate initial questions using the ResearchAgent for higher quality
                # (only reached for open research, structured-without-subquestions,
                # (only reached for open research or structured-without-subquestions;
                # a complete briefing returns earlier with its own Leitfragen).
                try:
                    if questions is None:
                        logger.info(f"Generating initial questions for mission {mission_id} using ResearchAgent")

                        # Get active goals for the mission to provide context
                        active_goals = self.controller.context_manager.get_active_goals(mission_id)

                        # Use the ResearchAgent to generate high-quality initial questions
                        questions, model_details = await self.controller.research_agent.generate_initial_questions(
                            mission_id=mission_id,
                            user_request=request_content,
                            active_goals=active_goals,
                            log_queue=log_queue,
                            update_callback=update_callback
                        )

                    if questions:
                        # Store the questions in mission metadata
                        await self.controller.context_manager.update_mission_metadata(
                            mission_id,
                            {"initial_questions": questions}
                        )

                        # Update mission stats
                        if model_details:
                            await self.controller.context_manager.update_mission_stats(mission_id, model_details, log_queue, update_callback)

                        # Replace the MessengerAgent's response entirely with a clean
                        # message + the ResearchAgent's questions only. The MessengerAgent
                        # often includes its own question suggestions that duplicate/conflict.
                        lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                        questions_text = "\n".join([f"{i+1}. {q}" for i, q in enumerate(questions)])
                        agent_output["response"] = f"{_get_ui_string('questions_intro', lang)}\n\n{questions_text}\n\n{_get_ui_string('questions_prompt', lang)}"
                        agent_output["questions"] = questions
                        
                        logger.info(f"Generated {len(questions)} initial questions for mission {mission_id} via ResearchAgent and included them in chat response")
                    
                    else:
                        logger.warning(f"ResearchAgent failed to generate questions for mission {mission_id}, proceeding without questions")
                        lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                        agent_output["response"] = f"{agent_output['response']}\n\n{_get_ui_string('ready_to_research', lang)}"
                
                except Exception as e:
                    logger.error(f"Error generating questions for mission {mission_id} with ResearchAgent: {e}", exc_info=True)
                    # Continue without questions rather than failing the whole flow
                    lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                    agent_output["response"] = f"{agent_output['response']}\n\n{_get_ui_string('ready_to_research', lang)}"
                
                # Return the agent output with the mission_id added
                agent_output["mission_id"] = mission_id
                return agent_output
            
            # Handle refine_questions action - Refine the research questions based on user feedback
            elif action == "refine_questions":
                # If mission_id is not provided, try to extract it from the agent output or use the passed mission_id
                target_mission_id = mission_id or agent_output.get("mission_id")
                
                if not target_mission_id:
                    logger.warning(f"No mission ID available for refine_questions action")
                    agent_output["response"] = "I need an active research mission to refine questions. Please start a new research request first."
                    return agent_output
                
                logger.info(f"Handling 'refine_questions' action for mission {target_mission_id}")
                
                # Get the stored questions from mission metadata
                mission_context = self.controller.context_manager.get_mission_context(target_mission_id)
                current_questions = mission_context.metadata.get("initial_questions", []) if mission_context else []
                
                if not current_questions:
                    logger.warning(f"No questions found for mission {target_mission_id}, cannot refine")
                    agent_output["response"] = "I don't have any questions to refine. Please start a new research request."
                    return agent_output
                
                # Use the request_content as user feedback for refinement
                user_feedback = request_content or user_message
                
                try:
                    # Call the refine_questions method
                    refined_questions, response_message = await self.refine_questions(
                        mission_id=target_mission_id,
                        user_feedback=user_feedback,
                        current_questions=current_questions,
                        log_queue=log_queue,
                        update_callback=update_callback
                    )
                    
                    # Update the agent response with the refined questions
                    agent_output["response"] = response_message
                    agent_output["questions"] = refined_questions  # Add questions to the response for frontend use
                    agent_output["mission_id"] = target_mission_id  # Ensure mission ID is returned
                    
                    logger.info(f"Successfully refined questions for mission {target_mission_id}")
                    return agent_output
                    
                except Exception as e:
                    logger.error(f"Error refining questions for mission {target_mission_id}: {e}", exc_info=True)
                    lang = _detect_language(self.controller, target_mission_id, agent_output.get("response", ""))
                    agent_output["response"] = _get_ui_string('refine_error', lang)
                    agent_output["mission_id"] = target_mission_id
                    return agent_output
            
            # Handle approve_questions action - Start the research process
            elif action == "approve_questions" and mission_id:
                logger.info(f"Handling 'approve_questions' action for mission {mission_id}")
                
                # Get the stored questions from mission metadata
                mission_context = self.controller.context_manager.get_mission_context(mission_id)
                
                # Use refined_questions if available, otherwise fall back to initial_questions
                final_questions = mission_context.metadata.get("refined_questions") or mission_context.metadata.get("initial_questions", [])
                
                if not final_questions:
                    logger.warning(f"No questions found for mission {mission_id}, cannot start research")
                    agent_output["response"] = "I don't have any questions to work with. Please start a new research request."
                    return agent_output
                
                # Preserve existing tool selection from mission metadata, or use conservative defaults
                existing_tool_selection = mission_context.metadata.get("tool_selection")
                if existing_tool_selection:
                    tool_selection = existing_tool_selection
                    logger.info(f"Using existing tool selection for mission {mission_id}: {tool_selection}")
                else:
                    # Conservative defaults if no tool selection is stored
                    tool_selection = {"local_rag": True, "web_search": False}
                    logger.warning(f"No existing tool selection found for mission {mission_id}, using conservative defaults: {tool_selection}")
                
                # Store final questions and tool selection
                await self.controller.context_manager.update_mission_metadata(mission_id, {
                    "final_questions": final_questions,
                    "tool_selection": tool_selection
                })
                
                # Update mission status to indicate research is starting
                await self.controller.context_manager.update_mission_status(mission_id, "planning")

                # Use the shared mission service to prepare the mission
                from services.mission_service import prepare_mission_start
                from ai_researcher.user_context import get_current_user
                from database.database import SessionLocal
                from database import models
                import json

                # Get current user's research parameters
                current_research_params = {}
                current_user = get_current_user()
                if current_user:
                    try:
                        with SessionLocal() as db:
                            db_user = db.query(models.User).filter(models.User.id == current_user.id).first()
                            if db_user and db_user.settings:
                                settings_dict = json.loads(db_user.settings) if isinstance(db_user.settings, str) else db_user.settings
                                research_settings = settings_dict.get("research_parameters", {})

                                # Extract research parameters
                                current_research_params = {k: v for k, v in research_settings.items() if v is not None}
                                logger.info(f"Retrieved {len(current_research_params)} research parameters for mission {mission_id}")
                    except Exception as e:
                        logger.warning(f"Failed to get user research parameters: {e}")

                # Prepare settings for the shared function
                mission_settings = {
                    "use_web_search": use_web_search,
                    "document_group_id": document_group_id,
                    "auto_create_document_group": auto_create_document_group,
                    "current_research_params": current_research_params
                }

                # Call the shared preparation function
                updated_settings = await prepare_mission_start(
                    mission_id=mission_id,
                    mission_context=mission_context,
                    context_mgr=self.controller.context_manager,
                    settings=mission_settings,
                    log_to_frontend=True
                )

                # Update local variables with any changes from the preparation
                if updated_settings.get("document_group_id"):
                    document_group_id = updated_settings["document_group_id"]
                    tool_selection = updated_settings["tool_selection"]

                # Note: Settings capture is now handled by the shared prepare_mission_start function
                
                # Apply auto-optimization logic with comprehensive logging
                try:
                    # Get current user from context
                    from ai_researcher.user_context import get_current_user
                    current_user = get_current_user()
                    
                    if current_user:
                        # Get chat history for auto-optimization
                        from database.database import get_async_db
                        from database import async_crud
                        
                        async with get_async_db() as db:
                            chat_history = await async_crud.get_chat_messages(db, chat_id=chat_id, user_id=current_user.id)
                        
                        # Apply auto-optimization using the shared function
                        from ai_researcher.settings_optimizer import apply_auto_optimization
                        await apply_auto_optimization(
                            mission_id=mission_id,
                            current_user=current_user,
                            context_mgr=self.controller.context_manager,
                            controller=self.controller,
                            chat_history=chat_history,
                            log_queue=log_queue,
                            update_callback=update_callback
                        )
                    else:
                        logger.warning(f"No current user found for auto-optimization in mission {mission_id}")
                        
                except Exception as optimization_error:
                    logger.error(f"Auto-optimization failed for mission {mission_id}: {optimization_error}", exc_info=True)
                    # Continue with research even if optimization fails
                
                # Log the approval step
                await self.controller.context_manager.log_execution_step(
                    mission_id=mission_id,
                    agent_name="AgentController",
                    action="Approve Questions and Start Research",
                    input_summary=f"Approved {len(final_questions)} questions",
                    output_summary="Research process initiated",
                    status="success",
                    full_input={"final_questions": final_questions, "tool_selection": tool_selection},
                    full_output={"mission_status": "planning"},
                    log_queue=log_queue,
                    update_callback=update_callback
                )
                
                # Start the actual research execution in the background
                # This is the missing piece - we need to trigger run_mission()
                async def run_research_background():
                    try:
                        # Set user context for background task to ensure proper model selection
                        from ai_researcher.user_context import set_current_user, get_current_user
                        background_user = get_current_user()
                        set_current_user(background_user)
                        
                        logger.info(f"Starting background research execution for mission {mission_id} with user context: {background_user.id if background_user else 'None'}")
                        await self.controller.run_mission(
                            mission_id=mission_id,
                            log_queue=log_queue,
                            update_callback=update_callback
                        )
                        logger.info(f"Background research execution completed for mission {mission_id}")
                    except Exception as research_error:
                        logger.error(f"Background research execution failed for mission {mission_id}: {research_error}", exc_info=True)
                        # Update mission status to failed if research execution fails
                        await self.controller.context_manager.update_mission_status(
                            mission_id, 
                            "failed", 
                            f"Research execution failed: {str(research_error)}"
                        )
                
                # Import asyncio and contextvars to create the background task with context
                import asyncio
                import contextvars
                
                # Copy the current context to preserve user settings in background task
                ctx = contextvars.copy_context()
                
                # Wrapper to run the background task with preserved context
                def run_with_context():
                    task = asyncio.create_task(run_research_background(), name=f"research_mission_{mission_id}")
                    # Register the task with the controller for proper cancellation
                    self.controller.register_mission_task(mission_id, task)
                    
                    # Add a done callback to clean up when the task completes
                    def cleanup_task(future):
                        try:
                            self.controller.unregister_mission_task(mission_id)
                            if future.cancelled():
                                logger.info(f"Mission {mission_id} task was cancelled")
                            elif future.exception():
                                logger.error(f"Mission {mission_id} task failed with exception: {future.exception()}")
                            else:
                                logger.info(f"Mission {mission_id} task completed successfully")
                        except Exception as e:
                            logger.error(f"Error in task cleanup for mission {mission_id}: {e}")
                    
                    task.add_done_callback(cleanup_task)
                    return task
                
                # Start the research execution as a background task with preserved context
                ctx.run(run_with_context)
                
                # Update the response to confirm research is starting
                lang = _detect_language(self.controller, mission_id, agent_output.get("response"))
                agent_output["response"] = f"{agent_output['response']}\n\n{_get_ui_string('research_starting', lang)}"
                agent_output["mission_id"] = mission_id  # Ensure mission ID is returned
                
                logger.info(f"Research approved and background execution started for mission {mission_id}")
                return agent_output
            
            # Handle refine_goal even if content extraction failed
            elif action == "refine_goal" and mission_id:
                goal_text_to_add = request_content  # Prioritize extracted content
                if not goal_text_to_add:
                    logger.warning(f"MessengerAgent detected 'refine_goal' but failed to extract content. Falling back to using the original user message as goal text.")
                    goal_text_to_add = original_user_message_for_goal  # Fallback to user message

                logger.info(f"Handling 'refine_goal' action for mission {mission_id}. Adding goal: '{goal_text_to_add[:60]}...'")
                # Add the goal to the context manager
                goal_id = await self.controller.context_manager.add_goal(
                    mission_id=mission_id,
                    text=goal_text_to_add,  # Use extracted content or fallback
                    source_agent=self.controller.messenger_agent.agent_name  # Record who added it
                )
                if goal_id:
                    logger.info(f"Added goal '{goal_id}' to mission {mission_id}: '{request_content[:50]}...'")
                    # Log the specific goal addition step
                    await self.controller.context_manager.log_execution_step(
                        mission_id=mission_id,
                        agent_name="AgentController",
                        action="Add User Goal",
                        input_summary=f"User goal feedback: {request_content[:60]}...",
                        output_summary=f"Stored goal {goal_id}.",
                        status="success",
                        full_input={'goal_text': request_content},
                        full_output={'goal_id': goal_id},
                        log_queue=log_queue,
                        update_callback=update_callback
                    )
                else:
                    logger.error(f"Failed to add goal to context manager for mission {mission_id}.")
                    # Log the failure
                    await self.controller.context_manager.log_execution_step(
                        mission_id=mission_id,
                        agent_name="AgentController",
                        action="Add User Goal",
                        input_summary=f"User goal feedback: {request_content[:60]}...",
                        output_summary="Failed to store goal.",
                        status="failure",
                        error_message="ContextManager.add_goal returned None.",
                        log_queue=log_queue,
                        update_callback=update_callback
                    )
                # Return the original agent_output, which contains the user-facing response
                return agent_output
            else:
                # Chat intent — RAG-grounded response from document library
                try:
                    doc_search_tool = getattr(self.controller, 'document_search_tool', None)
                    if doc_search_tool and self.controller.retriever:
                        # ── Step 1: Fetch document metadata (always) ──
                        doc_metadata_summary = ""
                        doc_title_lookup = {}  # filename -> authoritative title
                        doc_meta_lookup = {}   # filename -> {title, authors, year}
                        doc_id_to_meta = {}    # doc_id -> {title, authors, year}
                        try:
                            from database.database import get_db
                            from sqlalchemy import text as sql_text
                            db = next(get_db())
                            try:
                                # Scope the metadata list to the calling user.
                                # Group filter (if provided) layers on top.
                                from ai_researcher.user_context import get_current_user as _get_cu
                                _cu = _get_cu()
                                _uid = getattr(_cu, "id", None) if _cu else None

                                join_clause = ""
                                where_parts = []
                                params = {}
                                if _uid is not None:
                                    where_parts.append("d.user_id = :uid")
                                    params["uid"] = _uid
                                if document_group_id:
                                    join_clause = "JOIN document_group_association dga ON d.id = dga.document_id"
                                    where_parts.append("dga.document_group_id = :gid")
                                    params["gid"] = document_group_id

                                where_clause = (" WHERE " + " AND ".join(where_parts)) if where_parts else ""
                                rows = db.execute(sql_text(f"""
                                    SELECT d.original_filename, d.metadata_, d.id::text
                                    FROM documents d {join_clause}{where_clause}
                                    ORDER BY d.created_at DESC LIMIT 50
                                """), params).fetchall()
                                if rows:
                                    doc_lines = []
                                    for row in rows:
                                        meta = row[1] or {}
                                        title = meta.get("title") or row[0]
                                        authors = meta.get("authors", [])
                                        year = meta.get("publication_year", "")
                                        doc_type = meta.get("document_type", "")
                                        journal = meta.get("journal_or_source", "")
                                        doi = meta.get("doi", "")
                                        author_str = ", ".join(authors) if isinstance(authors, list) else str(authors)
                                        # Build authoritative lookups (by filename and by doc_id)
                                        meta_entry = {"title": title, "authors": authors, "year": year}
                                        doc_title_lookup[row[0]] = title
                                        doc_meta_lookup[row[0]] = meta_entry
                                        if len(row) > 2 and row[2]:
                                            doc_id_to_meta[row[2]] = meta_entry
                                        line = f"- {title}\n  Authors: {author_str} | Year: {year} | Type: {doc_type}"
                                        if journal:
                                            line += f" | Journal: {journal}"
                                        if doi:
                                            line += f" | DOI: {doi}"
                                        doc_lines.append(line)
                                    doc_metadata_summary = "\n".join(doc_lines)
                            finally:
                                db.close()
                        except Exception as e:
                            logger.debug(f"Failed to fetch document metadata: {e}")

                        # ── Step 2: Search chunks ──
                        logger.info(f"RAG chat: Searching chunks for '{user_message[:60]}...'")
                        search_results = await doc_search_tool.execute(
                            query=user_message,
                            document_group_id=document_group_id,
                            n_results=8,
                            use_reranker=True
                        )

                        # Build chunk context with truncation guard
                        document_context = ""
                        source_references = []
                        if search_results:
                            MAX_RAG_CONTEXT_CHARS = 12000
                            context_parts = []
                            total_chars = 0
                            for i, chunk in enumerate(search_results, 1):
                                chunk_text = chunk.get("text", "")
                                metadata = chunk.get("metadata", {})
                                source = metadata.get("original_filename") or "Unknown"
                                doc_id = metadata.get("doc_id") or chunk.get("doc_id") or ""
                                # Use authoritative doc table (by filename or doc_id), not chunk metadata
                                auth_meta = doc_meta_lookup.get(source) or doc_id_to_meta.get(doc_id, {})
                                title = auth_meta.get("title") or source
                                authors = auth_meta.get("authors") or metadata.get("authors", [])
                                year = auth_meta.get("year") or metadata.get("publication_year", "")
                                section_titles = metadata.get("section_titles", [])
                                section = section_titles[-1] if section_titles else ""

                                max_per_chunk = MAX_RAG_CONTEXT_CHARS // 8
                                if len(chunk_text) > max_per_chunk:
                                    chunk_text = chunk_text[:max_per_chunk] + "\n[... truncated]"

                                chunk_context = f"[Source {i} — from: {title}]\n{chunk_text}"

                                image_refs = metadata.get("image_refs", [])
                                if image_refs:
                                    img_lines = [f"[Available image: {r.get('alt_text', 'Figure')}](url:{r.get('path', '')})"
                                                 for r in image_refs if r.get("path")]
                                    if img_lines:
                                        chunk_context += "\n\nImages:\n" + "\n".join(img_lines)

                                total_chars += len(chunk_context)
                                if total_chars > MAX_RAG_CONTEXT_CHARS:
                                    break
                                context_parts.append(chunk_context)

                                # Build source reference
                                author_str = ", ".join(authors) if isinstance(authors, list) else str(authors)

                                # Extract page number from section titles (Marker embeds <span id="page-N">)
                                page_num = ""
                                for st in section_titles:
                                    page_match = re.search(r'page-(\d+)', str(st))
                                    if page_match:
                                        page_num = page_match.group(1)
                                        break

                                # Clean section title
                                clean_section = section
                                if clean_section:
                                    clean_section = re.sub(r'\*+', '', clean_section)
                                    clean_section = re.sub(r'<[^>]+>', '', clean_section)
                                    clean_section = clean_section.strip()
                                    if len(clean_section) < 3 or clean_section.startswith("http"):
                                        clean_section = ""

                                ref = f"- **[{i}]** {title}"
                                if author_str:
                                    ref += f" — {author_str}"
                                if year:
                                    ref += f" ({year})"
                                if clean_section:
                                    ref += f', Kap. "{clean_section}"'
                                if page_num:
                                    ref += f", S. {page_num}"
                                source_references.append(ref)

                            document_context = "\n\n---\n\n".join(context_parts)

                        # If nothing found at all, fall back to original response
                        if not doc_metadata_summary and not document_context:
                            logger.info("RAG chat: No documents or chunks found")
                            return agent_output

                        # ── Step 3: Build unified prompt with both metadata + chunks ──
                        doc_library_section = ""
                        if doc_metadata_summary:
                            doc_library_section = f"""
DOCUMENT LIBRARY (authoritative — this is the complete list of documents you have access to):
{doc_metadata_summary}
"""

                        excerpts_section = ""
                        if document_context:
                            ref_list = "\n".join(source_references)
                            excerpts_section = f"""
RELEVANT TEXT EXCERPTS (passages FROM the documents listed above — use these to answer content questions):
{document_context}

SOURCE REFERENCE LIST (use these for citations at the end of your response):
{ref_list}
"""

                        rag_system_prompt = f"""You are a helpful assistant that answers questions based on the user's document library.

{doc_library_section}{excerpts_section}RULES:
- For questions about what documents exist, authors, titles, years, or metadata: answer from the DOCUMENT LIBRARY section above.
- For questions about document content, arguments, or topics: answer from the TEXT EXCERPTS section above and cite sources using [1], [2], etc.
- At the end of your response, include a "Quellen" section as a markdown bullet list. For each [N] you cited, add a bullet point using the SOURCE REFERENCE LIST entry. Format: `- **[N]** Title — Authors (Year), Kapitel: "..."`. Do NOT modify source titles or authors.
- References and citations mentioned WITHIN the text excerpts are NOT documents in your library. Only the DOCUMENT LIBRARY list is authoritative.
- Do NOT invent or hallucinate document titles. If you don't know, say so.
- Images: The context may contain image references marked as [Available image: description](url:path).
  - When the user asks generally about graphics/images/figures: list what's available by description, ask which ones they want to see. Do NOT show all images at once.
  - When the user asks for a specific figure or diagram: only show it if the image path is explicitly referenced in the text excerpts. Do NOT guess which image file corresponds to a figure number.
  - If you cannot determine which image matches a requested figure, say so honestly.
  - Never show journal headers, logos, or decorative images — only figures, charts, diagrams, and tables."""

                        rag_messages = [
                            {"role": "system", "content": rag_system_prompt},
                            {"role": "user", "content": user_message}
                        ]

                        # Add chat history for conversational context
                        if chat_history:
                            history_messages = []
                            for hist_user, hist_assistant in chat_history[-3:]:
                                history_messages.append({"role": "user", "content": hist_user})
                                history_messages.append({"role": "assistant", "content": hist_assistant})
                            rag_messages = [rag_messages[0]] + history_messages + [rag_messages[-1]]

                        # Call LLM
                        rag_response, rag_model_details = await self.controller.model_dispatcher.dispatch(
                            messages=rag_messages,
                            agent_mode="messenger",
                            mission_id=mission_id,
                            log_queue=log_queue,
                            update_callback=update_callback
                        )

                        if rag_response and rag_response.choices and rag_response.choices[0].message.content:
                            response_text = rag_response.choices[0].message.content
                            response_text = re.sub(r'!\[([^\]]*)\]\(url:(/api/images/[^)]+)\)', r'![\1](\2)', response_text)
                            agent_output["response"] = response_text
                            logger.info(f"RAG chat: Generated grounded response")

                            if rag_model_details and mission_id:
                                await self.controller.context_manager.update_mission_stats(
                                    mission_id, rag_model_details, log_queue, update_callback
                                )
                        else:
                            logger.warning("RAG chat: LLM returned empty response, falling back to original")
                    else:
                        logger.debug("RAG chat: No document search tool or retriever available")
                except Exception as rag_error:
                    logger.error(f"RAG chat: Error during document-grounded response: {rag_error}", exc_info=True)

                return agent_output

        except Exception as e:
            logger.error(f"Error during MessengerAgent execution or goal handling: {e}", exc_info=True)
            # Return a default error response
            return {
                "response": f"Sorry, I encountered an error trying to process your message: {e}",
                "action": None,
                "request": None
            }

    async def refine_questions(
        self,
        mission_id: str,
        user_feedback: str,
        current_questions: List[str],
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, Any], None]] = None
    ) -> Tuple[List[str], str]:
        """
        Refines the research questions based on user feedback.
        Returns a tuple containing the updated list of questions and a response string for the user.
        """
        logger.info(f"Refining questions for mission {mission_id} based on user feedback: '{user_feedback[:50]}...'")
        
        # Prepare the prompt for question refinement
        prompt = f"""
You are a research assistant helping to refine research questions. Based on the user's feedback, modify the current list of research questions.

Current Questions:
{chr(10).join([f"- {q}" for q in current_questions])}

User Feedback:
{user_feedback}

Instructions:
1. Analyze the user's feedback carefully.
2. **If the feedback asks to increase the number of questions or add variety:** Keep the existing 'Current Questions' and ADD new, distinct questions based on the feedback and the original topic. Aim to expand the scope or explore different angles.
3. **If the feedback asks for other changes (e.g., rephrasing, focusing, removing):** Modify the 'Current Questions' list accordingly. You can rephrase, merge, remove, or replace questions.
4. Ensure all questions in the final list are clear, specific, and focused on the research topic.
5. Maintain a reasonable number of questions (typically 3-8, but allow more if explicitly requested).
6. Return ONLY the final list of questions, one per line, with no numbering or bullet points. Ensure the list includes both the original questions (if kept) and any new ones.
"""
        
        try:
            # Call the LLM to refine the questions
            async with self.controller.maybe_semaphore:
                response, model_details = await self.controller.model_dispatcher.dispatch(
                    messages=[{"role": "user", "content": prompt}],
                    agent_mode="planning",
                    mission_id=mission_id,  # Pass mission_id for cost tracking
                    log_queue=log_queue,  # Pass log_queue for UI updates
                    update_callback=update_callback  # Pass update_callback for cost tracking
                )
            
            # Update Stats
            if model_details:
                await self.controller.context_manager.update_mission_stats(mission_id, model_details, log_queue, update_callback)
            
            if response and response.choices and response.choices[0].message.content:
                content = response.choices[0].message.content
                refined_questions = [q.strip() for q in content.strip().split('\n') if q.strip()]
                
                # Log the refinement
                await self.controller.context_manager.log_execution_step(
                    mission_id, "AgentController", "Refine Questions",
                    input_summary=f"User Feedback: {user_feedback[:50]}...",
                    output_summary=f"Refined questions from {len(current_questions)} to {len(refined_questions)}.",
                    status="success",
                    full_input={'user_feedback': user_feedback, 'current_questions': current_questions},
                    full_output=refined_questions,
                    model_details=model_details,
                    log_queue=log_queue, update_callback=update_callback
                )
                
                # Update the questions in the mission context
                await self.controller.context_manager.update_mission_metadata(mission_id, {"refined_questions": refined_questions})

                # Construct the response string for the user
                response_string = "I've updated the questions based on your feedback:\n\n"
                response_string += "\n".join([f"- {q}" for q in refined_questions])
                response_string += "\n\nAny further changes, or type 'start research' to proceed."

                return refined_questions, response_string
            else:
                logger.error("LLM failed to refine questions.")
                error_response = "Sorry, I had trouble refining the questions. Please try again or proceed with the current ones:\n\n"
                error_response += "\n".join([f"- {q}" for q in current_questions])
                error_response += "\n\nType 'start research' to proceed."
                return current_questions, error_response  # Return original questions and error message

        except Exception as e:
            err_msg = f"Error refining questions: {e}"
            logger.error(err_msg, exc_info=True)
            
            # Log the failure
            await self.controller.context_manager.log_execution_step(
                mission_id, "AgentController", "Refine Questions",
                input_summary=f"User Feedback: {user_feedback[:50]}...",
                status="failure",
                error_message=err_msg,
                log_queue=log_queue, update_callback=update_callback
            )

            error_response = f"Sorry, an error occurred while refining questions: {e}\nPlease try again or proceed with the current ones:\n\n"
            error_response += "\n".join([f"- {q}" for q in current_questions])
            error_response += "\n\nType 'start research' to proceed."
            return current_questions, error_response  # Return original questions and error message

    async def confirm_questions_and_run(
        self,
        mission_id: str,
        final_questions: List[str],
        tool_selection: Dict[str, bool],
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, Any], None]] = None
    ) -> bool:
        """
        Confirms the final questions, stores tool selection, and prepares for research.
        Returns True if the process was started successfully.
        """
        logger.info(f"Confirming questions and settings for mission {mission_id}...")
        
        # Store final questions and tool selection in metadata
        await self.controller.context_manager.update_mission_metadata(mission_id, {
            "final_questions": final_questions,
            "tool_selection": tool_selection
        })
        logger.info(f"Stored final questions and tool selection ({tool_selection}) for mission {mission_id}.")
        
        # Log the confirmation step
        await self.controller.context_manager.log_execution_step(
            mission_id, "AgentController", "Confirm Questions and Settings",
            input_summary=f"Final Questions: {len(final_questions)}, Tools: {tool_selection}",
            output_summary="Confirmed questions and tool selection.",
            status="success",
            full_input={'final_questions': final_questions, 'tool_selection': tool_selection},
            log_queue=log_queue, update_callback=update_callback
        )

        # The actual start of the research (plan generation, execution) is triggered
        # by the UI based on the 'initializing' state set before calling this.
        # This function now primarily serves to store the confirmed data.
        return True
