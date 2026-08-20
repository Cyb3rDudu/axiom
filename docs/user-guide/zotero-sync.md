# Zotero Sync

axiom mirrors a local Zotero library, derives one preferred processable
attachment per document, and creates ingest jobs for selected content. Zotero
remains the bibliographic source of truth; PostgreSQL holds the canonical mirror
and processing state.

## Connect Zotero

Zotero desktop must run on the same host as axiom and expose its local API. The
default API base is `http://localhost:23119/api`; the default library is
`users/0`. A local user library needs no read API key.

Start axiom after Zotero. Startup probes the local API and logs the Zotero
server ID. `/api/health` reports the same dependency under `checks.zotero`.

## Trigger a sync

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync
```

A successful response reports canonical item and collection counts, normalized
document projections, newly enqueued jobs, and the committed Zotero library
version. See the [HTTP API reference](../references/api.md#post-apizoterosync)
for the exact request and response contract.

## What commits

One sync performs the following sequence:

1. Read canonical items since the stored Zotero library cursor and read the
   current collection set.
2. Merge the delta with the committed canonical mirror. An older Zotero item
   version cannot replace a newer stored version.
3. Resolve each active attachment path and compute its hash, size, modification
   time, and existence before opening the database apply transaction.
4. Resolve the effective document/collection selection.
5. Commit canonical items, collections, memberships, deletions, normalized
   document/attachment projections, ingest jobs, and the new cursor in one
   transaction.

Notes, annotations, and non-bibliographic parents stay in the canonical mirror
but do not become ingest jobs. A preferred processable attachment is selected
per document; PDF is preferred over EPUB when both are active. A metadata-only
change with an unchanged attachment hash does not enqueue another processing
job.

After a successful commit, axiom schedules exact-form entity consolidation. A
10-second debounce collapses consecutive successful syncs into one run. The
consolidation runs independently of the completed HTTP request and logs its
before/after counts.

## Choose what enters ingest

Selection changes job creation, not the Zotero mirror. Excluding a document does
not delete its canonical rows or already persisted search chunks.

Read persisted choices:

```bash
curl http://127.0.0.1:8011/api/zotero/selection
```

Write document and collection choices atomically:

```bash
curl -X PUT http://127.0.0.1:8011/api/zotero/selection \
  -H 'Content-Type: application/json' \
  -d '{
    "selection": [
      {"document_id": "<document-uuid>", "mode": "excluded"}
    ],
    "collections": [
      {"collection_key": "AB12CD34", "mode": "included"}
    ]
  }'
```

Modes are `included`, `excluded`, and `default`. `default` removes the stored
choice. Collection keys are Zotero's eight-character uppercase alphanumeric
keys.

The collection layer defines the base set:

- With no collection choices, absent document choices mean selected.
- One or more included collections restrict the base to their documents.
- With excluded collections only, the base contains every document outside
  those collections.
- A document exclusion always removes from the base.
- A document inclusion records an explicit choice but never overrides a
  collection exclusion or adds a document outside an included-collection base.

Inspect the expanded result before syncing:

```bash
curl http://127.0.0.1:8011/api/zotero/selection/resolved
```

The response lists each selected collection and its document IDs, persisted
document modes, and the effective number of suppressed documents.

## One-run overrides

A sync body can exclude or include document UUIDs for that call without changing
persisted choices:

```bash
curl -X POST http://127.0.0.1:8011/api/zotero/sync \
  -H 'Content-Type: application/json' \
  -d '{
    "include": ["<document-uuid>"],
    "exclude": ["<other-document-uuid>"]
  }'
```

An exclusion always applies. When collection choices exist, an inclusion cannot
resurrect a document outside the collection base.

## Inspect document state

```bash
curl 'http://127.0.0.1:8011/api/zotero/documents?sync_state=pending'
```

`sync_state` accepts `synced`, `held`, `processing`, or `pending`:

- `synced`: the preferred attachment has a completed ingest job.
- `held`: the document is excluded or has no job.
- `processing`: the latest job is claimed or processing.
- `pending`: the latest job is waiting for a dispatcher.

Each row includes the document's Zotero key and title, current job status, and
preferred attachment metadata and content hash.

## Zotero client semantics (external)

This section describes Zotero client behavior, not an axiom write path.

Zotero uses the citation dialog's page text field as the citation locator. The
verified rule is `locator == pageLabel`: the page entered in that text field is
the page emitted in the citation locator. axiom's trusted print-page label is
the reference against which that value is checked. For a passage spanning more
than one page, `/api/passage/{id}/page?at=N` resolves the relevant character
position to the exact label before the value is used in Zotero.

This behavior was empirically verified on 2026-08-20. axiom does not expose an
automated Zotero note-writing route on the documented product surface. The
separate work item is [Issue #189](https://github.com/Cyb3rDudu/axiom/issues/189).

## Read and write boundaries

Normal sync and ingest read Zotero and never move, edit, or delete source items.
The conditionally enabled repair API is a separate operational write gateway.
It requires a local Zotero write key, quarantines the original attachment before
mutation, and records each write in `zotero_write_audit`.

| Symptom | Check |
| --- | --- |
| Startup reports Zotero unreachable | Start Zotero and enable local API access under Zotero's advanced settings. |
| Sync reports a library error | Verify `AXIOM_ZOTERO_LIBRARY`; a local user library defaults to `users/0`. |
| A document has no job | Inspect `/api/zotero/documents`; confirm it is selected and has a preferred PDF or EPUB attachment. |
| Sync succeeds but enqueues zero jobs | Compare attachment hashes; unchanged content is intentionally not reprocessed. |

Next: [Ingest](ingest.md)
