# HTTP API

The Go service exposes a JSON API under `/api`. The default base URL is
`http://127.0.0.1:8011`. There is no application-level authentication; the
default loopback bind is the access boundary for administrative and write
routes. Set `AXIOM_BIND_ADDR` deliberately before exposing the service beyond
the local host.

Routes whose backing service is unavailable either return `503`, remain
unregistered, or fail closed with `404`, as noted below. JSON responses use
`Content-Type: application/json`. Errors normally have the form
`{"error":"message"}`; source delivery deliberately returns a plain `404` for
every rejected request.

## Route index

| Method | Path | Purpose | Availability |
| --- | --- | --- | --- |
| `GET` | `/api/health` | Dependency readiness | Always registered |
| `POST` | `/api/zotero/sync` | Mirror Zotero and enqueue changed attachments | Requires PostgreSQL wiring |
| `GET` | `/api/ingest/jobs` | List recent ingest jobs | Requires PostgreSQL wiring |
| `GET` | `/api/zotero/selection` | Read persisted document and collection choices | Requires PostgreSQL wiring |
| `GET` | `/api/zotero/selection/resolved` | Expand collection choices to document IDs | Requires PostgreSQL wiring |
| `PUT` | `/api/zotero/selection` | Write document and collection choices atomically | Requires PostgreSQL wiring |
| `GET` | `/api/zotero/documents` | List Zotero documents by effective ingest state | Requires PostgreSQL wiring |
| `POST` | `/api/search` | Search processed passages | Requires PostgreSQL wiring and a usable OpenSearch recall arm; query-runner failure degrades the available arms |
| `GET` | `/api/passage/{id}` | Read one active passage with source and neighbors | Requires search wiring |
| `GET` | `/api/passage/{id}/page` | Resolve a character offset to an exact page label | Requires search wiring and a paragraph page map |
| `GET` | `/api/kg/entities` | Search or browse graph entities | Requires PostgreSQL wiring |
| `GET` | `/api/kg/entities/{id}/neighbors` | Read one-hop graph edges | Requires PostgreSQL wiring |
| `GET` | `/api/kg/relations` | Browse and filter graph relations | Requires PostgreSQL wiring |
| `POST` | `/api/kg/consolidate` | Merge duplicate active entities by exact canonical form | Registered only when the consolidation service is wired |
| `GET` | `/api/processor/source/{jobID}` | Stream a leased job's source to a runner | Fails closed unless source secret and repository are wired |
| `GET` | `/api/repair/queue` | List repair work with source metadata | Registered only when the repair API is wired |
| `GET` | `/api/repair/cases` | List the 100 most recently updated repair cases | Registered only when the repair API is wired |
| `POST` | `/api/repair/cases/{id}/claim` | Claim a repair case | Registered only when the repair API is wired |
| `POST` | `/api/repair/cases/{id}/verdict` | Submit and optionally apply a repair verdict | Registered only when the repair API is wired |
| `GET` | `/api/repair/docs/{documentKey}/locator-stats` | Inspect active locator trust levels | Registered only when the repair API is wired |

## Health and jobs

### `GET /api/health`

The response is always HTTP `200`. `ok` is false when any registered checker
fails; `checks` contains `ok`, an error string, or `unknown` for each dependency.
A fully wired process checks Zotero, PostgreSQL, the query runner, and the ingest
runner. `build` carries the version banner and must match `axiom-ng --version`.

Live response shape:

```json
{"ok":true,"build":"axiom-ng v0.1.10-…-gc043bab (commit c043bab, release build)","checks":{"ingest-runner":"ok","postgres":"ok","query-runner":"ok","zotero":"ok"}}
```

### `GET /api/ingest/jobs`

| Query parameter | Default | Accepted values |
| --- | --- | --- |
| `limit` | `50` | Integer `1`–`500`; malformed or out-of-range values fall back to `50` |

The response is `{"jobs":[...]}`. Job fields currently retain their Go names:
`ID`, `SourceID`, `DocumentID`, `AttachmentID`, `Status`, `ContentHash`,
`Attempt`, `MaxAttempts`, `ErrorCode`, `ErrorMessage`, `ResolvedAt`, and
`EnqueuedAt`. Nullable database values appear as `null`.

