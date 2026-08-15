# axiom

Turn a personal research library into a searchable, citable knowledge base.

## The problem

You keep your literature in Zotero: PDFs, EPUBs, notes, tags, citations. But a
library of files is not a knowledge base — finding the exact passage that
supports a claim means opening document after document, searching for a phrase,
and hoping the right *page* turns up. When you cite a source you need a precise
locator (page, section); when you revisit a half-remembered idea you need to
search across *published work* by meaning, not just by filename.

**axiom** closes that gap. It takes your Zotero library and builds a
searchable, citable knowledge base from it — so a question finds the right
passage in the right book, and a citation points at the exact page.

## How it works in one line

```text
Zotero library → processing pipeline → searchable, citable knowledge base
```

## What axiom is for

- **For readers and researchers** — connect Zotero, process your documents, and
  ask questions that land on the exact book, section, and page.
- **For developers** — axiom separates *orchestration* (a Go application that
  owns all state, leases, and durability) from *computation* (a Python runner
  that does the heavy ML work). Clear contracts, a documented model, and a
  reproducible pipeline.
- **For operators** — a runner can live on a remote GPU host; the dispatcher
  drives it over a single HTTP contract. Setup is documented as requirements,
  not as one particular machine.

## Where to go next

| Goal | Path |
| --- | --- |
| Understand in 10 minutes how a library becomes a knowledge base | [Concept Tour](get-started/concept-tour.md) |
| Set up and run your first processing job | [Quickstart](get-started/quickstart.md) |
| Use the core features (sync, ingest, retrieval) | [User Guide](user-guide/ingest.md) |
| Understand the architecture and the contract | [Developer Guide](developer-guide/architecture.md) |
| Run and operate a runner | [Operations](operations/deployment.md) |
| Depth: data model, benchmarks, FAQ | [References](references/benchmarks.md) |
| Origin, license, logo | [About](about/index.md) |

## What axiom is NOT

- **Not a Zotero replacement.** Zotero stays the source of truth for your
  library, metadata, and citations. axiom reads from it and mirrors what it
  needs; it never competes with it.
- **Not a general-purpose document manager.** There is no folder UI, no manual
  filing workflow — axiom is built around one job: turn processed documents into
  a searchable, locator-precise knowledge base.
- **Not a files-in-folder search tool.** Searching happens over processed
  chunks with real source locators and embeddings — it understands *meaning*,
  not just filenames.

> New here? Start with the [Concept Tour](get-started/concept-tour.md), then do
> the [Quickstart](get-started/quickstart.md).
