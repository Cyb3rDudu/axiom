# Structured Briefing → Honor the Briefing & Direct Start

**Status:** Proposal / plan
**Predecessor:** `docs/plans/CHAT_MODES_AND_STRUCTURED_BRIEFING.md` (Feature 2 — structured briefing detection)
**Problem owner:** Chat-driven mission creation treats every specific assignment like open research.

## 1. The problem (observed)

A user sends a fully-specified assignment (KMU/Master-Hausarbeit prompt with
Leitfrage, Gliederung, Wortbudget, Fallunternehmen, Quellenanforderungen) and
gets one of two disappointing outcomes:

1. **Moderately specific prompt** → generic *generated* research questions
   (PESTEL / Porter / "in the context of global uncertainty …"), often in
   English despite a German prompt. ("Würfeln" — rolling the dice.)
2. **Highly specific prompt** → the five "Mögliche Unterfragen" from the prompt
   echoed back as *"Hier sind einige erste Forschungsfragen … Möchten Sie diese
   anpassen?"* — and the mission still sits in the approval loop, not running.

Either way the mission starts "extrem generisch" and the carefully written
briefing does not steer it.

## 2. Root cause (confirmed in code)

The structured-briefing detector **works** for the highly-specific case
(the prompt satisfies all 5 detector signals). What is broken / missing is
everything *after* detection:

