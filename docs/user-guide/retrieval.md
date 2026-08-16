# Retrieval

!!! note "Status"
    The retrieval feature (semantic search + the ability to ask questions over
    your indexed library) is still in development. This page describes
    the intended shape; the concrete search API and UI arrive when the
    feature ships.

## What retrieval will do

Once a library is processed (see [Ingest](ingest.md)), it's searchable. The
retrieval surface is meant to let you:

- **Search by meaning.** Ask a question or describe what you're looking for, and
  get the right passages from the right books — not just keyword matches.
- **See where the answer lives.** Every result carries its source locator
  (physical page, logical page label, section), so you can open the original and
  cite it confidently.
- **Use the knowledge graph.** Entity and relationship results are evidence-
  backed — each relation points at the exact passage that supports it.

The processing pipeline already produces everything retrieval needs: chunks with
exact locators, dense embeddings for semantic search, and an entity/relationship
graph. What's still coming is the polished query + result surface on top.

## Current state

- The pipeline reliably produces processed, locator-precise, indexed chunks.
  This was demonstrated end-to-end in the [measurement reports](../references/benchmarks.md).
- The full retrieval *API/UI* for end users is not released yet.

## What you can do today

Until the retrieval surface ships, you can:

- Confirm your documents finished processing and are indexed (see
  [Ingest](ingest.md)).
- Watch this page for the retrieval release.

Next: [Welcome](../index.md) · [Ingest](ingest.md)