```json
{
  "jobs": [
    {
      "ID": "e2499641-9325-4ae6-8041-945013e40f51",
      "SourceID": "87794fd7-676b-46d9-ae76-e26c8cba5589",
      "DocumentID": "a53eafed-7465-4ced-9685-373cb677ec28",
      "AttachmentID": "d08f95ec-7618-45d6-ad04-b68254c6ad13",
      "Status": "pending",
      "ContentHash": "d6973e45530ff92ac0de098541d958495c3e7de58af540697d8fc476662d0a73",
      "Attempt": 0,
      "MaxAttempts": 3,
      "ErrorCode": null,
      "ErrorMessage": null,
      "ResolvedAt": null,
      "EnqueuedAt": "2026-08-20 14:30:19.541459+00"
    }
  ]
}
```

## Zotero sync and selection

### `POST /api/zotero/sync`

An absent or empty body runs a sync with the persisted selection. An optional
body applies document overrides to this run only:

```json
{
  "include": ["<document-uuid>"],
  "exclude": ["<document-uuid>"]
}
```

Both arrays contain document UUIDs. The body limit is 4 MiB. An override does
not modify the persisted selection. When collection choices exist, `include`
cannot add a document outside the collection base; `exclude` always removes a
document from the effective set.

A successful response summarizes the committed canonical sync:

```json
{
  "source_id": "<source-uuid>",
  "canonical_items": 42,
  "canonical_collections": 7,
  "document_projections": 18,
  "enqueued_jobs": 3,
  "library_version": 912
}
```

