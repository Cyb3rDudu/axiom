# Concept Tour

A 10-minute read on how a pile of PDFs and EPUBs in Zotero becomes a knowledge
base you can search and cite. No code, no architecture internals — just the
"why does my search work" story.

## The big idea

A library is a collection of *files*. A knowledge base is a collection of
*statements with locations*. Everything axiom does is a pipeline from the first
to the second:

```text
Zotero library → processing pipeline → searchable, citable knowledge
```

For every document you process, axiom runs the file through five stations. Each
station leaves behind one piece of the knowledge base.

## Station 1 — Bring the library in (sync)

axiom connects to your *local* Zotero — the desktop instance with your real
library — and mirrors it. It reads your items, collections, tags, and — most
importantly — the attachment files (PDFs and EPUBs) you want to work with. 

Everything Zotero knows stays Zotero's truth. axiom just copies what it needs so
it can start working, and it picks one *preferred* attachment per document to
process.

**What you get:** a mirror of your library, ready for processing.

## Station 2 — Convert the file (convert)

The heavy lifting starts. Each preferred PDF or EPUB is converted to clean,
structured Markdown — the pages, headings, and tables are pulled out with their
positions intact. This is where a remote GPU runner earns its keep: converting a
large scanned book is compute-heavy.

**What you get:** a Markdown version of each document, with page and section
markers preserved.

## Station 3 — Cut it into chunks (chunk)

Markdown is now split into *chunks* — coherent, self-contained passages of a few
hundred tokens each. Each chunk remembers exactly where it came from: the
physical page, the logical page label, the section heading hierarchy, the
paragraph index. This is the raw material search will operate on.

**What you get:** thousands of small, addressable text passages per library.

## Station 4 — Embed the meaning (embed)

Each chunk is turned into an *embedding* — a numerical vector that captures its
meaning in a high-dimensional space. Chunks about the same concept end up close
together, whether or not they share exact words. This is what lets you search
by *meaning* instead of by keyword.

**What you get:** a semantic index — similar passages are now "near" each other.

## Station 5 — Name the things (extract)

axiom also recognizes *entities* (people, organizations, concepts, locations)
and the *relationships* between them, and links each mention back to the chunk
and page that contains it. This builds a knowledge graph: not just "this text
exists", but "X is related to Y, and here is the evidence".

**What you get:** a web of entities and relationships, each backed by the exact
passage it came from.

## What comes out

Put the five stations' outputs together and you have a citable knowledge base:

- **You can search by meaning.** Ask "what does the literature say about the
  criticism of ESG ratings?" and get the right passages from the right books,
  even across languages, because the search runs on embeddings.
- **Citations are exact.** Every result carries its source locator — physical
  page, logical page label, section — so you can open the original and cite
  confidently. No more "I think it was somewhere in this book".
- **The graph is evidence-backed.** Entity relationships point to the exact
  passage that supports them, ready to be verified or filtered.

## Your first high-level questions

- *Is this like full-text search?* — No. Full-text search matches words; axiom
  matches *meaning*, and it always tells you where the answer lives.
- *Do I have to reorganize my library?* — No. axiom works with your existing
  Zotero structure (collections, tags, items). It mirrors it and processes the
  documents; your library stays the way it is.
- *Does the machine learn from my data?* — Models convert, embed, and extract;
  your documents and extracted knowledge stay in your own stores. The pipeline
  is deterministic about everything except one small layout-recognition step,
  which does not affect retrieval.

Ready to try it? Go to the [Quickstart](quickstart.md). For the internals, jump
to the [Architecture Overview](../developer-guide/architecture.md).
