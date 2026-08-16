# Retrieval

Retrieval lets you ask a question over your processed library and get the exact
passages — from the right books, with source locators you can cite. This page
is a practical guide for a researcher: how to query, what comes back, and which
retrieval "arms" you can turn on.

## Ask a question

Search is a single HTTP call to the axiom API:

```bash
curl -X POST http://127.0.0.1:8011/api/search \
  -H 'Content-Type: application/json' \
  -d '{"query": "What does the literature say about criticism of ESG ratings?", "top_n": 5}'
```

`query` is whatever you're looking for, in your own words (works cross-lingually —
an English question finds German books and vice versa). `top_n` is how many
results you want. That's the whole call.

> Reading this locally? Point `POST /api/search` at your axiom API port (8011 by
> default). The response includes which recall arms were used, so you can see
> what happened even before reading the results.

## What comes back

Each result looks like this:

```json
{
  "query": "What does the literature say about criticism of ESG ratings?",
  "top_n": 5,
  "reranked": true,
  "arms": { "dense": true, "bm25": true },
  "took_ms": 402,
  "hits": [
    {
      "chunk_id": "..",
      "text": "The GRI standards have been criticized for …",
      "score": 0.82,
      "source": { "book": "Nachhaltiges Management", "authors": ["…"], "document_id": ".." },
      "locator": { "kind": "page", "label": "S. 221", "chapter": "Transparenz" },
      "section": ["Nachhaltiges Management", "Transparenz"]
    }
  ]
}
```

The parts that matter for a researcher:

- **`hits[].text`** — the passage that answers or supports.
- **`hits[].locator`** — where it lives in the original: a page (`kind: "page"`,
  with a label like `S. 47`) or an EPUB location (`kind: "epub_cfi"`), plus the
  deepest section heading (`chapter`). Open the original and verify — that's the
  point.
- **`hits[].source`** — the book, authors, and document id, so you know exactly
  which source a hit came from.
- **`hits[].score`** — a relevance score; higher is a better match.
- **`arms`** — which recall mechanisms contributed to this result (see below).

## The recall arms (in plain terms)

axiom's retrieval can combine up to five mechanisms. You usually leave them at
the defaults; the table explains what each one does and when it helps:

| Arm | Default | What it does | When it helps |
| --- | --- | --- | --- |
| **dense** | on | Semantic search over chunk embeddings — matches *meaning*, cross-lingually. | Everyday questions; the primary arm. |
| **bm25 (hybrid)** | on | Keyword search over the same chunks — matches exact terms. | Finds passages dense might miss because they're worded exactly. Adds recall. |
| **rerank** | on | A cross-encoder re-scores the hybrid candidates to fine-order them. | Improves ordering quality; costs extra latency (steerable via a remote runner). |
| **sparse** | off | A sparse token-signal arm for rare tokens (norm numbers, acronyms across languages). | Rare-token queries; off by default because it was measured as no quality gain for high latency. |
| **graph** | off | Expands results through the knowledge graph (entities/relationships). | Off by default (measured as slightly negative on the provisional suite); for tuned deployment later. |

`dense` + `bm25` together is the **hybrid** baseline; `rerank` on top re-orders
it. `sparse` and `graph` are opt-in arms behind `AXIOM_SEARCH_SPARSE_ARM` and
`AXIOM_SEARCH_GRAPH_ARM` (see [Configuration](../developer-guide/configuration.md)).

## Why this works

Retrieval runs over the chunks your processing pipeline produced — each with
exact locators and embeddings. The search API composes the enabled arms, merges
their candidates, and returns deduplicated, ranked hits with their sources and
locators. (For the numbers behind arm choices, see the
[Retrieval quality benchmark](../references/benchmarks/retrieval-quality.md).)

## If nothing matches

- Confirm the document finished processing and is indexed (see
  [Ingest](ingest.md)) — only processed documents are searchable.
- Try rephrasing the question; semantic search is forgiving of wording but a
  genuinely different framing still matters.
- If you need a very specific rare term (a norm number, an acronym), consider
  enabling the sparse arm — otherwise the defaults are the best measured
  balance.

Next: [Welcome](../index.md) · [Ingest](ingest.md)