The sync reads canonical items since the stored Zotero library cursor, mirrors
items and collections, updates normalized projections, hashes active attachment
files before opening the apply transaction, and enqueues selected attachments
only when their content requires work. A successful commit schedules entity
consolidation after the sync response; see [KG consolidation](#post-apikgconsolidate).

### `GET /api/zotero/selection`

Returns persisted modes as maps. No row means the default selection behavior.

```json
{"collections":{},"selection":{}}
```

### `GET /api/zotero/selection/resolved`

Returns every persisted collection choice expanded to direct document IDs, the
persisted document modes, and the effective number of suppressed documents.

```json
{"collections":[],"documents":{},"suppressed_documents":0}
```

### `PUT /api/zotero/selection`

Document and collection updates are committed in one transaction:

```json
{
  "selection": [
    {"document_id": "<document-uuid>", "mode": "excluded"}
  ],
  "collections": [
    {"collection_key": "AB12CD34", "mode": "included"}
  ]
}
```

`mode` is `included`, `excluded`, or `default`; an empty mode also means
`default`. `default` removes the persisted row. Document IDs must be UUIDs.
Collection keys must contain exactly eight uppercase ASCII letters or digits.
Unknown document IDs return `422`. Success returns:

```json
{"status":"ok"}
```

The effective cascade is:

1. With no collection rows, document rows are the complete gate and absent rows
   are selected.
2. Any included collection makes the base exactly the documents in included
   collections. With excluded collections only, the base is every document
   outside those collections.
3. A document exclusion removes from the base. A document inclusion records an
   explicit choice but does not resurrect a document excluded by the collection
   base.
4. A one-run sync exclusion always applies. A one-run inclusion cannot bypass an
   active collection gate.

### `GET /api/zotero/documents`

| Query parameter | Default | Accepted values |
| --- | --- | --- |
| `sync_state` | all | `synced`, `held`, `processing`, `pending` |

The response is `{"documents":[...]}`. Each row contains `document_id`,
`zotero_key`, `title`, `item_type`, `sync_state`, optional `job_status`, optional
preferred `attachment`, and `updated_at`. `held` means the document is excluded
or has no job; `synced` means the preferred attachment has a completed job.

## Search and passages

### `POST /api/search`

```json
{
  "query": "doppelte Wesentlichkeit",
  "top_n": 5,
  "filters": {
    "document_ids": ["<document-uuid>"]
  }
}
```

`query` must contain non-whitespace text. `top_n` defaults to `10` and cannot
exceed `64`. `filters` is optional; `document_ids` is the supported filter.
The response contains the effective query and count, whether cross-encoder
reranking completed, the successful recall arms, ranked hits, and total elapsed
milliseconds:

```json
{
  "query": "doppelte Wesentlichkeit",
  "top_n": 1,
  "reranked": true,
  "arms": {"dense": true, "bm25": true},
  "hits": [
    {
      "chunk_id": "3cfe6d2e-6d38-47c0-9f9c-e535fa69f38c",
      "text": "gebrauchen konnten und auch in dieser Phase sehr nützlich sind. …",
      "score": 0.04032705466300992,
      "source": {
        "doc_id": "ad61d257-5899-4b3f-a494-fa0380bbd046",
        "title": "Führungsaufgabe Change",
        "authors": ["Ulrich Grannemann", "Hagen Seele"],
        "year": 2016,
        "publisher": "Springer Fachmedien Wiesbaden",
        "language": "de",
        "tags": ["neutral", "secondary source"]
      },
      "locator": {
        "kind": "page",
        "label": "**Die doppelte Belastung: Die Mitarbeiter wollen Sicherheit und die Veränderung bringt den Boden zum Wanken** · S. 95-96",
        "chapter": "**Die doppelte Belastung: Die Mitarbeiter wollen Sicherheit und die Veränderung bringt den Boden zum Wanken**",
        "page_source": "folio_verified"
      },
      "section": [
        "Führungsaufgabe Change",
        "**Die doppelte Belastung: Die Mitarbeiter wollen Sicherheit und die Veränderung bringt den Boden zum Wanken**"
      ]
    }
  ],
  "took_ms": 23597
}
```

The example is a live response with the long passage text abbreviated. A hit may
also contain `collapsed_near_duplicates`. Locator fields are:

| Field | Meaning |
| --- | --- |
| `kind` | `page` or `epub_cfi` |
| `label` | Human-readable page, page range, or EPUB label |
| `chapter` | Deepest section heading, when known |
| `cfi` | EPUB CFI, when applicable |
| `chapter_number` | Chapter ordinal for chapter-relative pagination |
| `page_source` | `folio_verified`, `pdf_label_sane`, `physical_only`, or `none` |

A single failed recall arm degrades the result and is reported as false or
absent in `arms`; total recall failure returns `503`.

### `GET /api/passage/{id}`

`id` is a chunk UUID. The response returns the active chunk, document/snapshot/
attachment IDs, chunk index, text, section path, locator, bibliographic source,
and adjacent chunks at indexes −1 and +1 within the same attachment. New
generations also include the raw `paragraph_pages` map. Selected fields from a
live response are shown below; long text and neighbors are omitted:

```json
{
  "chunk_id": "3cfe6d2e-6d38-47c0-9f9c-e535fa69f38c",
  "document_id": "ad61d257-5899-4b3f-a494-fa0380bbd046",
  "snapshot_id": "e9521b56-439f-43b8-88f6-c9afdd74e00a",
  "attachment_id": "3cb2d4b5-52f2-4e78-9146-74d46aca15b3",
  "chunk_index": 160,
  "text": "…",
  "section": ["Führungsaufgabe Change", "**Die doppelte Belastung: …**"],
  "locator": {
    "kind": "page",
    "label": "**Die doppelte Belastung: …** · S. 95-96",
    "chapter": "**Die doppelte Belastung: …**",
    "page_source": "folio_verified"
  },
  "source": {
    "doc_id": "ad61d257-5899-4b3f-a494-fa0380bbd046",
    "title": "Führungsaufgabe Change",
    "authors": ["Ulrich Grannemann", "Hagen Seele"],
    "year": 2016,
    "publisher": "Springer Fachmedien Wiesbaden",
    "language": "de",
    "tags": ["neutral", "secondary source"]
  },
  "paragraph_pages": [["0", "95"], ["775", "96"]]
}
```

An unknown chunk returns `404`. A chunk from an inactive snapshot also returns
`404`, with its snapshot and attachment IDs plus a hint to search for the
current generation.

### `GET /api/passage/{id}/page?at=N`

`at` is a required, non-negative character offset into the chunk text. The
server selects the final `paragraph_pages` boundary whose offset is less than or
equal to `at`.

Live responses for the passage above:

```json
{"at":0,"chunk_id":"3cfe6d2e-6d38-47c0-9f9c-e535fa69f38c","page":"95"}
```

```json
{"at":1000,"chunk_id":"3cfe6d2e-6d38-47c0-9f9c-e535fa69f38c","page":"96"}
```

A generation without `paragraph_pages` returns `404` with the broader locator
span. Invalid offsets or chunk IDs return `400`.

## Knowledge graph

The KG API reads the materialized read model built from active snapshots:
`kg_entity_roots`, `kg_relation_triples`, and `kg_relation_evidence_docs`. Raw
extractor rows remain the source of truth; the read model is a rebuildable
projection. `min_mentions` is the number of distinct chunks that must mention an
entity root or endpoint; it defaults to `2` to suppress one-hit entities.

Entity IDs in read requests are root-aware. Passing a variant entity ID resolves
to `coalesce(alias_of, id)` before neighbors or relations are read.

When search source hydration is wired, neighbor and relation responses use an
envelope with a `sources` map keyed by evidence chunk ID. Without hydration,
they are bare arrays. Entity search is always a bare array. Empty arrays are
serialized as `[]`, never `null`.

### `GET /api/kg/entities`

| Query parameter | Default | Range |
| --- | --- | --- |
| `q` | empty | Text; empty lists the largest hubs |
| `min_mentions` | `2` | Clamped to `1`–`100` |
| `limit` | `50` | Clamped to `1`–`200` |

Malformed numeric values use their defaults. Entity matching applies the same
normalization to the query and stored forms:

1. lowercase;
2. replace `ß` with `ss`;
3. remove characters outside ASCII letters/digits and `äöü`, including spaces,
   hyphens, punctuation, `%`, and `_`;
4. map `theory` to `theorie` and `sustainability` to `nachhaltigkeit`;
5. for forms of at least six characters, strip one trailing `en`, `er`, `e`, or
   `s` suffix.

Results are KG family roots. `canonical_form` is the survivor/root form.
`forms` lists the visible family forms: the root plus variants linked by
`alias_of`. Matching can find a root through any family form. Ranking uses the
best tier reached by any form in the family, with mention count used only inside
a tier:

1. exact lowercased form;
2. normalized-equivalent form without bilingual-family substitution;
3. bilingual-family equivalent;
4. substring or reverse-containment decomposition.

Live response shape:

```json
[
  {
    "id": "1a34dc2f-661d-4e20-9593-30b21d63e02c",
    "canonical_form": "stakeholders",
    "text": "Stakeholders",
    "type": "CONCEPT",
    "mentions": 104,
    "forms": ["stakeholder", "stakeholders"]
  }
]
```

### `GET /api/kg/entities/{id}/neighbors`

`id` must be an entity UUID; variant IDs resolve to their family root.
`min_mentions` and `limit` have the same defaults and ranges as entity search.
Each edge contains `other_id`, `other_form`, optional `other_type`, `direction`
(`in` or `out`), relation `type`, optional persisted `strength`, computed
`confidence`, evidence chunk IDs, and the other endpoint's mention count.

Neighbors are read from materialized root-level triples. Intra-family self-loops
are excluded during read-model refresh.

### `GET /api/kg/relations`

| Query parameter | Default | Meaning |
| --- | --- | --- |
| `type` | empty | Exact relation-type filter |
| `entity_id` | empty | Source or target entity UUID; variants resolve to roots |
| `entity` | empty | Legacy alias used only when `entity_id` is absent |
| `document_id` | empty | Require evidence in this document's active snapshot |
| `min_mentions` | `2` | Endpoint stability floor, clamped to `1`–`100` |
| `limit` | `50` | Result cap, clamped to `1`–`200` |

Relations are ordered by cross-document corroboration first and endpoint
popularity second. `document_id` narrows evidence, but corroboration remains the
global count across active snapshots. The `(source_root_id, target_root_id,
type)` triple is unique in the read model. Direction is deterministic after raw
edge grouping: majority raw direction wins; evidence support breaks ties.

```json
{
  "relations": [
    {
      "id": "0c2e9ef3-c238-4b23-b649-2177e151943b",
      "type": "part_of",
      "source_id": "c38dc58e-8c63-470d-9f7b-be336750f556",
      "source_form": "mitarbeiter",
      "target_id": "8be61cdc-e5dd-4e7d-9dcd-76431c9f13cd",
      "target_form": "organisation",
      "strength": 0.7,
      "confidence": 0.9740336,
      "evidence_chunks": ["aad45cce-c8e2-479a-8385-ed7589c99fea"],
      "documents": 34,
      "corroborating_documents": 34
    }
  ],
  "sources": {
    "aad45cce-c8e2-479a-8385-ed7589c99fea": {
      "doc_id": "1e6b5887-86d6-4a16-9783-ae237edd0f21",
      "title": "Veränderungen erfolgreich managen",
      "authors": ["Bartscher", "Stöckl"],
      "year": 2018,
      "publisher": "Haufe",
      "language": "de",
      "tags": ["neutral", "secondary source"]
    }
  }
}
```

`documents` is the compatibility name for the number of distinct corroborating
library documents. `corroborating_documents` carries the same value under its
explicit name. It is not an evidence-chunk count.

`confidence` is computed at read time; persisted extractor `strength` remains
unchanged:

```text
confidence = 0.6 × (1 - 1/(1 + documents))
           + 0.3 × (1 - 1/repetition)
           + 0.1 × section_quality

repetition = evidence_chunk_count + matching_triple_row_count - 1
```

`section_quality` is the fraction of returned evidence chunks not classified as
frontmatter. All terms are bounded and monotonic. A practical consumer filter
is `confidence >= 0.65` and `corroborating_documents >= 2`.

### `POST /api/kg/consolidate`

The route takes no parameters. It runs guarded exact-form entity consolidation
under the KG maintenance lock and refreshes the read model. The survivor has
the most distinct mention chunks, with the smallest UUID as deterministic
tiebreaker. Mentions move to the survivor, relation endpoints are repointed,
evidence chunk IDs remain unchanged, duplicate mention spans are skipped, and
loser semantic evidence is archived before loser rows are deleted.

Homonym/type guards apply: naked `PERSON` surnames do not merge, incompatible
type families stay separate, and non-`PERSON` majority arbitration is
mention-weighted. The operation is idempotent.

```json
{"merged":7,"duplicate_forms_before":7,"duplicate_forms_after":0}
```

The same consolidation runs automatically after every successful Zotero sync.
A 10-second debounce collapses a burst of completed syncs into one run. The run
uses its own 30-minute timeout; an error is logged and does not change the
already successful sync response. A pending timer is cancelled during graceful
shutdown.

## Processor source delivery

### `GET /api/processor/source/{jobID}?exp=UNIX&sig=HEX`

This route is for runners, not interactive clients. The dispatcher signs the
ASCII string `jobID|exp` with HMAC-SHA256 using
`AXIOM_PROCESSOR_SOURCE_SECRET`; `sig` is the lowercase hexadecimal digest.
The server verifies, in order:

1. source delivery is configured;
2. `exp` parses and has not expired;
3. the signature matches in constant time;
4. the job and local source exist;
5. the job is `claimed` or `processing`;
6. its database lease has not expired;
7. the path is a regular readable file.

Every failure returns `404`, including malformed signatures, unknown jobs,
database lookup failures, and missing files. This prevents the route from
becoming an existence oracle. A valid response streams the source in place,
sets its stored content type when available, and supports HTTP range and
conditional requests through `http.ServeContent`.

## Repair API

The repair routes exist only when PostgreSQL is configured and a Zotero write
key can be read from `AXIOM_ZOTERO_WRITE_KEY_FILE`. They are an operational
surface for the fix service. The Go service remains the only Zotero write
gateway and keeps originals under `AXIOM_QUARANTINE_ROOT` before mutation.

### `GET /api/repair/queue`

Returns `{"cases":[...]}`. Each queue item extends the repair-case fields with
`title`, `creators`, `publisher`, `publication_year`, attachment/document Zotero
keys, `local_path`, and optional `epub_path`. A case whose attachment or
document disappeared is moved to `blocked_for_dudu` instead of being silently
served forever.

### `GET /api/repair/cases`

Returns `{"cases":[...]}` for the 100 most recently updated cases. Fields are
`id`, `status`, `attempts`, `suspicion_class`, `verify_score`,
`verify_contradictions`, optional `verdict`, optional `blocked_reason`, `title`,
and `updated_at`.

### `POST /api/repair/cases/{id}/claim`

Claims the case and returns the complete repair-case record. State conflicts
return `409`.

### `POST /api/repair/cases/{id}/verdict`

The endpoint accepts form fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `verdict` | no handler presence check | Expected values are `auto_apply`, `blocked`, or `failed`; an unknown or empty value becomes an effective blocked verdict |
| `score` | yes | Finite floating-point verification score |
| `contradictions` | no | Integer contradiction count; absent or unreadable input becomes `0` |
| `plan` | yes | Valid JSON repair plan |
| `plan_version` | no | Integer plan version; absent or unreadable input becomes `0` |
| `blocked_reason` | for blocked results | Human-readable reason |
| `healed_pdf` | for effective auto-apply | Non-empty multipart file |

Blocked and failed verdicts may use URL-encoded forms. An effective auto-apply
requires multipart data and executes this custody sequence: quarantine the
original, write the quarantine audit row, delete the old Zotero attachment,
create the healed attachment under the schema filename, write mutation audit
rows, and mark the case healed. The pre-delete audit is fail-closed. Success
returns `effective`; an applied repair also returns `applied`,
`new_attachment_key`, `filename`, and `quarantine`.

### `GET /api/repair/docs/{documentKey}/locator-stats`

`documentKey` is the parent item's Zotero key. The response groups active chunks
by `locator.page_source` and includes up to three sample chunk IDs and labels per
group:

```json
{
  "document": "5SGGSPJV",
  "locator_stats": [
    {
      "page_source": "folio_verified",
      "chunks": 29,
      "sample_chunk_ids": [
        "954d8e68-f597-4f4b-80af-a5bcc3a3141d",
        "b9023573-427d-4e0f-af9d-c6ff705fc483",
        "b9f62884-a219-4183-8b59-62d7e3e89318"
      ],
      "sample_labels": ["1316", "1317", "1318"]
    }
  ]
}
```
