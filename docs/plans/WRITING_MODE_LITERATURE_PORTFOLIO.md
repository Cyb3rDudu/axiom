# Writing-mode Literaturportfolio — Design spec

**Status:** proposed
**Owner:** writing + portfolio teams
**Related:** Epic #51 (structured bibliography), existing LiteraturePortfolioAgent (mission-side, shipped April 2026)

## 1. Context

Axiom today produces the KMU-compliant **Literaturportfolio** (the
tabular _Quellenangabe | Recherchetool | Relevanz | Qualität_
reflection) only for **research missions** — the agent is triggered
from `report_generator.process_citations` once a mission finishes, and
reads its source list from the mission's citation graph + note
metadata.

Writing-mode sessions (Epic #51 just landed) now carry their own
structured registry in `draft_references` and a clean
in-text-citation sync in `citation_entries`, but the portfolio agent
has no hook into them. A writing-mode Hausarbeit finishes with a
perfect Literaturverzeichnis and **no portfolio**, which is the
KMU-blocking artifact.

The fix is small and mostly plumbing: reuse the existing agent, map
`draft_references` rows into the shape the agent already expects, and
persist the output per draft.

## 2. Goal

1. Produce a KMU-compliant `PortfolioOutput` for every writing-mode
   draft whose session is flagged "academic" (same keyword detector as
   missions — `portfolio_optout.py`) and whose structured bibliography
   feature flag is on.
2. Reuse — not fork — the existing
   `LiteraturePortfolioAgent`, `assign_scientific_tier`,
   `discovery_tool_label`, `compute_quality_signals`, and compliance
   thresholds. A rewrite is out of scope.
3. Surface the portfolio in the Draft panel (next to the Bibliography
   widget) and in the DOCX export (alongside the Literaturverzeichnis
   section).

## 3. Non-goals

- No change to the mission-side path. The mission agent still runs
  end-of-research exactly as it does today.
- No new citation-style profiles, no Scimago integration, no
  Word/DOCX template beyond what exists.
- No automatic re-run on every draft edit. Triggered explicitly (user
  clicks "Portfolio generieren") or implicitly once per writing-mode
  session close, same as missions.
- No cross-draft reuse. Each draft owns its own portfolio, same
  invariant as its bibliography registry.

## 4. Design

### 4.1 Entry point

New manager class `WritingPortfolioManager` mirroring
`literature_portfolio_manager.LiteraturePortfolioManager` but scoped
to a draft:

```python
class WritingPortfolioManager:
    async def run_if_enabled(
        self,
        draft_id: str,
        user: models.User,
        trigger: str,  # "manual" | "session_close"
    ) -> Optional[PortfolioOutput]: ...
```

Trigger points:

- **Manual button** in Bibliography widget: `POST
  /api/writing/drafts/{draft_id}/portfolio/generate` →
  `run_if_enabled(trigger="manual")`.
- **Session close hook** (optional, stretch): when the user closes a
  writing-session chat, fire the manager with `trigger="session_close"`
  only if the portfolio opt-out keyword is absent from the session
  title/description.

Both paths are gated on `structured_bibliography_enabled` **and** the
portfolio opt-out absence check (reuse `portfolio_optout.is_enabled`).

### 4.2 Source aggregation

The agent's contract is `List[source_record: dict]` with the following
keys (from
`literature_portfolio_manager._build_source_records`):

| Key                     | Mission source                                  | Writing-mode source                                      |
| ----------------------- | ----------------------------------------------- | -------------------------------------------------------- |
| `source_id`             | `Note.source_id`                                | `Reference.entry_key`                                    |
| `apa_citation`          | `source_quality.extract_apa_citation(note)`     | Call `citation_rendering.render_entry(entry, "kmu_apa6")` |
| `discovery_tool`        | `source_quality.discovery_tool_label(note, provider)` | Same helper, new input shape (see §4.3)                  |
| `quality_signals`       | `compute_quality_signals(note)`                 | Same helper, adapter maps Reference → the shape it expects |
| `scientific_tier`       | `assign_scientific_tier(quality_signals)`       | Unchanged                                                 |
| `sections_used_in`      | Section IDs from citation graph                  | Section/paragraph indices from `citation_entries` rows   |
| `section_context_snippets` | ±180-char window around citation placeholders | ±180-char window around `char_offset_start/end` in draft body |

The mapping is an adapter function in a new module,
`services.writing_portfolio_adapter`:

```python
def reference_to_source_record(
    ref: models.Reference,
    citation_entries: list[models.CitationEntry],
    draft_body: str,
    mission_settings_like: dict,
) -> dict: ...
```

### 4.3 `discovery_tool` for writing mode

`source_quality.discovery_tool_label` currently needs a note-like
object + the mission's `search_provider`. For writing mode we have:

- **Local-doc refs** (`reference_type == "document"`, `document_id`
  populated): map to `"Axiom Local Library (RAG)"` (same label as
  missions).
- **Web refs** (`reference_type == "web"`, `url` populated): recognize
  `scholar.google.com`, `crossref.org`, `arxiv.org`, `ssrn.com`,
  `springer.com`, `sciencedirect.com` via the existing
  `discovery_tool_label` domain heuristics.
- **Fallback** (generic web): `"Web Search ({provider})"` where
  `provider` comes from the writing session's effective search
  provider (user settings → `writing_settings.search_provider` →
  "tavily" default). If the user disabled web search for the session,
  label as `"Direct URL entry"`.

Adapter synthesizes a note-like dict so `discovery_tool_label` stays a
shared helper — no rewrite.

### 4.4 `sections_used_in` + context snippets

Epic #51's `citation_entries` table already stores one row per in-text
citation occurrence with `paragraph_index` and `char_offset_start/end`.

Two additions needed:

1. **Persist `citation_entries` today** — `citation_sync` currently
   returns a validator report but doesn't write rows. Add a
   `record_citation_occurrences` call at the end of the writing-chat
   background task so the table reflects the latest body.
2. **Derive section IDs from paragraph indices** — writing mode has
   no numbered section tree like missions. Use heading path as the
   section ID instead: for each `CitationEntry`, walk backwards from
   `char_offset_start` to the nearest `#`/`##`/`###` line and emit its
   slugified title as the section ID.

Context snippets come free: `draft_body[offset_start-180:offset_end+180]`.

### 4.5 Quality signal enrichment

Writing-mode refs don't carry `Note.source_metadata` — but
`draft_references` has `authors`, `year`, `title`, `container_title`,
`publisher`, `url`, `doi` populated by the writer. Enough to drive
`compute_quality_signals` after a thin adapter:

```python
def _fake_note(ref: Reference) -> types.SimpleNamespace:
    return types.SimpleNamespace(
        source_metadata={
            "authors": ref.authors,
            "year": ref.year,
            "title": ref.title,
            "journal": ref.container_title,
            "publisher": ref.publisher,
            "doi": ref.doi,
            "url": ref.url,
        },
        source_type="document" if ref.document_id else "web",
        source_id=ref.entry_key,
    )
```

`publisher_tiers.classify_tier` already accepts `(publisher, journal,
url, domain, doi, filename)` separately, so the adapter maps cleanly.

### 4.6 Persistence

Add one JSONB column via the same migration pattern:

```sql
ALTER TABLE drafts
    ADD COLUMN IF NOT EXISTS portfolio_output JSONB;

CREATE INDEX IF NOT EXISTS idx_drafts_portfolio_traffic_light
    ON drafts ((portfolio_output->'compliance'->>'traffic_light'))
    WHERE portfolio_output IS NOT NULL;
```

- **Why per-draft, not per-session:** drafts are versioned (each
  revision creates a new row), and the portfolio reflects the
  bibliography state at the moment of generation. A per-session column
  would either lie about which revision was analyzed or force us to
  version it manually.
- **Writer-side cache invalidation:** nuke the column on the old
  draft every time a new version is minted via
  `POST /api/writing/drafts/.../versions`. The UI shows "regenerate
  portfolio" when the column is null on the current draft.

### 4.7 LLM call

Reuse `LiteraturePortfolioAgent` unchanged:

- Mission's `comprehensive_settings.language_code` → map from
  `user.settings.preferred_language` (default `de`).
- Mission's `mission_goal` → pass the draft's title as the goal hint,
  or derive from the chat title (`"Writing: {title}"`).
- Batching (≤20 sources/call) already handled.
- Cost: still one Sonnet call per generation (≤ 20 sources); ~2–5k
  input tokens, ~2–3k output.

## 5. UI surface

### Bibliography widget (`BibliographyWidget.tsx`)

- New "Portfolio" button next to "Aus Markdown importieren". Disabled
  when:
  - flag off,
  - structured registry empty,
  - portfolio already up-to-date for the current draft (column
    populated and draft unchanged since generation).
- Click → loading spinner → renders `PortfolioOutput.markdown_table`
  inline + compliance badge (green/yellow/red).

### DOCX export

Extend `_maybe_append_structured_bibliography` to also append the
portfolio table when the column is populated. Order in export:

1. Draft body (cleaned of inline Literaturverzeichnis)
2. `## Literaturverzeichnis` (from structured registry render)
3. `## Literaturportfolio` (from `portfolio_output.markdown_table`)

## 6. Open questions

- **Relevance bullets for revision-only writing sessions.** The
  mission agent grounds relevance in research notes; writing mode
  lacks that context. Mitigation: pass the **section context snippets**
  (±180 chars around each cite) to the agent instead — reflects _how
  the source is used_ rather than _why it was collected_. Same LLM
  input size order of magnitude.

- **Portfolio opt-out wording.** Missions match on the user request
  text at mission creation. Writing sessions have no single "request";
  best signal is the session's chat title or an explicit per-session
  setting. Proposal: new
  `writing_session.settings.portfolio_enabled` boolean (default
  `true`), plus the existing keyword detector on the session's chat
  title for parity.

- **Recherchetool fidelity for hand-added refs.** If the user added a
  reference manually via the Bibliography panel (follow-up to #56),
  we don't know which tool they used. Emit `"Manual entry"` and let
  them override in the portfolio table at render time — stretch goal.

## 7. Effort

Rough breakdown (sequential):

- **Adapter + discovery_tool mapping** — ~0.5 day. Pure functions +
  unit tests.
- **Persist `citation_entries` from the chat-task path** — ~0.5 day.
  Already parsed by the sync validator; just write-back.
- **Draft column migration + manager class** — ~1 day. Mirrors the
  existing mission manager.
- **UI button + rendering** — ~1 day.
- **DOCX export integration** — ~0.5 day.
- **End-to-end test with a recent writing session** — ~0.5 day.

**Total: ~4 engineering days.** Most of the mass is glue; no new LLM
prompt, no new data model beyond the single JSONB column.

## 8. Sub-issue breakdown (proposed)

Intended as the GitHub sub-issues under a new Epic ("Writing-mode
Literaturportfolio"):

1. `services.writing_portfolio_adapter` — Reference → source_record mapping + tests
2. Persist `citation_entries` in the chat-task path (closes the gap
   between Epic #51's validator and the DB rows it described)
3. Migration: `drafts.portfolio_output` JSONB + traffic-light index
4. `WritingPortfolioManager` + `POST /drafts/{id}/portfolio/generate`
5. Bibliography widget: Portfolio button + inline rendering
6. DOCX export: append `## Literaturportfolio` section when column is
   populated
7. Session-close auto-trigger + `writing_session.settings.portfolio_enabled`
8. Documentation: user-guide entry + extension of the architecture
   citation-registry doc

## 9. Relation to Epic #51

Strictly additive. Nothing in this plan changes the structured
bibliography contract; we just layer reflection on top of the
registry. If #51 hadn't landed, this spec would be ~2 eng-weeks
instead of ~4 days — we're banking on:

- `draft_references` already carrying authors/year/title/publisher/url,
- `citation_entries` already carrying offset + paragraph metadata,
- `feature_flags.structured_bibliography_enabled` already providing
  the gate.

Once this is shipped, a single writing-mode Hausarbeit produces both
the Literaturverzeichnis **and** the KMU-obligatory Literaturportfolio
without leaving the chat panel.