| # | Finding | Evidence |
|---|---------|----------|
| **A** | The wrong questions are extracted. `## Zentrale Leitfrage` (singular) is not recognized because the header regex matches `leitfragen` (plural) only. The extractor falls back to "longest numbered block not under Gliederung" = the `Mögliche Unterfragen`. | `controller/utils/briefing_detector.py` `_LEITFRAGEN_HEADER_RE`; `extract_leitfragen` rule (2). |
| **B** | The full briefing is **discarded** by Planning. `briefing_style` / `full_briefing` has exactly **one** consumer in the whole codebase (`user_interaction.py`). `planning_agent.py` never reads them. | `grep -rln briefing_style` → only `user_interaction.py`. `planning_agent.run()` builds its prompt from `user_request` alone (`user_prompt = f"Research Request: {user_request}\n\n"`, line 606). |
| **C** | `user_request` is the **distilled** topic, not the briefing. In the chat path the mission is created with `user_request = request_content` (the MessengerAgent's extracted one-liner). The `## Empfohlene Gliederung`, Hauptthese, Fallunternehmen, Wortbudget are lost. | `user_interaction.py` start_research branch; `core_controller.run_mission` line 37 `user_request = mission_context.user_request` → passed straight to Planning. |
| **D** | Direct start is **not a design goal** today. The predecessor doc §3.4 explicitly says "Approval loop presents those user-provided questions." Auto-start was never specified. | `docs/plans/CHAT_MODES_AND_STRUCTURED_BRIEFING.md` Feature 2, point 4. |
| **E** | Generated questions ignore prompt language. When the detector does **not** fire (less specific prompt), `research_agent.generate_initial_questions` can produce English questions for a German prompt. | `_detect_language` runs on the response, not on the question-generation input. |

**Net effect:** Planning works from a distilled one-liner, so it reinvents the
outline and questions. That is exactly "die Richtung der Mission ändern".

## 3. Answer to the question

> *Kann man bei sehr spezifischen Missionsaufträgen direkt starten?*

**Yes.** The building blocks exist: briefing detector, full-briefing metadata,
the `/missions/{id}/start` background launcher, and the `approve-questions`
code path that already launches `run_mission` without a second click. The
missing pieces are (1) a stricter "complete assignment" classifier, (2) making
Planning actually honor the briefing (prerequisite — otherwise direct start
still yields generic output), and (3) an auto-start branch in
`handle_user_message`.

## 4. Plan (phased, smallest-first)

Each phase is independently shippable and each fixes real behaviour. Phases
are ordered so that **P1 is the root-cause fix and must land before direct
start actually produces good output.**

### Phase 0 — Fix the Leitfragen extraction (quick, ~1 file)
- In `briefing_detector.py`, extend `_LEITFRAGEN_HEADER_RE` to match the
  singular forms: `leitfrage(n)?`, `forschungsfrage(n)?`, and a prose line
  under `## Zentrale Leitfrage` (the primary question is often a single quoted
  sentence, not a numbered list).
- Add `extract_primary_leitfrage(message) -> Optional[str]` that returns the
  main research question when present.
- `extract_leitfragen` keeps its numbered-list behaviour for sub-questions, but
  when a primary Leitfrage exists it is returned as item[0].
- Tests: add fixtures `tests/agentic_layer/test_briefing_detector.py` for the
  Bergtech-style prompt (singular `Leitfrage`, `Mögliche Unterfragen`,
  `## Empfohlene Gliederung`).

**Effect:** the chat response and the stored questions reflect the *real*
research question, not the optional sub-questions.

### Phase 1 — Make Planning honor the briefing (ROOT CAUSE)
Goal: a structured briefing steers the outline verbatim; Planning stops
reinventing it.

- `planning_agent.run(...)` gains access to the mission metadata (it already
  receives `mission_id` and has `controller`). Read
  `metadata.briefing_style`, `metadata.full_briefing`,
  `metadata.initial_questions` / `final_questions`.
- When `briefing_style == 'structured'`:
  - Inject the **full briefing body** into the user prompt as
    `User Briefing (use verbatim):`, ahead of (or instead of) the distilled
    `Research Request:` line.
  - Use the structured system-prompt variant the predecessor doc described:
    *"Use the user's Leitfragen as the research questions and the user's
    Gliederung as the outline. Do not invent alternative questions or a
    different structure. If the user provided section strategies
    (research_based / content_based / synthesize_from_subsections), honour
    them."*
  - If the user's outline is parseable (`# 1. Einleitung`, `# 2. …`), seed
    the report outline from it rather than generating from scratch.
- **Mission `user_request` fix:** when a structured briefing is detected, set
  `mission_context.user_request` to the full briefing (or a faithful compact
  summary) so every downstream agent that only reads `user_request` also sees
  the real assignment. Keep `full_briefing` metadata as the exact original.
- Tests: unit test Planning prompt assembly with a structured-briefing fixture;
  integration test that the produced outline sections match the user's headings.

**Effect:** even **without** direct start, the mission now follows the
briefing. This alone removes "die Richtung ändern".

### Phase 2 — "Complete assignment" classifier + direct start
Goal: a very specific assignment skips the question/approval loop and starts.

- New pure helper in `briefing_detector.py`:
  `classify_assignment(message) -> dict` returning
  `specificity ∈ {'open','structured','complete'}` plus structured fields
  (`primary_question`, `questions`, `has_outline`, `has_scope`,
  `has_deliverable`, `deliverable`, `case_subject`).
  - `complete` = `structured` **and** has outline (≥3 Gliederung sections)
    **and** has scope (word count) **and** has a deliverable
    (Hausarbeit/Report/…). This is the "don't roll the dice" threshold.
- In `handle_user_message` (`user_interaction.py`) start_research branch, when
  `classify_assignment(...)['specificity'] == 'complete'`:
  1. Create the mission with `user_request` = full briefing (P1 change).
  2. Store `briefing_style='structured'`, `full_briefing`, and set
     `final_questions` from primary Leitfrage + sub-questions directly (skip
     the generic `generate_initial_questions` call).
  3. Replace the generic `questions_intro` / `questions_prompt` response with a
     concise acknowledgment: deliverable, word budget, # sections, case subject,
     primary Leitfrage — e.g. *"Verstanden: Hausarbeit (~3000 Wörter) zu
     Bergtech Maschinenbau GmbH, 6 Kapitel, Leitfrage: …. Starte die Mission."*
  4. Launch execution: reuse the same background-launch logic as
     `/chat/approve-questions` (set status `planning`, spawn `run_mission` in
     the thread pool with `set_current_user` + lifecycle registration).
- The chat response carries `action: 'start_research'` with `mission_id` so the
  existing frontend (`ChatPanel` `handleAgentAction`) opens the ResearchPanel
  and the Start button is already in a running/planning state.

**Effect:** for a complete assignment the user's message is acknowledged
specifically and the mission runs immediately — no generic questions, no
extra click.

### Phase 3 — UX & safety rails
- **Config toggle** (user setting + chat setting) `auto_start_complete_briefing`
  defaulting to ON for research chats, OFF in `chat_type='chat'`.
  - OFF behaviour for `complete` briefings: still skip generic question-gen,
    still show the specific acknowledgment, but land in `planning`/`ready` so
    the user clicks **Start** once (one prominent button, not a question loop).
- **Validation parity:** ensure `/missions/{id}/start`'s source check
  (web_search OR document_group) runs before auto-launch; if no source, fall
  back to the one-click state with the "⚠️ No sources enabled" warning the
  ResearchPanel already renders.
- **Keep open research untouched:** `specificity == 'open'` stays on the
  current generate-questions + approval path (P0/P1 still improve it because
  Planning now honors full text when a briefing sneaks through).
- **Language fix (E):** pass detected `language_code` into
  `research_agent.generate_initial_questions` so generated questions match the
  prompt language.

## 5. File-change summary

| Phase | File | Change |
|---|---|---|
| 0 | `controller/utils/briefing_detector.py` | singular header regex; `extract_primary_leitfrage` |
| 0 | `tests/agentic_layer/test_briefing_detector.py` | fixtures (Bergtech-style) |
| 1 | `agents/planning_agent.py` | read metadata.briefing_style/full_briefing; structured prompt variant; seed outline from user Gliederung |
| 1 | `controller/user_interaction.py` | set `user_request` = full briefing on structured detection |
| 1 | `tests/...` | Planning prompt + outline-seeding tests |
| 2 | `controller/utils/briefing_detector.py` | `classify_assignment` |
| 2 | `controller/user_interaction.py` | `complete`-assignment branch: skip question-gen, acknowledge specifically, background-launch `run_mission` |
| 3 | `services/feature_flags.py` + settings | `auto_start_complete_briefing` toggle |
| 3 | `controller/user_interaction.py` / `research_agent` | pass `language_code` to question generation |

No DB migration needed: `briefing_style`, `full_briefing`, `final_questions`
already live in the mission `metadata` JSONB; the toggle rides on existing
user/chat settings JSON.

## 6. What stays the same
- `chat_type='chat'` chat-only mode (no research at all) — unchanged.
- Open research (vague topic) — unchanged path, only better language handling.
- The `/missions/{id}/start` endpoint and ResearchPanel Start button — reused,
  not replaced.
- `approve_questions` / `refine_questions` loops — still available for the
  "structured but not complete" middle case.

## 7. Risks
- **Auto-start surprise:** a long research mission firing from a single
  message can be startling. Mitigated by the P3 toggle and by the specific
  acknowledgment message that states exactly what will run.
- **Outline seeding brittleness:** user Gliederungen vary in format. Mitigated
  by only *seeding* (Planning may adjust) and by keeping the structured prompt
  variant conservative.
- **Over-classification:** `complete` must stay strict (outline + scope +
  deliverable) to avoid auto-starting on casual but formatted messages.
  Mitigated by the ≥3-section outline requirement and the feature flag.
