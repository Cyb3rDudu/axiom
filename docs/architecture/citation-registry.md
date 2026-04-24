# Citation registry architecture

Design notes for Epic #51's structured bibliography. This document is
the source of truth for the invariants that the writing agent, the
post-response audit, and the UI all rely on.

## Data model

Two tables:

### `draft_references` (extended)

Existing columns (kept as-is for legacy drafts): `id`, `draft_id`,
`document_id`, `web_url`, `citation_text`, `context`, `reference_type`,
`created_at`.

New structured columns (all nullable):

| Column               | Type  | Purpose                                                                 |
| -------------------- | ----- | ----------------------------------------------------------------------- |
| `authors`            | JSONB | `[{family, given}]`. Institutional authors use `{family: 'X', given: ''}`. |
| `year`               | INT   | `NULL` for n.d. / o. J. sources.                                          |
| `title`              | TEXT  |                                                                         |
| `container_title`    | TEXT  | Journal / site / series name.                                            |
| `publisher`          | TEXT  |                                                                         |
| `pages`              | TEXT  | Kept as text ("23–45", "S. 12").                                         |
| `url`                | TEXT  | Canonical landing page (may equal the legacy `web_url`).                 |
| `accessed_at`        | DATE  | Required for web citations under KMU APA 6.                              |
| `doi`                | TEXT  |                                                                         |
| `entry_key`          | TEXT  | Stable per-draft slug. Partial unique index on `(draft_id, entry_key)`.  |
| `source_fingerprint` | TEXT  | Hash for dedup hints (doc-id + pages, or normalised URL).                |

`entry_key` is the authoritative link between in-text markers and
entries. Structured flow callers set it; legacy rows leave it `NULL`
and are filtered out of structured queries.

### `citation_entries`

One row per in-text citation occurrence in the draft body.

| Column              | Type   | Purpose                                       |
| ------------------- | ------ | --------------------------------------------- |
| `id`                | UUID   | PK.                                           |
| `draft_id`          | UUID   | FK → `drafts(id)`.                            |
| `reference_id`      | UUID   | FK → `draft_references(id)`.                  |
| `in_text_marker`    | TEXT   | Literal marker as it appears in the body.     |
| `paragraph_index`   | INT    | For localising diagnostics in the UI.         |
| `char_offset_start` | INT    | Body offset of the marker.                    |
| `char_offset_end`   | INT    |                                               |
| `created_at`        | TIMESTAMPTZ | |

Cascades on draft / reference delete. Populated by the citation-sync
pass (today: pure validator; tomorrow: DB-sync endpoint hit by the
live-editor flow).

## Writer contract

When `structured_bibliography_enabled` resolves to true for the user,
the writer's system prompt instructs the LLM to emit:

````
```content-block:references
[
  {
    "entry_key": "destatis-2024",
    "authors": [{"family": "Destatis", "given": ""}],
    "year": 2024,
    "title": "Außenhandel 2024",
    "url": "https://www.destatis.de/...",
    "reference_type": "web"
  }
]
```
````

Invariants enforced by `services.structured_bibliography.parse_references_block`:

- Must be a JSON array.
- Each entry needs `entry_key` + `title` + at least one of `url`,
  `container_title`, `publisher`, `document_id`.
- Duplicate `entry_key`s within the block are dropped with a warning.
- Invalid entries are skipped; valid ones persist. No all-or-nothing.

The full block is the _entire_ bibliography for the turn —
`replace_draft_registry` deletes entries whose keys are missing, so
the writer must re-emit every kept entry each turn.

Malformed block or missing block on a response with citations → the
inline-Markdown path stays authoritative for that turn; a warning
lands in the WebSocket payload's `structured_references.errors` field.

## Sync contract

`services.citation_sync.validate_citations(body, registry)` runs
inside the writing-chat background task after every response:

1. `strip_references_block(body)` removes the
   `content-block:references` fence so its JSON keys don't get parsed
   as in-text citations.
2. `parse_in_text_citations(body)` extracts APA parenthetical
   (`(Autor, Jahr, S. X)`) and numbered/doc-id bracket markers.
3. Match against the registry:
   - APA → `(family, year)` pair (German umlauts folded ä→ae etc.
     before matching).
   - Numbered with digit payload → 1-based position in registry.
   - Numbered with slug payload → direct `entry_key` match.
4. Emit `CitationSyncReport` with `orphan_markers`, `dead_entries`,
   and `resolved` lists. Year mismatches count as orphans (different
   family+year is a different source).

The report rides on the same WebSocket payload as the audit result so
the UI can badge both on a single bubble.

## Rendering contract

`services.citation_rendering.render_bibliography(entries, profile_id)`
is the one-way projection from structured registry → Markdown:

- **kmu_apa6**: `Autor, V. (Jahr). *Titel*. Verlag.` /
  `Autor, V. (Jahr). *Titel*. Abgerufen am TT.MM.YYYY, von URL`. Sort
  by first-author family, then year.
