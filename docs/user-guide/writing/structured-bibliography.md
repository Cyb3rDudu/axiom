# Structured bibliography

Axiom stores citations as structured data instead of inline Markdown
when the structured-bibliography flag is enabled. The writer manages
the registry automatically; you only need to touch the Bibliography
panel for curation.

## Enable the feature

1. Open **Settings → Writing** and flip **Strukturierte Bibliografie**
   to on.
2. Your next writing session will start with the new flow. Existing
   drafts keep working through the legacy Markdown path until you
   migrate them (see below).

## What changes in the chat flow

When the flag is on, every writer response that contains citations
also emits a structured `content-block:references` block alongside the
prose. The backend:

- parses the block into structured entries (`entry_key`, authors,
  year, title, container_title, publisher, url, pages, accessed_at,
  doi),
- replaces the draft's registry atomically — each revision turn owns
  the whole bibliography, so sources the writer drops are removed, not
  appended next to the kept ones,
- runs the in-text citation validator and surfaces two diagnostics on
  the assistant bubble:
  - **orphan citation**: marker in the body with no matching entry,
  - **dead entry**: registry entry never cited in the body.

The inline `## Literaturverzeichnis` / `## References` section in the
response text stays there for readability, but the structured registry
is the source of truth for downstream rendering (DOCX export, UI
widget).

## The Bibliography panel

Open **Draft → References** to see the structured registry for the
active draft. From here you can:

- inspect each entry's authors / year / title / source,
- delete entries you don't want,
- trigger the Markdown → structured migration for a legacy draft.

A full add / edit form lands in the next iteration; for now new
entries come from the writer's response flow.

## Migrating a legacy draft

1. Open the draft you want to migrate.
2. Go to the References tab.
3. Click **Aus Markdown importieren**. The backend parses the existing
   Literaturverzeichnis and shows a preview with:
   - parsed entries (highlighted by confidence),
   - unparsable lines surfaced verbatim so you can re-enter them.
4. Click **Übernehmen** to persist the parsed entries. The inline
   Markdown section stays in the draft body — the structured entries
   now exist alongside it.

If the parser mangles something you care about, hit **Abbrechen**
instead of committing; the original Markdown is untouched and you can
either fix the source text and retry, or enter the entry manually
(once the form ships).

## Exporting

DOCX export (`Draft → Export → .docx`) automatically replaces any
inline `## Literaturverzeichnis` section with a fresh render from the
structured registry when the flag is on and the draft has structured
entries. Legacy drafts export unchanged.

## Troubleshooting

- **Writer didn't emit a references block.** Check that the flag is
  on in your user settings. The writer only emits structured entries
  when the flag is on; otherwise it falls back to the pure
  inline-Markdown path and the panel stays empty.
- **Migration preview says 'unparsbar' for half my entries.** The
  parser handles APA and numbered bibliographies; mixed or
  non-standard formats may miss. Commit the clean entries and add the
  rest once the add/edit form ships.
- **Orphan/dead-entry badges on the bubble.** These are diagnostics,
  not blockers. Ask the writer for a fix-up pass
  (`Entferne die toten Einträge`) or prune the registry manually in
  the Bibliography panel.

## Literaturportfolio

The KMU Akademie requires a tabular **Literaturportfolio** alongside
the Literaturverzeichnis — a per-source reflection with _Quellenangabe
· Recherchetool · Relevanz · Qualität_. Axiom generates it
automatically from the structured registry for writing-mode drafts.

### How to trigger

Three paths, all equivalent:

1. **Bibliography panel → "Portfolio generieren"** button. Fires the
   agent right away (~30s). Requires the bibliography panel to be
   non-empty.
2. **On session finalize.** When the frontend calls
   `POST /api/writing/sessions/{id}/finalize` (e.g. on session
   unmount or an explicit "Fertigstellen" action), the manager runs
   for the current draft if no portfolio exists yet.
3. **Regenerate.** If the bibliography changed after you generated a
   portfolio, click **Aktualisieren** — nulls the column and runs a
   fresh agent call.

Each draft version gets its own portfolio. When you create a new draft
version, the old one keeps its frozen portfolio and the new version
starts empty.

### Compliance badge

The portfolio header shows a traffic light reflecting KMU thresholds:

- **🟢 grün** — 10–20 Quellen, ≥50 % wissenschaftlich/facheinschlägig
  (Tier A/B), keine Blacklist-Treffer.
- **🟡 gelb** — Quellenanzahl außerhalb des Korridors oder
  Aktualitätswarnung (>10 Jahre alt), aber nicht blacklist-kritisch.
- **🔴 rot** — Blacklist-Treffer (z. B. Wikipedia, Gabler, Boulevard)
  oder wissenschaftlicher Anteil unter 50 %.

Hover the badge for the detailed thresholds.

### Opt-out per session

The portfolio is generated by default. To suppress it for a session:

- **Keyword im Chat-Titel** — z. B. "Hausarbeit ohne Literaturportfolio"
  oder "no portfolio". Beim Session-Create schreibt der Backend
  automatisch `settings.portfolio_enabled = false`.
- **Explicit setting** — PUT the writing session with
  `settings.portfolio_enabled = false` if you want to disable it
  after creation.

With the flag off, the Portfolio button disappears from the
Bibliography panel and the finalize endpoint returns `skipped`.

### Export

DOCX export automatically appends the `## Literaturportfolio` section
after the Literaturverzeichnis when the draft has a persisted
portfolio. Any inline `## Literaturportfolio` section you wrote by
hand is stripped first to avoid duplicates. Order in the exported
DOCX: Body → Literaturverzeichnis → Literaturportfolio.

### Troubleshooting

- **"Portfolio generieren" button is greyed out.** The Bibliography
  panel needs at least one structured entry. Run the writer once with
  the flag on, or use "Aus Markdown importieren" on a legacy draft.
- **Discovery tool says "Web Search" without a provider name.** The
  session's settings didn't carry a `search_provider` field. Set it
  via the writing-settings panel or via
  `writing_settings.search_provider` in user settings.
- **Quality signals missing for some entries.** The entry lacks a
  publisher / journal / URL that the tier classifier recognises.
  Either enrich the entry manually via `PUT
  /api/writing/drafts/{id}/references/{refid}/structured` (UI form
  lands in a follow-up) or accept the "unknown" tier.
- **Compliance stays red even after editing.** The traffic light is
  computed each time the portfolio is generated — hit **Aktualisieren**
  after curating the registry.

## Non-goals

The following are explicitly **out of scope** for this feature:

- BibTeX / Zotero interop
- Automated dedup across references from different sources with
  slightly different spellings
- Cross-draft reference reuse — each draft owns its own registry
- Automated portfolio regeneration on every draft edit — triggered
  explicitly (button) or on session finalize

## Kill switch

Ops can disable the feature globally even for opted-in users by
setting `WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED=false` in the backend
environment. A failed resolution logs the decision inputs so the cause
is visible in server logs.
