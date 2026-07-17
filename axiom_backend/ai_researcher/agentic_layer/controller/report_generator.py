import logging
from typing import Dict, Any, Optional, List, Callable, Set, Tuple
import queue
import re
import datetime
import json
import hashlib

from ai_researcher.config import THOUGHT_PAD_CONTEXT_LIMIT
from ai_researcher.agentic_layer.async_context_manager import ExecutionLogEntry
from ai_researcher.agentic_layer.schemas.planning import ReportSection
from ai_researcher.agentic_layer.controller.literature_portfolio_manager import (
    LiteraturePortfolioManager,
)

logger = logging.getLogger(__name__)

class ReportGenerator:
    """
    Manages the report generation phase of the mission, including title generation
    and citation processing.
    """
    
    def __init__(self, controller):
        """
        Initialize the ReportGenerator with a reference to the AgentController.

        Args:
            controller: The AgentController instance
        """
        self.controller = controller
        self._portfolio_manager = LiteraturePortfolioManager(controller)

    async def _maybe_generate_portfolio(
        self,
        *,
        mission_id: str,
        full_draft: str,
        used_doc_ids: Set[str],
        doc_metadata_source: Dict[str, Any],
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, ExecutionLogEntry], None]] = None,
    ) -> str:
        """If enabled for this mission, build the Literaturportfolio and
        return a markdown snippet to append to the final report. Returns ''
        when disabled, no cited sources, or on any failure (non-fatal)."""
        try:
            output = await self._portfolio_manager.run_if_enabled(
                mission_id=mission_id,
                full_draft=full_draft,
                used_doc_ids=used_doc_ids,
                doc_metadata_source=doc_metadata_source,
                log_queue=log_queue,
                update_callback=update_callback,
            )
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "Literaturportfolio generation failed for mission %s (non-fatal): %s",
                mission_id,
                exc,
                exc_info=True,
            )
            return ""

        if output is None or not output.markdown_table:
            return ""

        await self.controller.context_manager.log_execution_step(
            mission_id, "LiteraturePortfolioAgent", "Generate Literaturportfolio",
            output_summary=(
                f"Portfolio generated: {output.compliance.source_count} sources, "
                f"{output.compliance.scientific_share:.0%} scientific, "
                f"traffic_light={output.compliance.traffic_light}."
            ),
            status="success",
            log_queue=log_queue, update_callback=update_callback,
        )
        return "\n\n" + output.markdown_table.strip()
        
    async def generate_report_title(
        self,
        mission_id: str,
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, ExecutionLogEntry], None]] = None
    ) -> bool:
        """
        Generates a title for the final report using the WritingAgent based on the
        original query, active goals/thoughts, and the first/last sections of the report.
        """
        logger.info(f"Generating report title for mission {mission_id}...")
        mission_context = self.controller.context_manager.get_mission_context(mission_id)

        if not mission_context or not mission_context.plan or not mission_context.report_content:
            logger.error(f"Cannot generate title: Mission context, plan, or report content missing for {mission_id}.")
            await self.controller.context_manager.log_execution_step(
                mission_id, "AgentController", "Generate Report Title",
                status="failure", error_message="Prerequisites missing (context, plan, or content).",
                log_queue=log_queue, update_callback=update_callback
            )
            return False

        user_request = mission_context.user_request
        report_outline = mission_context.plan.report_outline
        report_content_map = mission_context.report_content

        first_section_content = "[First section content not found]"
        last_section_content = "[Last section content not found]"

        # Get first top-level section ID and content
        if report_outline:
            first_section_id = report_outline[0].section_id
            first_section_content = report_content_map.get(first_section_id, f"[Content missing for section {first_section_id}]")

            # Get last top-level section ID and content
            # Need to handle nested structures to find the *true* last section content
            # Let's get the last top-level section object first
            last_top_level_section = report_outline[-1]
            # Now find the very last section in the depth-first order originating from this last top-level section
            # Use the utility function get_sections_in_order
            from ai_researcher.agentic_layer.controller.utils import outline_utils
            last_section_overall_id = outline_utils.get_sections_in_order([last_top_level_section])[-1].section_id  # Get last section in DFS order of the last top-level branch
            last_section_content = report_content_map.get(last_section_overall_id, f"[Content missing for section {last_section_overall_id}]")

        # Limit content length to avoid excessive token usage
        max_content_length = 1500  # Characters per section for context
        first_section_snippet = first_section_content[:max_content_length]
        last_section_snippet = last_section_content[:max_content_length]

        # Fetch Active Thoughts
        active_thoughts = self.controller.context_manager.get_recent_thoughts(mission_id, limit=THOUGHT_PAD_CONTEXT_LIMIT)
        thoughts_context = "\nRecent Thoughts (Focus Points & Reminders):\n---\n"
        if active_thoughts:
            for thought in active_thoughts:
                thoughts_context += f"- [{thought.timestamp.strftime('%Y-%m-%d %H:%M:%S')}] ({thought.agent_name}): {thought.content}\n"
        else:
            thoughts_context += "No recent thoughts.\n"
        thoughts_context += "---\n"

        # Format Active Goals
        active_goals = self.controller.context_manager.get_active_goals(mission_id)
        goals_context = "\nActive Goals:\n---\n"
        if active_goals:
            for goal in active_goals:
                # Check if goal is an object with description or just a string
                if hasattr(goal, 'description'):
                    goals_context += f"- {goal.description}\n"
                elif isinstance(goal, str):
                    goals_context += f"- {goal}\n"
                else:
                    goals_context += f"- {str(goal)}\n"  # Fallback
        else:
            goals_context += "No active goals.\n"
        goals_context += "---\n"

        # Construct the prompt for the title generation
        prompt = f"""
Generate a concise and compelling title for a research report based on the original user query, active goals, recent thoughts, and the content of the first and last sections.

Original User Query:
---
{user_request}
---
{goals_context}
{thoughts_context}
First Section Content (Snippet):
---
{first_section_snippet}
---

Last Section Content (Snippet):
---
{last_section_snippet}
---

Instructions:
1. Analyze the Original User Query, Active Goals, and Recent Thoughts to understand the core request, key objectives, and latest focus points. Determine the likely tone (e.g., academic, technical, general interest).
2. Consider the First Section (likely introduction/background) and Last Section (likely conclusion/summary) to grasp the report's scope and key takeaways.
3. Generate a title that accurately reflects the report's main topic and findings, incorporating insights from the goals and thoughts.
4. Match the title's tone to the inferred tone of the Original User Query and Goals.
5. The title should be concise (ideally 5-15 words).
6. Output ONLY the generated title text, with no extra formatting, quotes, or explanations.

CRITICAL: Do NOT include formatting like "**Title:**", "Title:", markdown, or any prefixes. Return ONLY the plain title text itself.
"""

        generated_title = None
        model_details = None
        log_status = "failure"
        error_message = "LLM call failed or returned empty content."

        try:
            # Use the model dispatcher directly with the 'writing' model configuration
            async with self.controller.maybe_semaphore:
                response, model_details = await self.controller.model_dispatcher.dispatch(
                    messages=[{"role": "user", "content": prompt}],
                    agent_mode="writing",  # Use the writing model configuration
                    mission_id=mission_id,  # Pass mission_id for cost tracking
                    log_queue=log_queue,  # Pass log_queue for UI updates
                    update_callback=update_callback  # Pass update_callback for cost tracking
                )

            if response and response.choices and response.choices[0].message.content:
                generated_title = response.choices[0].message.content.strip().strip('"')  # Remove surrounding quotes if any
                
                # Clean up common formatting patterns from thinking models
                generated_title = re.sub(r'^\*\*Title:\*\*\s*', '', generated_title, flags=re.IGNORECASE)
                generated_title = re.sub(r'^Title:\s*', '', generated_title, flags=re.IGNORECASE)
                generated_title = re.sub(r'^\*\*.*?\*\*:\s*', '', generated_title)  # Remove any **Label:** pattern
                generated_title = generated_title.strip()
                
                if generated_title:
                    # Store the title in metadata
                    await self.controller.context_manager.update_mission_metadata(mission_id, {"report_title": generated_title})
                    log_status = "success"
                    error_message = None
                    logger.info(f"Generated report title: '{generated_title}'")
                else:
                    error_message = "LLM returned empty content for title."
            else:
                error_message = "LLM response was invalid or missing content."

            # Update stats if details are available
            if model_details:
                await self.controller.context_manager.update_mission_stats(mission_id, model_details, log_queue, update_callback)

        except Exception as e:
            logger.error(f"Error during title generation LLM call for mission {mission_id}: {e}", exc_info=True)
            error_message = f"Exception during LLM call: {e}"
            # Keep generated_title as None

        # Log the outcome
        await self.controller.context_manager.log_execution_step(
            mission_id, "AgentController", "Generate Report Title",
            input_summary=f"Query: {user_request[:50]}..., First/Last section snippets provided.",
            output_summary=f"Generated Title: '{generated_title}'" if log_status == "success" else f"Failed: {error_message}",
            status=log_status, error_message=error_message,
            full_input={'user_request': user_request, 'first_section_snippet': first_section_snippet, 'last_section_snippet': last_section_snippet},
            full_output={'generated_title': generated_title}, model_details=model_details,
            log_queue=log_queue, update_callback=update_callback
        )

        return log_status == "success"

    def _map_note_id_to_doc_id(self, note_id: str, all_notes: List[Any]) -> Optional[str]:
        """
        Maps a note ID to its corresponding document ID.
        
        Args:
            note_id: The note ID to map (e.g., 'note_38c7c6a2')
            all_notes: List of all Note objects for the mission
            
        Returns:
            The corresponding document ID if found, None otherwise
        """
        # Find the note with the given ID
        note = next((n for n in all_notes if n.note_id == note_id), None)
        if not note:
            logger.warning(f"Could not find note with ID '{note_id}' in the mission notes.")
            return None
            
        # Extract the document ID from the source_id
        source_id_full = note.source_id
        source_type = note.source_type
        doc_id = None
        
        if source_type == "document":
            # For documents, source_id is the full UUID
            doc_id = source_id_full
        elif source_type == "web":
            # Generate a stable ID for web sources
            url_str = str(source_id_full)
            doc_id = hashlib.sha1(url_str.encode()).hexdigest()[:8]
        elif source_type == "internal":
            doc_id = source_id_full
        else:
            # Fallback
            doc_id = source_id_full
            
        return doc_id
    
    def _get_citation_mode(self, mission_id: str) -> str:
        """Get the citation mode for a mission (numbered or author_year)."""
        try:
            from services.citation_profiles import resolve_citation_profile
            mc = self.controller.context_manager.get_mission_context(mission_id)
            mission_metadata = mc.metadata if mc else None
            user_settings = None
            if mission_metadata:
                user_settings = mission_metadata.get("comprehensive_settings", {}).get("all_user_settings")
            profile = resolve_citation_profile(mission_metadata, user_settings)
            return profile.citation_mode
        except Exception as e:
            logger.warning(f"Failed to resolve citation profile for {mission_id}: {e}")
            return "numbered"

    def _build_author_year_bibliography(self, all_notes: list, doc_metadata_source: dict, mission_id: str) -> str:
        """Build a KMU APA 7 (German) bibliography from all research sources used in the mission.

        Format per KMU Akademie Zitierrichtlinien (Stand 25.03.2026):
        - Books: Autor, V. (Jahr). *Titel* (Auflage). Verlag.
        - Journals: Autor, V. (Jahr). Titel. *Zeitschrift, Band*(Heft), Seiten. DOI/URL
        - Web: Autor, V. (Jahr). *Titel*. Abgerufen am TT.MM.JJJJ, von URL
        - Without author: Institution as author, or title at author position.
        """
        today_str = datetime.datetime.now().strftime("%d.%m.%Y")

        # Deduplicate sources by source_id
        seen_sources = {}
        for note in all_notes:
            if note.source_type == "internal":
                continue
            source_key = note.source_id
            if source_key in seen_sources:
                continue
            meta = note.source_metadata if hasattr(note, 'source_metadata') and note.source_metadata else {}
            if isinstance(meta, dict):
                metadata = meta
            elif hasattr(meta, 'dict'):
                metadata = meta.dict()
            else:
                metadata = {}

            authors = metadata.get('authors', '') or metadata.get('author', '') or ''
            title = metadata.get('title', '') or ''
            year = metadata.get('publication_year', '') or metadata.get('year', '') or ''
            url = metadata.get('url', '') or ''
            journal = metadata.get('journal_or_source', '') or ''

            if note.source_type == "web" and not url:
                url = str(note.source_id)

            # Parse authors from JSON string if needed
            if isinstance(authors, str):
                try:
                    parsed = json.loads(authors)
                    if isinstance(parsed, list):
                        authors = ', '.join(str(a) for a in parsed)
                except (json.JSONDecodeError, TypeError):
                    pass

            # Extract 4-digit year
            year_match = re.search(r'(\d{4})', str(year))
            year_str = year_match.group(1) if year_match else ''

            if not title or title.strip() == '':
                title = metadata.get('original_filename', 'Unknown Source')

            # Skip entries with no useful info at all
            if not authors and not title:
                continue

            seen_sources[source_key] = {
                'authors': authors,
                'year': year_str,
                'title': title,
                'url': url,
                'journal': journal,
                'source_type': note.source_type,
            }

        if not seen_sources:
            logger.info(f"No sources found for author-year bibliography in mission {mission_id}")
            return ""

        # Sort alphabetically by author (or title if no author), then by year
        def sort_key(s):
            primary = s['authors'].lower() if s['authors'] else s['title'].lower()
            return (primary, s['year'])

        sorted_sources = sorted(seen_sources.values(), key=sort_key)

        # Build bibliography entries per KMU APA 7 format
        entries = []
        for src in sorted_sources:
            author_part = src['authors'] if src['authors'] else src['title']
            year_part = src['year'] if src['year'] else 'o. J.'
            is_web = src['source_type'] == 'web' or (src['url'] and not src['journal'])

            if is_web:
                # KMU format for online sources:
                # Autor (Jahr). *Titel*. Abgerufen am TT.MM.JJJJ, von URL
                if src['authors']:
                    entry = f"{author_part} ({year_part}). *{src['title']}*."
                else:
                    # No author: title at author position (not italicized per APA when at author pos)
                    entry = f"{src['title']} ({year_part})."
                if src['url']:
                    entry += f" Abgerufen am {today_str}, von {src['url']}"
            else:
                # Books / journal articles / other document sources
                entry = f"{author_part} ({year_part}). *{src['title']}*."
                if src['journal']:
                    entry += f" {src['journal']}."
                if src['url']:
                    entry += f" Abgerufen am {today_str}, von {src['url']}"

            entries.append(entry)

        bibliography = "## Literaturverzeichnis\n\n"
        bibliography += "\n\n".join(entries)

        logger.info(f"Built author-year bibliography with {len(entries)} entries for mission {mission_id}")
        return bibliography

    async def _persist_final_word_metrics(
        self, mission_id: str, final_string: str,
        content_words: int, banner_words: int, reference_words: int,
    ) -> None:
        """Persist the word-budget metrics computed from the EXACT final report
        string (review round 4).

        This MUST be called right before ``store_final_report()`` with the same
        string that is stored, so ``final_file_words`` always equals
        ``len(stored_report.split())`` — the report gains a title, literature
        portfolio and references section AFTER the early budget decision, so any
        earlier measurement would undercount the stored file.

        The over/under DECISION is still based on ``content_words`` (the budget /
        Umfang describes section content, not headings/title/references). The
        breakdown components are best-effort: ``heading_words`` is the residual
        (section headings + title + anything not accounted as content/banner/
        reference). ``final_file_words`` is exact.
        """
        try:
            mc = self.controller.context_manager.get_mission_context(mission_id)
            _wb = ((mc.metadata if mc else {}) or {}).get("word_budget") or {}
            _total_max = ((_wb.get("total_word_budget") or {})).get("max")
            if not _total_max:
                return  # no budget configured -> no metrics
            final_file_words = len(final_string.split())
            heading_words = max(
                0, final_file_words - content_words - banner_words - reference_words
            )
            if content_words > _total_max:
                over_by = content_words - _total_max
                logger.warning(
                    "FINAL WORD-BUDGET: mission %s over budget — content %d / max %d "
                    "(file %d = content %d + headings %d + banner %d + refs %d). "
                    "Marking completed_with_word_budget_warning.",
                    mission_id, content_words, _total_max, final_file_words,
                    content_words, heading_words, banner_words, reference_words,
                )
                await self.controller.context_manager.update_mission_metadata(
                    mission_id,
                    {
                        "word_budget_exceeded": {
                            "content_words": content_words,
                            "heading_words": heading_words,
                            "banner_words": banner_words,
                            "reference_words": reference_words,
                            "final_file_words": final_file_words,
                            "budget_max": _total_max,
                            "over_by": over_by,
                        },
                        "completed_with_word_budget_warning": True,
                    },
                )
            else:
                logger.info(
                    "FINAL WORD-BUDGET: mission %s OK — content %d / max %d "
                    "(file %d = content %d + headings %d + banner %d + refs %d).",
                    mission_id, content_words, _total_max, final_file_words,
                    content_words, heading_words, banner_words, reference_words,
                )
                # Clear stale exceeded/warning flags from a prior over-budget run.
                await self.controller.context_manager.update_mission_metadata(
                    mission_id,
                    {
                        "word_budget_exceeded": None,
                        "completed_with_word_budget_warning": None,
                    },
                )
        except Exception as _wbm_err:
            # Do NOT silently swallow: a configured academic word-budget report
            # must record its metrics or surface the failure explicitly. Mark
            # the mission so a missing budget metadata / stale-flag can never
            # masquerade as a clean OK run. (Nested try: the DB itself may be
            # the reason this failed.)
            logger.error(
                "Final word-budget metrics persistence FAILED for mission %s: %s",
                mission_id, _wbm_err, exc_info=True,
            )
            try:
                await self.controller.context_manager.update_mission_metadata(
                    mission_id,
                    {
                        "word_metrics_persistence_failed": {
                            "error": repr(_wbm_err)[:300],
                        }
                    },
                )
            except Exception:
                logger.error(
                    "Could not even set word_metrics_persistence_failed flag for "
                    "mission %s (DB unavailable?)", mission_id, exc_info=True,
                )

    async def process_citations(
        self,
        mission_id: str,
        log_queue: Optional[queue.Queue] = None,
        update_callback: Optional[Callable[[queue.Queue, ExecutionLogEntry], None]] = None
    ) -> bool:
        """Processes citation placeholders and generates the reference list."""
        logger.info(f"Processing citations for mission {mission_id}...")
        await self.controller.context_manager.log_execution_step(
            mission_id, "AgentController", "Process Citations",
            input_summary="Starting citation processing.", status="success",
            log_queue=log_queue, update_callback=update_callback
        )
        mission_context = self.controller.context_manager.get_mission_context(mission_id)
        if not mission_context or not mission_context.plan or not mission_context.report_content:
            logger.error(f"Cannot process citations: Mission context, plan, or report content missing for {mission_id}.")
            await self.controller.context_manager.log_execution_step(
                mission_id, "AgentController", "Process Citations",
                input_summary="Checking prerequisites", status="failure",
                error_message="Mission context, plan, or report content missing.",
                log_queue=log_queue, update_callback=update_callback
            )
            return False

        # Deterministic aggregate word-budget trim pass (review finding 3).
        # The per-section guard already trims each section, but defensively we
        # re-assert every section's own target_words_max on its STORED content
        # BEFORE assembling the draft. This guarantees sum(body) <=
        # sum(section_max), and (with correct budget allocation, finding 4) the
        # sum stays within total_word_budget.max. Trimming the stored content
        # (not the assembled string) keeps every section whole/coherent rather
        # than cutting the report mid-sentence.
        try:
            from ai_researcher.agentic_layer.controller.writing_manager import (
                _trim_to_word_budget,
            )
            _trimmed_any = False
            def _trim_outline_pass(section_list):
                nonlocal _trimmed_any
                for sec in section_list:
                    tw_max = getattr(sec, "target_words_max", None)
                    if tw_max:
                        _content = mission_context.report_content.get(sec.section_id, "")
                        if _content:
                            # HARD trim to target_words_max exactly (review
                            # finding 1): the 1.2x tolerance is allowed only
                            # DURING generation (writing_agent guard); the FINAL
                            # assembly trim must enforce the hard per-section max
                            # so sum(body) <= sum(section_max) <= total_max.
                            _hard = int(tw_max)
                            if len(_content.split()) > _hard:
                                mission_context.report_content[sec.section_id] = (
                                    _trim_to_word_budget(_content, _hard)
                                )
                                _trimmed_any = True
                    if sec.subsections:
                        _trim_outline_pass(sec.subsections)
            _trim_outline_pass(mission_context.plan.report_outline)
            if _trimmed_any:
                logger.info(
                    "Aggregate trim pass: one or more sections exceeded their own "
                    "target_words_max on assembly; trimmed to the hard limit."
                )
        except Exception as _trim_err:
            logger.warning("Aggregate word-budget trim pass skipped: %s", _trim_err)

        # Review finding 1: the briefing's word budget (Umfang) describes section
        # CONTENT, not the generated Markdown headings/numbering. The assembled
        # draft mixes both, so counting the assembled string would flag a false
        # over-budget whenever headings push the file past total_max even though
        # all section CONTENT is within budget (NexMach: section maxima already
        # sum to ~3,290; 18 headings add ~80 words). We therefore count CONTENT
        # words separately (stored section text only) and base the budget
        # decision on content_words; the final FILE word count (incl. headings
        # and the warning banner) is recorded as a separate metric.
        _content_words = 0
        def _count_content_pass(section_list):
            nonlocal _content_words
            for sec in section_list:
                _c = mission_context.report_content.get(sec.section_id, "")
                if _c:
                    _content_words += len(_c.split())
                if sec.subsections:
                    _count_content_pass(sec.subsections)
        _count_content_pass(mission_context.plan.report_outline)

        # Use recursive function to build draft with hierarchical numbering
        full_draft = ""
        # Modify the recursive function to accept and generate numbering prefixes
        def build_draft_recursive(section_list: List[ReportSection], level: int = 1, prefix: str = ""):
            nonlocal full_draft
            for i, section in enumerate(section_list):
                # Calculate the number for the current section
                current_number = f"{prefix}{i + 1}"
                # Generate the heading markdown
                heading_marker = "#" * level
                # Prepend the number to the title in the heading
                full_draft += f"{heading_marker} {current_number}. {section.title}\n\n"
                # Get the content for the section
                content = mission_context.report_content.get(section.section_id, f"[Content missing for section {section.section_id}]")
                full_draft += f"{content}\n\n"
                # Recursively call for subsections, passing the new prefix
                if section.subsections:
                    build_draft_recursive(section.subsections, level + 1, prefix=f"{current_number}.")

        # Initial call to the recursive function
        build_draft_recursive(mission_context.plan.report_outline)

        # Clean up any escaped brackets and underscores in the text that LLMs might produce
        # Handle various escaping patterns
        full_draft = full_draft.replace('\\[', '[').replace('\\]', ']')
        full_draft = full_draft.replace('\\\\[', '[').replace('\\\\]', ']')
        # Also handle escaped underscores in UUIDs (some LLMs escape these)
        full_draft = re.sub(r'\\(_)', r'\1', full_draft)
        
        # Normalize Unicode brackets to square brackets for consistent processing
        # Some LLMs use 【】 instead of []
        full_draft = full_draft.replace('【', '[').replace('】', ']')

        # Aggregate word-budget DECISION (the metric PERSISTENCE is deferred to
        # _persist_final_word_metrics(), called right before each
        # store_final_report() with the EXACT final string — review round 4: the
        # report gains a title, literature portfolio and references section AFTER
        # this point, so measuring final_file_words here would undercount the
        # stored file). The budget (Umfang) describes section CONTENT, so the
        # over/under decision is based on _content_words (stored section text
        # only, no headings/numbering). Only when CONTENT exceeds total_max do we
        # prepend a visible banner (banner_words tracked separately so
        # heading_words is not polluted by it).
        _banner_words = 0
        _over_content = False
        _total_max = None
        try:
            _wb = (mission_context.metadata or {}).get("word_budget") or {}
            _wb_total = (_wb.get("total_word_budget") or {})
            _total_max = _wb_total.get("max")
            if _total_max:
                _over_content = _content_words > _total_max
                if _over_content:
                    _budget_overrun = _content_words - _total_max
                    logger.warning(
                        "AGGREGATE WORD-BUDGET GUARD: mission %s CONTENT is %d "
                        "words, HARD total max is %d — over by %d even after the "
                        "per-section trim pass (section budgets likely sum > total). "
                        "Will mark completed_with_word_budget_warning.",
                        mission_id, _content_words, _total_max, _budget_overrun,
                    )
                    # Visible banner uses the CONTENT word count (what the user
                    # can actually trim) so the message is actionable.
                    _banner = (
                        f"> ⚠️ **Hinweis zur Wortanzahl:** Der Berichtstext umfasst "
                        f"{_content_words} Wörter und überschreitet damit das im "
                        f"Auftrag vorgegebene Limit von ca. {_total_max} Wörtern um "
                        f"{_budget_overrun} Wörter (ohne Überschriften gezählt). "
                        f"Bitte vor Abgabe entsprechend kürzen.\n\n"
                    )
                    full_draft = _banner + full_draft
                    _banner_words = len(_banner.split())
                else:
                    logger.info(
                        "Aggregate word-budget OK so far: mission %s content %d "
                        "words / max %d (final metrics persisted after assembly).",
                        mission_id, _content_words, _total_max,
                    )
        except Exception as _wbg_err:
            logger.warning("Aggregate word-budget decision skipped: %s", _wbg_err)
        
        # Get mission context to check for simple reference mappings
        has_simple_refs = False
        if mission_context and mission_context.reference_id_map:
            has_simple_refs = True
            logger.info(f"Mission {mission_id} has {len(mission_context.reference_id_map)} reference ID mappings")
        
        # Regex to find placeholders - now also supports simple refs like [ref1], [ref2], etc.
        # It captures the full content inside the brackets.
        # Now supports both 8-char hex IDs, full UUIDs, note_IDs, and simple refs (ref1, ref2, etc.)
        # Also supports Unicode brackets 【】 that some LLMs use
        uuid_or_hex = r'(?:[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}|[a-f0-9]{8})'
        simple_ref = r'ref\d+'  # Matches ref1, ref2, ref123, etc.
        # Match both square brackets [] and Unicode brackets 【】
        placeholder_pattern = re.compile(r'(?:\[|【)((?:' + uuid_or_hex + r'|note_[a-f0-9]{8}|' + simple_ref + r')(?:\s*,\s*(?:' + uuid_or_hex + r'|note_[a-f0-9]{8}|' + simple_ref + r'))*)(?:\]|】)')
        # Regex to extract individual UUIDs, 8-char hex IDs, note_IDs, or simple refs from the content within brackets
        id_pattern = re.compile(r'([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}|[a-f0-9]{8}|note_[a-f0-9]{8}|ref\d+)')

        # Build doc_metadata_source (mapping doc_id -> Note object for metadata lookup)
        all_notes = self.controller.context_manager.get_notes(mission_id)
        doc_metadata_source: Dict[str, Any] = {}
        note_id_to_doc_id_map: Dict[str, str] = {}  # Map note_ids to doc_ids
        
        for note in all_notes:
            source_id_full = note.source_id  # e.g., doc_abc_123 or https://...
            source_type = note.source_type
            lookup_key = ""
            if source_type == "document":
                lookup_key = source_id_full  # Use full UUID as doc_id
            elif source_type == "web":
                url_str = str(source_id_full)
                lookup_key = hashlib.sha1(url_str.encode()).hexdigest()[:8]
            elif source_type == "internal":
                lookup_key = source_id_full
            else:
                lookup_key = source_id_full  # Fallback

            if lookup_key and lookup_key not in doc_metadata_source:
                doc_metadata_source[lookup_key] = note
                
            # Store mapping from note_id to doc_id
            note_id_to_doc_id_map[note.note_id] = lookup_key

        # Find and Validate Used IDs
        used_doc_ids = set()
        all_matches = list(placeholder_pattern.finditer(full_draft))  # Find all potential placeholders first

        for match in all_matches:
            content_inside_brackets = match.group(1)
            potential_ids_in_match = id_pattern.findall(content_inside_brackets)
            for potential_id in potential_ids_in_match:
                original_id = potential_id  # Keep track of the original for replacement
                
                # Check if it's a simple reference ID (ref1, ref2, etc.) and translate back
                if potential_id.startswith('ref') and mission_context:
                    original_uuid = mission_context.get_original_reference_id(potential_id)
                    if original_uuid:
                        logger.info(f"Translated simple ref '{potential_id}' back to UUID '{original_uuid}'")
                        potential_id = original_uuid
                    else:
                        logger.warning(f"Could not translate simple ref '{potential_id}' back to UUID")
                        continue  # Skip this ID
                
                # Check if it's a note_id and map it to doc_id if needed
                elif potential_id.startswith('note_'):
                    if potential_id in note_id_to_doc_id_map:
                        mapped_doc_id = note_id_to_doc_id_map[potential_id]
                        logger.info(f"Mapped note ID '{potential_id}' to document ID '{mapped_doc_id}'")
                        # Use the mapped doc_id instead
                        potential_id = mapped_doc_id
                    else:
                        # Try to map it using the helper method
                        mapped_doc_id = self._map_note_id_to_doc_id(potential_id, all_notes)
                        if mapped_doc_id:
                            logger.info(f"Mapped note ID '{potential_id}' to document ID '{mapped_doc_id}' using helper method")
                            # Use the mapped doc_id instead
                            potential_id = mapped_doc_id
                        else:
                            logger.warning(f"Could not map note ID '{potential_id}' to a document ID")
                            continue  # Skip this ID
                
                # Validate against known sources BEFORE adding to used_doc_ids
                if potential_id in doc_metadata_source:
                    used_doc_ids.add(potential_id)
                else:
                    # Log invalid IDs found within a potential placeholder pattern
                    logger.warning(f"Found potential but invalid/unknown doc ID '{potential_id}' inside brackets: {match.group(0)}")

        if not used_doc_ids:
            # Check if this mission uses author-year citation mode
            # If so, build a bibliography from all research sources instead of [doc_id] placeholders
            citation_mode = self._get_citation_mode(mission_id)
            if citation_mode == "author_year":
                logger.info(f"Author-year mode: building bibliography from all research sources for mission {mission_id}")
                bibliography = self._build_author_year_bibliography(all_notes, doc_metadata_source, mission_id)
                if bibliography:
                    full_draft = full_draft.strip() + "\n\n" + bibliography
                # Author-year mode: every note's source counts as a citation
                # for the purpose of the Literaturportfolio (they all appear
                # in the bibliography). We pass the full doc_metadata_source
                # keyset so the portfolio reflects the same sources.
                portfolio_md = await self._maybe_generate_portfolio(
                    mission_id=mission_id,
                    full_draft=full_draft,
                    used_doc_ids=set(doc_metadata_source.keys()),
                    doc_metadata_source=doc_metadata_source,
                    log_queue=log_queue, update_callback=update_callback,
                )
                if portfolio_md:
                    full_draft = full_draft.strip() + portfolio_md
                _ay_final = full_draft.strip()
                # Review round 4: persist metrics from the EXACT final string
                # (incl. bibliography + portfolio), so final_file_words matches
                # the stored report. bibliography + portfolio count as references.
                _ay_ref_words = (
                    (len(bibliography.split()) if bibliography else 0)
                    + (len(portfolio_md.split()) if portfolio_md else 0)
                )
                await self._persist_final_word_metrics(
                    mission_id, _ay_final, _content_words, _banner_words, _ay_ref_words,
                )
                await self.controller.context_manager.store_final_report(mission_id, _ay_final)
                await self.controller.context_manager.update_mission_status(mission_id, "completed")
                await self.controller.context_manager.log_execution_step(
                    mission_id, "AgentController", "Process Citations",
                    output_summary=f"Completed (Author-year bibliography with sources from research notes).", status="success",
                    full_input={'draft_length': len(full_draft)}, full_output=full_draft.strip(),
                    log_queue=log_queue, update_callback=update_callback
                )
                return True

            logger.info(f"No valid citation placeholders containing known document IDs found in the draft for mission {mission_id}.")
            _nc_final = full_draft.strip()
            # Review round 4: persist metrics from the EXACT final string (no
            # references/title appended in this branch -> reference_words 0).
            await self._persist_final_word_metrics(
                mission_id, _nc_final, _content_words, _banner_words, 0,
            )
            await self.controller.context_manager.store_final_report(mission_id, _nc_final)
            await self.controller.context_manager.update_mission_status(mission_id, "completed")
            await self.controller.context_manager.log_execution_step(
                mission_id, "AgentController", "Process Citations",
                output_summary="Completed (No citations found/needed).", status="success",
                full_input={'draft_length': len(full_draft)}, full_output=full_draft.strip(),
                log_queue=log_queue, update_callback=update_callback
            )
            return True

        doc_citation_map = {}
        reference_entries = {}
        citation_counter = 1
        processed_text = full_draft  # Start with the original draft for replacement

        # Build map and reference list ONLY for validated IDs
        for doc_id in sorted(list(used_doc_ids)):  # Sort for consistent numbering
            if doc_id not in doc_citation_map:
                doc_citation_map[doc_id] = citation_counter
                # Metadata source already validated, so doc_id MUST be in doc_metadata_source
                metadata_note = doc_metadata_source[doc_id]
                ref_entry = f"{citation_counter}. Unknown Source ({doc_id})"  # Default reference

                if metadata_note:
                    source_type = metadata_note.source_type
                    metadata = metadata_note.source_metadata

                    logger.debug(f"Processing doc_id '{doc_id}': source_type='{source_type}', metadata keys: {list(metadata.dict().keys()) if hasattr(metadata, 'dict') else list(metadata.keys() if isinstance(metadata, dict) else 'Not a dict')}")

                    # Handle different metadata structure - check if it's a Pydantic model or dict
                    metadata_dict = metadata.dict() if hasattr(metadata, 'dict') else metadata
                    
                    if source_type == "document" or source_type == "document_window":
                        # Document Source Handling - fetch from PostgreSQL database
                        title = None
                        year = None
                        authors = None
                        journal = None
                        
                        # First try to get from PostgreSQL if doc_id looks like a UUID
                        uuid_pattern = re.compile(r'^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$', re.IGNORECASE)
                        
                        if uuid_pattern.match(doc_id):
                            # Fetch document from PostgreSQL
                            from database.database import get_db
                            from database.models import Document
                            db = next(get_db())
                            try:
                                doc = db.query(Document).filter(Document.id == doc_id).first()
                                if doc and doc.metadata_:
                                    title = doc.metadata_.get('title')
                                    year = doc.metadata_.get('publication_year') or doc.metadata_.get('year')
                                    authors = doc.metadata_.get('authors')
                                    journal = doc.metadata_.get('journal_or_source') or doc.metadata_.get('journal')
                                    
                                    # Fallback to filename if no title
                                    if not title:
                                        title = doc.original_filename or doc.filename
                                        if title and title.endswith('.pdf'):
                                            title = title[:-4]
                                    
                                    logger.debug(f"Document metadata from PostgreSQL for '{doc_id}': title='{title}', authors='{authors}', year='{year}', journal='{journal}'")
                                else:
                                    logger.warning(f"Document '{doc_id}' not found in PostgreSQL database.")
                            finally:
                                db.close()
                        
                        # If not found in PostgreSQL or not a UUID, try metadata from Note (legacy support)
                        if not title and not year and not authors:
                            if 'overlapping_chunks' in metadata_dict and metadata_dict['overlapping_chunks']:
                                # Extract from overlapping_chunks metadata (legacy)
                                chunk_metadata = metadata_dict['overlapping_chunks'][0]
                                title = chunk_metadata.get('title')
                                year = chunk_metadata.get('publication_year')
                                authors = chunk_metadata.get('authors')
                                journal = chunk_metadata.get('journal_or_source')
                                
                                logger.debug(f"Document metadata from overlapping_chunks for '{doc_id}': title='{title}', authors='{authors}', year='{year}', journal='{journal}'")
                            else:
                                # Fallback: try to extract from top-level metadata fields
                                logger.warning(f"Document '{doc_id}' missing or empty overlapping_chunks metadata. Trying fallback extraction.")
                                
                                title = metadata_dict.get('title') or metadata_dict.get('original_filename', f'Document {doc_id}')
                                year = metadata_dict.get('publication_year') or metadata_dict.get('year')
                                authors = metadata_dict.get('authors')
                                journal = metadata_dict.get('journal_or_source')
                                
                                # Remove file extension from title if it's a filename
                                if title and title.endswith('.pdf'):
                                    title = title[:-4]
                                
                                logger.debug(f"Document fallback metadata for '{doc_id}': title='{title}', authors='{authors}', year='{year}', journal='{journal}'")

                        # Process authors only if available and not the default placeholder
                        authors_str = None
                        if authors and authors != 'Unknown Authors':
                            try:
                                if isinstance(authors, str) and authors.startswith('[') and authors.endswith(']'):
                                    import ast
                                    authors_list = ast.literal_eval(authors)
                                    if isinstance(authors_list, list) and authors_list:  # Check if list is not empty
                                        authors_str = ", ".join(authors_list)
                                elif isinstance(authors, list) and authors:  # Check if list is not empty
                                    authors_str = ", ".join(authors)
                                elif isinstance(authors, str):  # Handle plain string authors if not list format
                                    authors_str = authors
                                # If parsing fails or authors is an empty list/string, authors_str remains None
                            except (SyntaxError, ValueError, TypeError) as parse_err:
                                logger.warning(f"Could not parse authors field for doc_id '{doc_id}': {authors}. Error: {parse_err}")
                                # Keep authors_str as None

                        # Check if year is available and not the default placeholder
                        year_str = str(year) if year and year != 'N/A' else None

                        # Check if title is available and not the default placeholder
                        title_str = title if title and title != 'Unknown Title' else None
                        # Check if journal is available and not the default placeholder
                        journal_str = journal if journal and journal != 'Unknown Journal/Source' else None

                        # Build APA-like reference string piece by piece
                        ref_parts = [f"{citation_counter}."]
                        if authors_str:
                            ref_parts.append(f"{authors_str}.")  # APA ends author list with '.'
                        if year_str:
                            ref_parts.append(f"({year_str}).")  # Year in parentheses with '.'
                        if title_str:
                            # APA uses sentence case for article titles, keeping original for now
                            ref_parts.append(f"{title_str}.")  # Title ends with '.'
                        if journal_str:
                            ref_parts.append(f"*{journal_str}*.")  # Journal italicized, ends with '.'

                        # Join the parts with spaces. If only counter exists, use default.
                        if len(ref_parts) > 1:
                            ref_entry = " ".join(ref_parts)
                        else:
                            # Fallback if no meaningful metadata was found
                            ref_entry = f"{citation_counter}. Unknown Document ({doc_id})"
                            logger.warning(f"Using fallback reference for doc_id '{doc_id}' as no meaningful metadata (authors/year/title/journal) was found.")

                    elif source_type == "web":
                        # Web Source Handling
                        title = metadata_dict.get('title', 'Unknown Title')
                        url = metadata_dict.get('url', doc_id)  # Use doc_id (which should be URL hash or similar) as fallback

                        # Get timestamp from the Note object itself
                        access_timestamp = metadata_note.created_at
                        access_date_str = "Unknown Date"
                        if isinstance(access_timestamp, datetime.datetime):
                            # Format as "Month Day, Year" e.g., "April 16, 2025"
                            access_date_str = access_timestamp.strftime("%B %d, %Y")
                        elif isinstance(access_timestamp, str):
                            # Attempt to parse if it's a string (ISO format expected)
                            try:
                                dt_obj = datetime.datetime.fromisoformat(access_timestamp.replace('Z', '+00:00'))  # Handle Z timezone
                                access_date_str = dt_obj.strftime("%B %d, %Y")
                            except ValueError:
                                logger.warning(f"Could not parse timestamp string '{access_timestamp}' for web source {doc_id}.")
                                access_date_str = access_timestamp  # Use raw string if parsing fails

                        # Web Source Handling (Academic Style + URL)
                        # Extract metadata fields similar to document sources
                        # The metadata here comes from the Note's source_metadata,
                        # which should now contain the output from MetadataExtractor
                        web_title = metadata_dict.get('title', 'Unknown Title')
                        web_year = metadata_dict.get('publication_year')  # Get raw value or None
                        web_authors = metadata_dict.get('authors')  # Get raw value or None
                        web_source_name = metadata_dict.get('journal_or_source')  # e.g., website name

                        # Process authors (similar to document handling)
                        web_authors_str = None
                        if web_authors and web_authors != 'Unknown Authors':
                            try:
                                if isinstance(web_authors, str) and web_authors.startswith('[') and web_authors.endswith(']'):
                                    import ast
                                    authors_list = ast.literal_eval(web_authors)
                                    if isinstance(authors_list, list) and authors_list:
                                        web_authors_str = ", ".join(authors_list)
                                elif isinstance(web_authors, list) and web_authors:
                                    web_authors_str = ", ".join(web_authors)
                                elif isinstance(web_authors, str):
                                    web_authors_str = web_authors
                            except (SyntaxError, ValueError, TypeError) as parse_err:
                                logger.warning(f"Could not parse authors field for web source '{doc_id}': {web_authors}. Error: {parse_err}")

                        web_year_str = str(web_year) if web_year else None
                        web_title_str = web_title if web_title and web_title != 'Unknown Title' else None
                        web_source_name_str = web_source_name if web_source_name else None

                        # Build APA-like reference string piece by piece
                        ref_parts = [f"{citation_counter}."]
                        if web_authors_str:
                            ref_parts.append(f"{web_authors_str}.")
                        if web_year_str:
                            ref_parts.append(f"({web_year_str}).")
                        if web_title_str:
                            # Use italics for web page titles? APA often uses sentence case. Let's keep it plain for now.
                            ref_parts.append(f"{web_title_str}.")
                        if web_source_name_str:
                            # Website name usually isn't italicized unless it's a formal publication name
                            ref_parts.append(f"Retrieved from {web_source_name_str}.")  # Indicate retrieval source

                        # Append URL and Access Date
                        ref_parts.append(f"Available at: {url}")
                        ref_parts.append(f"(Accessed: {access_date_str})")

                        # Join the parts. Use fallback if only counter and URL/Date exist.
                        if len(ref_parts) > 3:  # Check if more than just counter, URL, date exist
                            ref_entry = " ".join(ref_parts)
                        else:
                            # Fallback if minimal metadata was extracted
                            ref_entry = f"{citation_counter}. {web_title_str or 'Web Page'}. Available at: {url} (Accessed: {access_date_str})"
                            logger.warning(f"Using fallback reference for web source '{doc_id}' as minimal metadata was found.")

                    elif source_type == "internal":
                        # Internal/Synthesized Note Handling (Optional)
                        # Decide how to represent these if they are ever cited directly
                        ref_entry = f"{citation_counter}. Internal Synthesis ({doc_id}). Based on notes: {metadata_dict.get('synthesized_from_notes', [])}"
                        logger.warning(f"Cited an internal note '{doc_id}'. Representation may need refinement.")

                    else:
                        # Fallback for unknown source types or missing specific metadata
                        logger.warning(f"Could not determine reference format for doc_id '{doc_id}' (Source Type: {source_type}). Using default.")
                        ref_entry = f"{citation_counter}. Unknown Source Type ({doc_id})"

                else:
                    # Fallback if no metadata note was found for the doc_id
                    logger.warning(f"Could not find any metadata source note for doc_id '{doc_id}' used in text.")
                    # Keep the default ref_entry = f"{citation_counter}. Unknown Source ({doc_id})"

                reference_entries[citation_counter] = ref_entry
                citation_counter += 1

        # Replacement function to handle single or multiple IDs within brackets
        def replace_placeholder(match):
            content_inside_brackets = match.group(1)
            # Extract individual IDs from the matched content
            individual_ids_in_match = id_pattern.findall(content_inside_brackets)

            # Process each ID, mapping note_ids to doc_ids and simple refs to UUIDs if needed
            processed_ids = []
            for id_str in individual_ids_in_match:
                if id_str.startswith('ref') and mission_context:
                    # Map simple ref ID back to original UUID
                    original_uuid = mission_context.get_original_reference_id(id_str)
                    if original_uuid:
                        processed_ids.append(original_uuid)
                    else:
                        logger.warning(f"Could not map simple ref '{id_str}' back to UUID during replacement")
                elif id_str.startswith('note_'):
                    # Map note_id to doc_id
                    if id_str in note_id_to_doc_id_map:
                        processed_ids.append(note_id_to_doc_id_map[id_str])
                    else:
                        # Try to map it using the helper method
                        mapped_doc_id = self._map_note_id_to_doc_id(id_str, all_notes)
                        if mapped_doc_id:
                            processed_ids.append(mapped_doc_id)
                        else:
                            logger.warning(f"Could not map note ID '{id_str}' to a document ID during replacement")
                else:
                    # It's already a doc_id
                    processed_ids.append(id_str)

            # Look up numbers ONLY for VALID IDs found in this placeholder
            numbers = [str(doc_citation_map.get(doc_id)) for doc_id in processed_ids if doc_id in doc_citation_map]

            if numbers:
                # Sort the numbers numerically before joining
                sorted_numbers = sorted(numbers, key=int)
                # Format the replacement string, e.g., "[1, 2, 3]"
                return f"[{', '.join(sorted_numbers)}]"
            else:
                # This case now means the placeholder matched the pattern but contained ONLY invalid/unknown IDs
                logger.warning(f"Placeholder '{match.group(0)}' matched pattern but contained no known document IDs. Leaving unchanged.")
                return match.group(0)  # Leave it unchanged

        final_text_body = placeholder_pattern.sub(replace_placeholder, processed_text)

        num_references = len(reference_entries)
        references_section = ""
        if reference_entries:
            sorted_references = [reference_entries[i] for i in sorted(reference_entries.keys())]
            references_section = "\n\n## References\n\n" + "\n".join(sorted_references)

        # Prepend Report Title
        final_report_string = ""
        _title_words = 0
        mission_context_for_title = self.controller.context_manager.get_mission_context(mission_id)  # Re-fetch context
        if mission_context_for_title and mission_context_for_title.metadata:
            report_title = mission_context_for_title.metadata.get("report_title")
            if report_title:
                _title_block = f"# {report_title}\n\n"
                final_report_string += _title_block
                _title_words = len(_title_block.split())
                logger.info(f"Prepending report title: '{report_title}'")
            else:
                logger.warning(f"Report title not found in metadata for mission {mission_id}. Final report will not have a title.")
        else:
            logger.warning(f"Mission context or metadata not found when trying to prepend title for mission {mission_id}.")

        # Insert the Literaturportfolio (if enabled) between body and references
        # so the references list still sits at the very end of the document.
        portfolio_md = await self._maybe_generate_portfolio(
            mission_id=mission_id,
            full_draft=final_text_body,
            used_doc_ids=used_doc_ids,
            doc_metadata_source=doc_metadata_source,
            log_queue=log_queue, update_callback=update_callback,
        )
        body_with_portfolio = final_text_body.strip()
        if portfolio_md:
            body_with_portfolio = body_with_portfolio + portfolio_md

        final_report_string += body_with_portfolio + references_section

        _cite_final = final_report_string.strip()
        # Review round 4: persist metrics from the EXACT final string, which in
        # this branch includes a title, portfolio and references section. Those
        # count as reference/overhead words (title + portfolio + refs).
        _cite_ref_words = (
            _title_words
            + (len(portfolio_md.split()) if portfolio_md else 0)
            + len(references_section.split())
        )
        await self._persist_final_word_metrics(
            mission_id, _cite_final, _content_words, _banner_words, _cite_ref_words,
        )
        await self.controller.context_manager.store_final_report(mission_id, _cite_final)
        await self.controller.context_manager.update_mission_status(mission_id, "completed")
        logger.info(f"Citation processing complete. {num_references} unique references generated for mission {mission_id}.")
        await self.controller.context_manager.log_execution_step(
            mission_id, "AgentController", "Process Citations",
            output_summary=f"Completed ({num_references} references generated).", status="success",
            full_input={'draft_length': len(full_draft), 'used_doc_ids': list(used_doc_ids)},
            full_output={'final_text': final_text_body.strip(), 'references': reference_entries},
            log_queue=log_queue, update_callback=update_callback
        )
        return True