- **apa7_en**: same structure, English conventions (`Retrieved …, from`,
  `n.d.` instead of `o. J.`).
- **numbered**: `[N] Author, Year. Title. Source.` Preserve input
  order (caller typically sorts by first-appearance via citation
  sync).

Used by:

- DOCX export path in `api.writing.export_draft_as_docx` — when the
  flag is on and the draft has structured entries, the inline
  Literaturverzeichnis in the submitted Markdown is stripped and the
  registry render appended in its place.
- The writer's ingest path — each structured entry gets a rendered
  `citation_text` stored alongside so legacy readers keep working
  even on structured drafts.

## Feature-flag resolution

`services.feature_flags.structured_bibliography_enabled(user_settings)`:

1. If `WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED=false` in the env,
   return `False` unconditionally.
2. Else: resolve
   `user_settings.writing_settings.structured_bibliography_enabled`.
3. Unset env + unset user flag → `False` (default closed).

The env is an opt-out kill switch; setting it to `true` alone does
not enable the flag for users who haven't opted in. This keeps the
rollout predictable: a flip in the env can only _reduce_ the set of
enabled users, not expand it.

## Known limitations

- **No BibTeX / Zotero interop.** The Reference schema is
  intentionally narrow.
- **No cross-draft reference reuse.** Each draft owns its own
  registry; there is no global "library" concept.
- **No automated dedup.** `source_fingerprint` exists to _hint_ at
  duplicates but nothing prunes them automatically — the UI will eventually
  surface near-matches for manual merge.
- **Citation picker lives in a follow-up.** The live-editor flow
  (`(` triggers a dropdown of existing entries) is not wired yet;
  new entries come from writer responses only.
- **Migration is best-effort.** Unparsable lines are surfaced, not
  crashed on. The user is responsible for curating them.

## Portfolio integration (Epic #61)

The writing-mode Literaturportfolio layers on top of the registry
without touching its contract. Full design:
`docs/plans/WRITING_MODE_LITERATURE_PORTFOLIO.md`.

### Data flow

```
draft_references  ─┐
citation_entries  ─┼─▶  writing_portfolio_adapter  ─▶  source_record[]  ─▶  LiteraturePortfolioAgent
draft.content      ─┘                                                         │
                                                                              ▼
                                      WritingPortfolioManager ◀── entries + compliance + markdown_table
                                      persists to drafts.portfolio_output (JSONB)
```

### Invariants

- **Per-draft persistence**: `drafts.portfolio_output` is per-draft
  (not per-session). New draft versions start null so the old
  version's portfolio stays frozen — matching the mission-side
  `missions.literature_portfolio_output` immutability.
- **Agent reuse**: `LiteraturePortfolioAgent.run` is called unchanged.
  No prompt fork, no schema fork.
- **Compliance mirror**: `WritingPortfolioManager._compute_compliance`
  mirrors `LiteraturePortfolioManager._compute_compliance` byte-for-
  byte (same thresholds 10–20 sources, ≥50% wissenschaftlich, same
  blacklist semantics). Keep them in sync if either moves.
- **Section IDs** in writing mode come from the nearest preceding
  Markdown heading, slugified with German-umlaut folding. Context
  snippets are ±180 chars around each `citation_entries` offset.
- **Feature gating**: resolved by
  `structured_bibliography_enabled(user.settings)` AND
  `writing_session.settings.portfolio_enabled != False` AND keyword
  detector absence on the chat title (when the explicit flag is None).

### Export order

`api.writing.export_draft_as_docx` composes:

1. Draft body (with inline Literaturverzeichnis stripped)
2. `## Literaturverzeichnis` (rendered from structured registry via
   `citation_rendering.render_bibliography`)
3. `## Literaturportfolio` (rendered from
   `drafts.portfolio_output.markdown_table`)

Both `_strip_inline_bibliography` and `_strip_inline_portfolio`
run first so a draft that still carries inline markdown for either
section exports cleanly.

## Related modules

- `axiom_backend/services/structured_bibliography.py` — service layer
  and writer-block parser.
- `axiom_backend/services/citation_rendering.py` — registry → Markdown.
- `axiom_backend/services/citation_sync.py` — in-text ↔ registry
  validator.
- `axiom_backend/services/bibliography_migrator.py` — Markdown →
  structured parser.
- `axiom_backend/services/writing_portfolio_adapter.py` — Reference →
  source_record projection (#61/#62).
- `axiom_backend/ai_researcher/agentic_layer/controller/writing_portfolio_manager.py`
  — per-draft portfolio orchestrator (#61/#65).
- `axiom_backend/services/feature_flags.py` — two-layer flag resolver.
- `axiom_backend/database/migrations/add_structured_bibliography.sql` —
  the schema extension.
- `axiom_backend/database/migrations/add_writing_portfolio.sql` —
  drafts.portfolio_output column (#64).
