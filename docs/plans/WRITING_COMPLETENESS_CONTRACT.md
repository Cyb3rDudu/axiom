# Writing Completeness Contract — Plan

**Status:** proposed
**Related:** Epic #41 (writing UX), #51 (structured bibliography),
#61 (writing-mode portfolio)
**Motivation:** Live Hausarbeit runs (Apr 24) exposed four user-visible
problems that break the "user gives a prompt, gets back a complete
deliverable" promise:

1. **Truncation leaks into the UX.** Token-budget overflows produce
   cut-off responses with a warning pill. The user now has to know
   about 8192-token caps, figure out what's missing, and craft a
   follow-up prompt.
2. **Hallucinated Wortbilanz.** The writer claims 2,910 words when
   the body is 2,370. The summary line is unreliable every single
   turn — observed delta 15–25 %.
3. **Sources disappear from chat on follow-ups.** When a turn
   doesn't re-emit `content-block:references` (e.g. "expand section 4,
   keep registry unchanged"), the user sees no sources in the chat —
   only in the DraftTab → References panel, which isn't obvious.
4. **Figure placeholders stay empty.** The writer emits
   `![Abbildung 1: …](placeholder-fig1.png)` — fake paths. The mission
   has already extracted images into `document_images` (pgvector CLIP
   embeddings), but that library is never consulted during writing.

## Contract the user should see

**"Ein Prompt → ein vollständiges Ergebnis."** Every writer response
the user sees in chat MUST contain:

1. The full prose deliverable (all sections complete, no truncation
   artifacts, no "continue here" warnings).
