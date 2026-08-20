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
      "source": { "title": "Nachhaltiges Management", "authors": ["…"], "doc_id": ".." },
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
  Reports the `dense`/`bm25`/`sparse` flags; the graph arm contributes
  candidates but is not echoed.

## The recall arms (in plain terms)

axiom's retrieval can combine up to five mechanisms. You usually leave them at
the defaults; the table explains what each one does and when it helps:

| Arm | Default | What it does | When it helps |
| --- | --- | --- | --- |
| **dense** | on | Semantic search over chunk embeddings — matches *meaning*, cross-lingually. | Everyday questions; the primary arm. |
| **bm25 (hybrid)** | on | Keyword search over the same chunks — matches exact terms. | Finds passages dense might miss because they're worded exactly. Adds recall. |
| **rerank** | on | A cross-encoder re-scores the hybrid candidates to fine-order them. | Marginally improves ordering quality (measured marginal but consistent); costs extra latency (steerable via a remote runner). |
| **sparse** | off | A sparse token-signal arm for rare tokens. | Available for rare-token queries; disabled because the measured suite showed no quality gain for its latency cost. |
| **graph** | off | Expands results through the knowledge graph (entities/relationships). | Available for explicitly graph-assisted deployments; disabled because the measured suite showed a slight quality loss and high latency. |

`dense` + `bm25` together is the **hybrid** baseline; `rerank` on top re-orders
it. `sparse` and `graph` are opt-in arms behind `AXIOM_SEARCH_SPARSE_ARM` and
`AXIOM_SEARCH_GRAPH_ARM` (see [Configuration](../developer-guide/configuration.md)).

## Expand a hit into a passage

Use the returned `chunk_id` to retrieve the full citation context:

```bash
curl http://127.0.0.1:8011/api/passage/<chunk-id>
```

The passage response contains the chunk, its source and locator, and the
adjacent chunks from the same attachment. It also exposes `paragraph_pages` on
current generations. This is a sequence of `[character offset, page label]`
boundaries, for example:

```json
{"paragraph_pages":[["0","95"],["775","96"]]}
```

Resolve a character position inside a multi-page passage to one exact print
page:

```bash
curl 'http://127.0.0.1:8011/api/passage/<chunk-id>/page?at=1000'
```

```json
{"at":1000,"chunk_id":"<chunk-id>","page":"96"}
```

A pre-map generation returns the honest page span instead of inventing an exact
page.

## Read the knowledge graph

The graph API exposes entities, one-hop neighbors, and evidence-backed
relations:

```bash
curl 'http://127.0.0.1:8011/api/kg/entities?q=doppelte%20Wesentlichkeit'
curl 'http://127.0.0.1:8011/api/kg/entities/<entity-id>/neighbors?limit=20'
curl 'http://127.0.0.1:8011/api/kg/relations?document_id=<document-uuid>'
```

Entity search ranks four match classes in order: exact form,
normalized-equivalent form, bilingual-family equivalent, then substring or
compound decomposition. This prevents a popular fragment from outranking an
exact but less frequent form.

Relations expose two read-quality signals:

- `confidence` combines distinct-document support (60%), repeated extraction
  evidence (30%), and body-section evidence quality (10%).
- `corroborating_documents` counts distinct library documents supporting the
  same canonical source/type/target triple. The compatibility field `documents`
  has the same value.

A practical relation filter is `confidence >= 0.65` and
`corroborating_documents >= 2`. Evidence chunk IDs resolve through the response's
`sources` map and the passage API. The full normalization chain, parameters,
conditional response envelopes, and error behavior are in the
[HTTP API reference](../references/api.md#knowledge-graph).

## Why this works

Retrieval runs over active-snapshot chunks with locators and embeddings. The
search API composes the enabled arms, merges their candidates, and returns
deduplicated, ranked hits with their sources and locators. The graph read path
aggregates only active evidence and computes confidence without changing the
persisted extractor strength. For the measured arm choices, see the
[Retrieval quality benchmark](../references/benchmarks/retrieval-quality.md).

## If nothing matches

- Confirm the document finished processing and is indexed (see
  [Ingest](ingest.md)) — only processed documents are searchable.
- Try rephrasing the question; semantic search is forgiving of wording but a
  genuinely different framing still matters.
- If you need a very specific rare term (a norm number, an acronym), consider
  enabling the sparse arm — otherwise the defaults are the best measured
  balance.

Exact request and response fields: [HTTP API](../references/api.md)

Next: [Welcome](../index.md) · [Ingest](ingest.md)
