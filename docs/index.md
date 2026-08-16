# axiom

A research knowledge engine for your Zotero library. axiom turns the documents
you already manage in Zotero into a searchable, citable research knowledge base
that you query through a simple API — by meaning, with exact source locations.

## The problem

Your literature lives in Zotero: PDFs, EPUBs, notes, tags, citations. But a
library of files is not yet a knowledge base. To find the passage that supports
a claim you open document after document, search for a phrase, and hope the
right *page* turns up. To cite a source you need a precise locator; to revisit a
half-remembered idea you need to search across *published work* by meaning, not
by filename.

axiom closes that gap. It integrates with the Zotero you already use and gives
you a **research RAG** — retrieve the right passage in the right book, and cite
it with a locator that points at the exact page.

## How axiom works

```text
your Zotero library  ──API──▶  axiom  ──API──▶  your research clients
```

- **Zotero is your document and source management.** axiom speaks to Zotero
  through its API, mirrors what it needs, and processes the documents — Zotero
  stays the source of truth for your library, and axiom never replaces it.
- **axiom exposes a simple API for your clients.** Query the knowledge base by
  meaning, and drive the RAG pipeline (what gets processed, what is searchable)
  through that API. A researcher talks to axiom; axiom talks to Zotero and to
  compute.
- **Compute runs on a modular runner model.** Heavy work is done by discrete
  runners that can live on different machines: e.g. ranking and retrieval on a
  local runner for low latency, chunking and full processing on a strong remote
  GPU runner. Each runs where it makes sense.

## What you can do with axiom

- **Ask questions that land on the exact book, section, and page.** Retrieval
  is semantic: it understands meaning, cross-lingually, not just filenames.
- **Cite with confidence.** Every result carries its source locator (physical
  page, logical page label, section), so you can open the original and cite it.
- **Keep Zotero exactly as you like it.** axiom works with your existing
  collections, tags, and items; the pipeline mirrors and processes without
  moving or editing your library.

## Where to go next

| Goal | Path |
| --- | --- |
| Understand in 10 minutes how a library becomes a knowledge base | [Concept Tour](get-started/concept-tour.md) |
| Get axiom up and running | [Quickstart](get-started/quickstart.md) |
| Use the core features (sync, ingest, retrieval) | [User Guide](user-guide/ingest.md) |
| Build on or extend axiom (architecture, config, data model) | [Developer Guide](developer-guide/architecture.md) |
| Run a deployment (multi-machine runners, monitoring) | [Operations](operations/deployment.md) |
| Depth: data model, benchmarks, FAQ | [References](references/benchmarks.md) |
| Origin, license, logo | [About](about/index.md) |

## What axiom is NOT

- **Not a Zotero replacement.** Zotero stays the source of truth; axiom sits
  beside it and never competes with it.
- **Not a general-purpose document manager.** It has one job: turn processed
  research documents into a searchable, locator-precise knowledge base.
- **Not files-in-folder search.** It searches *processed knowledge* with real
  source locators and embeddings — meaning, not filenames.

> New here? Start with the [Concept Tour](get-started/concept-tour.md), then do
> the [Quickstart](get-started/quickstart.md).