2. An accurate Wortbilanz header (actual count, not hallucinated).
3. A sources block rendered in chat — either inline or as the
   collapsed pill (#78), showing what the backend currently has
   persisted for this draft, not just what the LLM happened to emit
   this turn.
4. Figures where requested, resolved to real image URLs pulled from
   `document_images`, not placeholder paths.

Backend plumbing (retries, continuations, word counting, image
lookup) is invisible to the user.

## Non-goals

- **No new agent layer.** Reuse `SimplifiedWritingAgent` + existing
  post-response pipeline.
- **No new LLM provider.** Stay on DeepSeek-chat; the 8192 cap is a
  constraint, not a reason to switch.
- **No figure generation.** Only surface images that already exist
  in `document_images`. Generating new charts from CSV data is a
  separate epic.
- **No change to the #51 structured-bibliography data model.** We
  consume it, we don't rearchitect it.

## Stages — smallest lift first

### Stage 1 — Deterministic post-processing (~1 day)

**Goal:** Fix Wortbilanz hallucination + always-sources-in-chat
without touching the writer prompt or adding LLM calls.

**Scope:**

1. **Deterministic word counter in the audit pipeline.**
   - Extend `services/writing_response_audit.py` (#47) with a
     `recompute_wortbilanz(content)` function that:
     - Parses the document block
     - Counts words per section (split on `^# `, strip markdown
       citations, strip figure captions, strip `[NNN]` counters)
     - Rewrites the `Wortbilanz:` trailer to reflect real counts
   - Call it from `process_writing_chat_in_background` before
     persisting the assistant message to DB. What the user sees =
     what the backend counted.
   - Add the declared-vs-actual delta as a structured log entry
     (#74 telemetry) so we can track the hallucination rate over
     time.

2. **Always-visible sources block on every turn.**
   - After the existing references-block parse + persistence, the
     backend looks at the draft's current `draft_references` set
     (not just what the LLM emitted this turn) and generates a
     canonical `content-block:references` fence that lists the
     current registry state.
   - If the LLM's response already contains a references block,
     backend replaces it with the canonical one. If not, backend
     appends it at the end of the response (after `content-block:document`).
   - This means follow-up prompts like "expand section 4, don't
     re-emit refs" still show the user the sources in chat — the
     backend synthesizes the block from the registry.
   - The MessageBubble pill (#78) already collapses this to a
     compact summary, so the user experience is clean.

**Acceptance:**
- Hausarbeit turn with declared 2,910 words and actual 2,370 shows
  "2,370 insgesamt" to the user, not the hallucinated number.
- Follow-up prompt that says "don't re-emit references" still shows
  the 18-entry registry pill in chat.
- Tests: unit coverage for `recompute_wortbilanz` on known fixtures;
  integration test asserting the backend-synthesized refs block
  matches the current `draft_references` state after an upsert.

**Risk:** Low. Pure post-processing, idempotent, can be feature-
flagged off if it breaks a turn.

### Stage 2 — Transparent continuation (~2 days)

**Goal:** Make truncation invisible. User's response always contains
the full document. Backend handles continuation deterministically,
without the "model restarts the whole draft" failure mode from the
current auto-continue.

**Why the current auto-continue failed:** The continuation prompt
said "resume where you stopped." DeepSeek ignored and re-emitted the
whole `content-block:document` from scratch. Two full drafts
concatenated → Apply-all-Blocks produced a doubled body.

**Scope — section-scoped continuation:**

1. **Detect truncation + the cut point structurally.**
   - When `finish_reason == "length"`, backend parses the partial
     document and identifies:
     - Which `# N. <section>` heading is last-completed (word count
       within tolerance of its target budget)
     - Which section got cut mid-content (last partial sentence)
     - Which sections were never started (e.g. truncation hit in
       section 3, sections 4+5 missing entirely)

2. **Section-scoped continuation call.**
   - Fire a new LLM call with a **scoped prompt**:
     ```
     The following Hausarbeit draft stopped mid-writing. Continue
     from where it stopped, emit ONLY the remaining sections:
     [section 4 start] ... [section 5 end].
     The already-written sections {1, 2, 3} are locked — do not
     repeat or reference them by "as mentioned above". Target
     budgets: section 4 = 860 words, section 5 = 320 words.
     Emit a single content-block:document containing just the
     missing sections, NO wrapping prose, NO reopening of the
     references block.
     ```
   - Backend deterministically stitches: [partial section 3] +
     [continuation section 3 tail] + [section 4] + [section 5].
   - Up to 2 continuation attempts. Third truncation → surface the
     warning (same as today) because at that point the prompt is
     simply too large.

3. **Assembly guard.**
   - After stitch, backend re-validates: H1 headings count matches
     expected sections, no duplicate `# N.` headings, all figure
     placeholders still reference declared figures, Wortbilanz
     recomputed (via Stage 1).

**Acceptance:**
- A prompt that fits the single-call budget behaves identically to
  today (fast path unchanged).
- A prompt that triggers truncation at 8192 tokens completes
  transparently within 2 total calls, user sees a single coherent
  draft with correct section count.
- No doubled sections in output (the failure mode from PR #83's
  motivation).
- If 3 continuations still truncate, user sees the honest warning
  and telemetry records `truncation_unrecoverable`.

**Risk:** Medium. Stitching requires a correct cut-point detector;
edge cases (figure caption mid-truncation, code block spanning the
cut) need careful handling. Feature-flag gated on
`WRITING_TRANSPARENT_CONTINUATION_ENABLED` (default off for the
first week, flipped on after dogfooding).

### Stage 3 — RAG figure resolution (~2 days)

**Goal:** When the prompt asks for figures, the writer emits real
image links from `document_images`, not placeholder paths.

**Scope:**

1. **Figure-intent detection in the prompt.**
   - Cheap keyword match on the user prompt: "Abbildung", "Figure",
     "Chart", "Diagramm", explicit `![…](placeholder`, numbered
     figure list in prompt. If any hit → figure-resolution path
     activates.

2. **Pre-fetch candidate figures from RAG.**
   - For each figure description the user requested (e.g. "Chinas
     BIP und Leistungsbilanzsaldo 2000-2024"), the backend runs:
     - Text embedding of the description (BGE-M3, already in GPU
       worker)
     - CLIP embedding of the description (if image model available)
     - `pgvector_store.query_multimodal(text, image, n_results=3)`
       scoped to the writing session's document groups
     - Returns top-k matches with `image_path`, `alt_text`, parent
       document metadata
   - Injects the candidates into the writer system prompt as:
     ```
     AVAILABLE FIGURES FROM YOUR DOCUMENT LIBRARY:
     For your requested "Chinas BIP und Leistungsbilanzsaldo":
       ![Abbildung X](/api/documents/images/{doc_id}/{file})
       Caption hint: "Chart from Körner (2014), p. 145"
       Relevance: 0.82
     If one fits, COPY ITS URL VERBATIM into your
     Abbildung-Markdown. Do NOT fabricate paths.
     ```

3. **Backward compatibility.**
   - If no figure-intent in the prompt → no change, no extra RAG
     query, no cost impact.
   - If figure-intent but `document_images` table is empty for the
     session's groups → writer falls back to placeholder paths +
     the response pill flags "0 figure candidates found in RAG".

4. **Fall-back image hosting.**
   - Response parser re-validates figure URLs: if a path points at
     a non-existent `document_images.image_id`, replace with
     placeholder + telemetry warning `figure_lookup_missed`.

**Acceptance:**
- Hausarbeit turn with 3 figure placeholders + document group
  containing matching Chart-extractions → 3 real image URLs in the
  final response that render in the editor and export in DOCX.
- Hausarbeit turn without figure-intent → no additional RAG queries
  (cheap path preserved).
- Hausarbeit turn with figure-intent but empty library → clean
  placeholder flow, user sees telemetry hint in the response pill.

**Risk:** Medium-high. CLIP query accuracy for German academic
charts is uneven; text-only matching is the safer default. Figure-
captions may need LLM-assisted matching in a future pass.

### Stage 4 — Document state machine (deferred, ~5 days)

**Only if Stages 1-3 don't hit the reliability bar after 2 weeks of
dogfooding.**

**Full plan-and-execute rewrite of the writing turn:**
- Planner pass: reads prompt + current draft, emits JSON plan
  `{sections: [{id, title, target_words, key_sources, figures}],
  references_delta: [...]}`
- Section executors: one LLM call per section, scoped prompts with
  the planner's budget + source constraints
- Assembler: deterministic Python stitches [refs-block] +
  [section 1] + ... + [section N] + [recomputed Wortbilanz]

Trades latency (N+1 calls per turn) for reliability (each sub-call
has ample token headroom). Stage 4 would only ship if Stages 1-3
still leave a user-visible gap after real-world use.

## Implementation order + dependencies

```
Stage 1 (audit + sources injection)
  └── Stage 2 (continuation) depends on Stage 1's audit extension
       └── Stage 3 (figures) independent, can ship parallel to S2
            └── Stage 4 (full state machine) only if S1-S3 insufficient
```

Proposed GitHub issues:

- Epic: **Writing Completeness Contract (#XX)**
  - Sub #XX: Stage 1a — deterministic Wortbilanz recompute
  - Sub #XX: Stage 1b — always-append sources block from registry
  - Sub #XX: Stage 2a — truncation cut-point detector + section map
  - Sub #XX: Stage 2b — section-scoped continuation + stitcher
  - Sub #XX: Stage 3a — figure-intent detector + pre-fetch
  - Sub #XX: Stage 3b — figure URL validator + fallback
  - Sub #XX: documentation + rollout flag

## Rollout

- `WRITING_COMPLETENESS_CONTRACT_ENABLED=true` single env var gates
  all four stages. Default off.
- Per-stage feature flags on user settings for granular dogfooding:
  `writing.completeness.wordcount_fix`,
  `writing.completeness.sources_always`,
  `writing.completeness.transparent_continuation`,
  `writing.completeness.rag_figures`.
- Ship Stage 1 behind its flags first → flip all-on for the
  Hausarbeit user → 2-3 real runs → ship Stage 2 → repeat.
- Roll to all writing-mode users after 14 days without incidents.

## Telemetry (reuse #74 structure)

New counters:
- `writing_wordcount_drift_total{bucket}` — absolute delta between
  declared and recomputed: `0-5%`, `5-15%`, `15%+`.
- `writing_sources_synth_total{source}` — `llm_emitted`,
  `backend_synthesized` (backend filled gap), `hybrid`.
- `writing_continuation_total{outcome}` — `not_needed`, `single_continuation`,
  `multi_continuation`, `still_truncated_after_retries`.
- `writing_figure_lookup_total{outcome}` — `hit`, `low_confidence`,
  `empty_library`, `no_intent`.

## Effort

| Stage | Estimate |
|---|---|
| 1 | 1 day |
| 2 | 2 days |
| 3 | 2 days |
| 4 (if needed) | 5 days |

Stages 1-3 combined: **~5 engineering days**.

## Relation to current state

Stages 1-3 are net-additive on top of PRs #82/#83:
- #82 already emits refs first — Stage 1 assumes that order.
- #83 killed the destructive auto-continue — Stage 2 replaces it
  with a correct implementation scoped to sections.
- #51/#61 built the structured registry + portfolio — Stages 1-3
  consume that state without mutating it.
