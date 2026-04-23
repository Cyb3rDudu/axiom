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

## Non-goals

The following are explicitly **out of scope** for this feature:

- BibTeX / Zotero interop
- Automated dedup across references from different sources with
  slightly different spellings
- Cross-draft reference reuse — each draft owns its own registry

## Kill switch

Ops can disable the feature globally even for opted-in users by
setting `WRITING_STRUCTURED_BIBLIOGRAPHY_ENABLED=false` in the backend
environment. A failed resolution logs the decision inputs so the cause
is visible in server logs.
