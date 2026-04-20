# Chat Modes + Structured Briefing

**Status:** Implementation in progress
**Scope:** Two orthogonal UX/intent-classification improvements in the MessengerAgent path.
**Motivation:** Users complain about three distinct pain points that all trace back to the MessengerAgent treating every chat as a potential research mission and distilling every prompt into a one-liner before Planning sees it:

1. A detailed, structured briefing (KMU-Hausarbeit-style prompt with explicit Leitfragen, Gliederung, Quellenstrategie) is distilled down to the topic sentence — Planning-Agent invents generic research questions from scratch instead of honouring the user's outline.
2. Conversational messages about documents ("erkläre mir", "fasse zusammen", "analysiere") accidentally trigger full research missions because of keyword matches.
3. Genuinely open research (vague topic, let the agent shape it) works today and must keep working.

These three cases are two orthogonal dimensions, not three unrelated bugs.

## 1. The two dimensions

| Dimension | Values | Per |
|---|---|---|
| **Chat mode** | `chat` (conversational only) vs. `research` (research-enabled) | set at chat-creation time; stored in `chats.chat_type` |
| **Briefing style** (only relevant in `research` mode) | `open` (agent invents Leitfragen) vs. `structured` (user supplies Leitfragen + outline) | detected per-message from structural signals |

Mapping to the user's three scenarios:

| Scenario | Chat mode | Briefing style |
|---|---|---|
| KMU-Hausarbeit prompt | `research` | `structured` |
| Plain conversation about RAG docs | `chat` | — |
| Open research (vague topic) | `research` | `open` |

## 2. Feature 1 — chat-only mode (`chat_type='chat'`)

### Goal
When the user opts into `chat_type='chat'` on a chat, the MessengerAgent may never return intent `start_research` or `refine_questions`. Every user message is treated as conversation. Users who *do* want to escalate into a mission must explicitly open a new research chat (or, future UI, press an "escalate to mission" button).

### Why at the chat level, not per-message?
Per-message intent-classification drifts — a user asking "analysiere diese Textstelle" inside a content discussion is still conversing, not starting a mission. A durable mode flag on the chat matches user expectation and avoids the whack-a-mole of refining the intent prompt for edge keywords.

### Changes

1. **API `POST /chats/`** already accepts `chat_type`. Add `'chat'` to the accepted values; default stays `'research'` for backward compat.
2. **Chat creation UI** (follow-up; not in this change) gets a toggle. For now we accept `chat_type='chat'` via API and DB.
3. **`handle_user_message`** (`user_interaction.py`) looks up the chat's `chat_type` from DB when `chat_id` is known and passes it to `MessengerAgent.run()` as a new parameter `chat_mode`.
4. **`MessengerAgent`** accepts `chat_mode` and adjusts its system prompt:
    - `chat_mode='chat'`: the allowed intent set is `{chat, refine_goal, approve_questions}`. The prompt explicitly forbids `start_research` and `refine_questions`.
    - `chat_mode='research'` (default): today's behaviour.
5. **Safety net**: if the LLM returns `start_research` despite the gating (rare), `handle_user_message` downgrades it to `chat` before acting, so no mission is accidentally created.

### Tests
- Unit: `MessengerAgent.run(chat_mode='chat', user_message="...")` with a prompt that today triggers `start_research` — assert intent ∈ {chat, refine_goal, approve_questions}.
- Integration: create a `chat_type='chat'` chat, send "fasse mir mal die VWL-Bücher zusammen", assert **no mission row** is created.

## 3. Feature 2 — structured briefing detection + passthrough

### Goal
When the user's message *in research mode* contains a structured briefing (multiple markdown headings, numbered Leitfragen, explicit word-count etc.), axiom treats it as a ready-made plan and stops re-inventing the wheel. Planning-Agent must receive the user's Leitfragen and outline **verbatim** and produce those as its Phase-1 output.

### Signals we detect (heuristic)

A message is classified as **structured briefing** when it matches **≥ 3** of:

- Contains ≥ 2 markdown `##` headings *other than the title line*.
- Contains a numbered Leitfragen-style list (≥ 3 consecutive `^\d+\. ` lines, each ≥ 40 characters).
- Contains a word-count hint (`/\b\d[\d.,]*\s*(?:Wörter|words)\b/i`).
- Contains an academic-task keyword (`Hausarbeit`, `Seminararbeit`, `Case Study`, `Bachelorarbeit`, `Masterarbeit`, `Term Paper`, `Thesis`, `Essay`).
- Contains an explicit source-pool directive (regex matching keywords like `Quellen`, `Datenbanken`, `APA 7`, `Literatur`).

The detector is a pure function (`detect_structured_briefing(user_message) -> bool`). Testable without any LLM.

### Passthrough flow

1. **`handle_user_message`** runs the detector on the raw user message *before* calling the messenger.
2. After the messenger returns, if intent was `start_research` *and* the detector fired on the original message:
    - The mission's `user_request` and `metadata.full_briefing` get the **full message body** (not the messenger's distilled `extracted_content`).
    - Any numbered Leitfragen-list is parsed into `metadata.initial_questions` (or replaces it if the messenger set one).
    - `metadata.briefing_style = 'structured'`.
    - A goal entry `briefing_style=structured — use user's Leitfragen and outline verbatim` is added to `goal_pad`.
3. **`PlanningAgent` Phase 1** checks `mission.metadata.briefing_style`. If `structured`:
    - Prompt variant `system_prompt_structured` (DE + EN) tells the agent to **use** the user's Leitfragen from `metadata.initial_questions` as the research questions and the user's Gliederung (if present) as the outline, rather than generating from scratch.
    - The outline's section-strategy tags (`research_based`, `content_based`, `synthesize_from_subsections`) are honoured as written if the user provided them.
4. Approval loop presents those user-provided questions, not synthetic ones.

### Tests
- Unit: `detect_structured_briefing` true/false on fixtures.
- Unit: Leitfragen parser on fixtures (numbered list extraction).
- Integration: end-to-end "KMU Hausarbeit prompt" → mission created with `briefing_style='structured'`, `initial_questions` == user's 4 Leitfragen, Planning-Agent prompt variant selected.

## 4. File changes (summary)

| File | Feature | Change |
|---|---|---|
| `api/schemas.py` | 1 | (already) `ChatCreate.chat_type` — extend docstring to mention `'chat'` |
| `api/chats.py` | 1 | validate `chat_type ∈ {'research', 'chat', 'writing'}` |
| `ai_researcher/agentic_layer/controller/user_interaction.py` | 1+2 | fetch `chat_type` from DB; pass `chat_mode` to messenger; run structured-briefing detector; patch mission metadata on structured briefing |
| `ai_researcher/agentic_layer/agents/messenger_agent.py` | 1 | accept `chat_mode`; adjust system prompt to gate intents when `chat_mode='chat'`; safety-net downgrade |
| `ai_researcher/agentic_layer/controller/utils/briefing_detector.py` (new) | 2 | `detect_structured_briefing`, `extract_leitfragen` |
| `ai_researcher/agentic_layer/agents/planning_agent.py` | 2 | select structured prompt variant when `metadata.briefing_style == 'structured'`; honour user's initial_questions |
| `tests/agentic_layer/test_briefing_detector.py` (new) | 2 | detector + parser tests |
| `tests/agentic_layer/agents/test_messenger_chat_mode.py` (new) | 1 | messenger chat-mode gating |

No DB schema migration needed: `chat_type` column already exists as VARCHAR; `briefing_style` lives inside the free-form `metadata` JSONB blob on mission context.

## 5. Rollout

1. Ship Feature 1 + Feature 2 together (they're cohesive but independent).
2. Default `chat_type` stays `'research'` — no existing chat changes behaviour.
3. Frontend toggle for `chat_type` is a separate, future change (doesn't block backend shipping).
4. `briefing_style` detection is conservative (≥ 3 signals) to avoid false positives; open research remains the default for casual prompts.

## 6. Non-goals

- No auto-promotion from `chat_type='chat'` into mission. Escalation requires a new chat, by design.
- No structured-briefing detection in `chat_type='chat'` mode — that mode never triggers research.
- No refactor of the Planning-Agent's phase model; we only add a prompt variant.
- No change to existing open-research behaviour.
