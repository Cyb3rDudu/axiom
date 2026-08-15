# Zotero Sync

This chapter is about connecting axiom to your Zotero library and asking it to
mirror what's there. It's written for people who just want their documents
processed — no code, no database internals.

## What you need

- A **local Zotero desktop instance** with your real library. axiom talks to the
  *local* Zotero API, so Zotero must be running on the same machine. For a local
  library this needs no API key.
- axiom must be told where Zotero's local API is (default `http://localhost:23119/api`)
  and which library to mirror (default `users/0`, your standard local library).

That's it. axiom is not an online service you hand your library to; it works
with the Zotero that's already on your machine.

## Connect

Make sure Zotero is running, then start axiom (see the
[Quickstart](../get-started/quickstart.md)). axiom checks at startup that it can
reach Zotero; if it can't, it warns you immediately rather than failing silently
later.

## Trigger a sync

Ask axiom to mirror your library:

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync
```

Or, from the axiom API surface you'll be using, that's the one "sync" action.
The mirror is also refreshed as part of your normal flow, so you don't chase it
manually every time you add a paper.

## What happens during a sync

In user terms, three things:

1. **It mirrors your library** — items, collections, tags, and metadata are
   copied into axiom's durable store. Zotero remains the single source of truth;
   axiom just keeps a working copy of what it needs.
2. **It picks what to process** — for each document, axiom selects exactly one
   *preferred attachment* to process (PDF preferred over EPUB when both exist).
   Notes and non-attachment items are preserved but not turned into processing
   jobs.
3. **It creates ingest jobs** — every preferred processable attachment becomes a
   *job*. New jobs start `pending` and wait for the processing pipeline; a
   document whose attachment already matches a previous run is not re-processed,
   so a sync doesn't re-do work for no reason.

After the sync you'll have a queue of jobs ready to be processed — that's the
[next chapter: Ingest](ingest.md).

## If something goes wrong (top 3)

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| Startup warns Zotero not reachable | Zotero closed, or the Local API disabled | Start Zotero; enable Settings → Advanced → "Allow other applications on this computer to communicate with Zotero". |
| Sync returns an error about the library | Wrong library id | Confirm `AXIOM_ZOTERO_LIBRARY`; local libraries default to `users/0`. |
| A document got no job | No preferred attachment | In Zotero, attach a PDF/EPUB to the item; axiom prefers PDF. Only items with a processable attachment become jobs. |

> **Is my library changed?** No. axiom only reads from Zotero. It never moves,
> edits, or deletes your items or attachments — your library stays exactly the
> way you keep it.

Next: [Ingest](ingest.md) — follow a job through the pipeline.
