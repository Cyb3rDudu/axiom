---
name: zotero
description: Use the local Zotero HTTP API to manage the library — create book/journal items with verified metadata, import EPUB/PDF attachments with convention-compliant names, write notes and highlights, use tags and collections correctly. Use when a task requires filing a source into Zotero, attaching a file, creating a note on an item, or reading/modifying library entries. Covers the local API auth model, the file-import protocol, naming/tagging conventions, pagination, trash semantics, and versioned writes.
---

# Zotero (local library, via HTTP API)

The library is Zotero 10+ with the local API enabled (Settings → Advanced → "Allow other applications to communicate with Zotero"). This skill covers writing items, attachments, notes, and annotations the way the library's conventions demand.

## Access & auth

```
Base:  http://localhost:23119/api
Headers for READ:  none required
Headers for WRITE:
  Zotero-API-Version: 3
  Zotero-Server-ID:   <value from any response header 'Zotero-Server-ID'>
  Zotero-API-Key:     <local key — authorize once:
                      POST /api/local/authorize {"appName":"<name>"}
                      → user confirms in Zotero dialog, "Always Allow" persists>
```

- Both auth header variants work: `Zotero-API-Key: <key>` and `Authorization: Bearer <key>`.
- The local key is **not** a cloud key; it is invalid against api.zotero.org.
- Optimistic concurrency: PATCH/PUT/DELETE need `If-Unmodified-Since-Version: N`, where N is the item's current version (GET body `version` or response header `Last-Modified-Version`).

## Reading: pagination is mandatory

`GET /items` returns **max 100 items** per call. Always loop:

```
start=0; loop: GET /items?limit=100&start={start} → until page < 100
```

Without the loop an agent silently reads only the first ~19% of a large library (verified on a 530+-item library). `q=` searches titles, **not** note bodies — for content matches, page through and filter locally.

## Filing a book (end-to-end — run in this exact order)

Every step below guards a failure that has actually occurred in this library. Do not skip or reorder.

### Step 1 — Duplicate check (BEFORE creating anything)
```
GET /items?limit=100&start=N&format=json   (loop until page < 100)
```
Match candidates by **normalized title** (`re.sub(r'[^a-z0-9äöüß]', '', title.lower())`, compare 20–25-char prefixes both directions) **and** by ISBN. Inspect the matches' **attachments**, not just the docs:
- Doc exists **with** the format you're about to import → **stop, report, do not import** (that's a duplicate).
- Doc exists **without** it → attach to this doc (go to Step 3, skip doc creation).
- No doc → create (Step 2).
Ambiguous matches (>1 candidate, or partial title overlap): **report, never guess.** e2e/test items named like real books ("[E2E-222] Databricks…") have caused wrong-parent imports here.

### Step 2 — Metadata from the book itself (never from the filename alone)
- **EPUB:** read `package.opf` inside the zip: `dc:title` (if multiple: first = title; **drop a second `<dc:title>` equal to `"main"`** — Springer artifact), `dc:creator` (filter out pure digits and `aut`/`edt` markers before splitting names), `dc:date` (year = first 4 chars), `dc:publisher`, `dc:language`, ISBN from `urn:isbn:…` (strip dashes).
- **PDF:** title page/imprint; beware back-cover ads for *other* books.
- Only fall back to Crossref/OpenLibrary/web when the interior has nothing. Unverified ⇒ field stays empty.

### Step 3 — Create the doc (only if Step 1 said none exists)
```
POST /items  [{"itemType":"book","title":…,"creators":[…],"date":…,"publisher":…,"language":…,"ISBN":…,"tags":[],"collections":[]}]
→ new key = resp["success"]["0"]   (string! not a dict)
```
After POST: GET the doc once and verify `data.title` — fix immediately (versioned PUT) if a "main"/subtitle slipped through.

### Step 4 — Attach the file
a) **Parent liveness check:** `GET /items/<parentKey>` — if `data.deleted` is truthy or 404 → the parent is trash/purged. **Do not attach.** Create a proper doc instead (a child posted under a trashed parent is auto-trashed with it).
b) Build the conventional filename from the doc's own existing attachments if any (copy their pattern, incl. an existing ` (EPUB)` suffix); otherwise `<Nachname> - <Jahr> - <Titel>.<ext>` per naming rules.
c) `POST /items  [{"itemType":"attachment","parentItem":<key>,"linkMode":"imported_file","contentType":"application/epub+zip"|"application/pdf","filename":<name>,"title":<name>,"note":<provenance if any>,"tags":[]}]`
d) Copy the bytes: `cp <source> ~/Zotero/storage/<attachmentKey>/<filename>` — the filename on disk **must match `filename` byte-exactly** (Umlauts: keep NFD/NFC as-is from the API string).
e) Source-file suffixes like `.injected` are **never** carried into the stored name.

### Step 5 — Provenance & tags
- Provenance goes in the attachment **`note`** field (e.g. `"page-list derived from PDF sibling (#222), 2026-08-26"`), optionally a machine tag like `page-list:derived-from-pdf-sibling-222`. Filename stays clean.

### Step 6 — Verify (three checks, all must pass)
1. `GET /items/<attKey>/file/view` → **302** to the file path you wrote.
2. Attachment census on the parent (paginated GET): the **counts per format** match the intended end state (e.g. "exactly 1 epub + 1 pdf", or "2 epubs + 1 pdf" when an enriched copy was added). Extra/missing ⇒ investigate before proceeding.
3. For imports meant to *replace* originals: confirm the old attachment is gone (direct GET → 404) and — if recoverability matters — that it was rescued to disk **first** (API-DELETE purges, see Deletion semantics).

Report per book: doc key, attachment key(s), filename, census result. On any ✗: stop, report, do not improvise.

## Creating an item (book example)

```
POST /api/users/0/items
[{
  "itemType": "book",
  "title": "Künstliche Intelligenz im produzierenden Mittelstand",
  "creators": [{"creatorType": "author", "firstName": "…", "lastName": "…"}],
  "publicationYear": 2024,       // or "date"
  "publisher": "…", "place": "…", "ISBN": "…", "edition": "…",
  "DOI": "…", "language": "de",
  "tags": [{"tag": "…"}],
  "collections": ["<collectionKey>"]
}]
→ {"success": {"0": "<KEY>"}, "successful": {"0": {…full item…}}, …}
```

Parse tip: `success.0` is the new key as a **string**; `successful.0` is the full item object. Both exist — don't index `success.0` as a dict.

Batch POST works (JSON array, mixed item types allowed). Field sets by type: journalArticle (`publicationTitle, volume, issue, pages, DOI, date, language`), report (`institution, reportTitle, place, reportNumber`), webpage (`websiteTitle, url, accessDate`), statute (`nameOfAct` instead of title!). Editors: `creatorType: "editor"`. Surname-only: `{"name": "Nachname"}`.

**Metadata verification hierarchy** (never guess authors/years — an empty field beats a wrong one):
1. PDF/EPUB interior (title page/imprint; watch for back-cover ads showing *other* books)
2. Crossref (`api.crossref.org/works?query.bibliographic=…`; Springer book DOI = `10.1007/978-<E-ISBN>`; chapter DOIs end in `_x` → use the base)
3. OpenLibrary by ISBN (verify title+author — false matches happen)
4. Web search as last resort

Creator gotcha (Springer OPFs): `<dc:creator>` lists interleave names with sequence numbers and role markers (`"Jason Yip", "1", "aut", "Nikhil Gupta", "2", "aut"`). Filter out pure digits and `aut`/`edt` before splitting names.

## Attaching a file (the 3-phase upload contract)

```
1. Attachment item:
   POST /api/users/0/items
   [{"itemType":"attachment","linkMode":"imported_file",
     "contentType":"application/epub+zip" | "application/pdf",
     "filename":"<conventional name>","title":"<same>","parentItem":"<parentKey>"}]

2. Register upload:
   POST /api/users/0/items/<attKey>/file
   Content-Type: application/x-www-form-urlencoded
   If-None-Match: *
   body: md5=<md5>&filename=<name>&filesize=<bytes>&mtime=<MILLISECONDS>
   → {"url":"…","uploadKey":"…"}

3. Send bytes, then commit:
   POST <url>  (raw file bytes, Content-Type from the register response)
   POST /api/users/0/items/<attKey>/file   body: upload=<uploadKey>   If-None-Match: *   → 204
```

Pragmatic local shortcut (equally valid when Zotero and the files share the machine): create the attachment item, then copy the file directly to `~/Zotero/storage/<attachmentKey>/<filename>` — no upload protocol needed.

**Before setting `parentItem`: GET the parent and check `data.deleted`.** Attaching to a trashed parent auto-trashes the new attachment (verified: child silently disappeared with its trashed parent). This bites especially with e2e/test items that sit in the trash.

Attachments accept a **`note` field on POST** — use it for provenance/comments (e.g. `"note": "page-list derived from PDF sibling (#222)"`) without polluting the filename. Reads back via `data.note`.

## Naming convention (mandatory)

```
{Erstautor-Nachname | Institution} - {Jahr} - {Titel}.{ext}
```
- Title: `:` → ` - `, `/` → `-`, truncate at a word boundary near 80 chars
- Renaming an existing attachment: PATCH `filename`/`title` changes metadata only — also `os.rename` the file inside `~/Zotero/storage/<attKey>/`, or the link breaks
- macOS trap: normalize NFD/NFC when matching Umlaut filenames

## Notes

```
POST /api/users/0/items
[{"itemType":"note","parentItem":"<parent-or-attachment key>",
  "note":"<html body — may include <p>, <strong>, highlight/citation spans>",
  "tags":[{"tag":"…"}]}]
```

## Highlights / annotations (on PDF attachments)

```
[{"itemType":"annotation","parentItem":"<ATTACHMENT key>",
  "annotationType":"highlight",
  "annotationColor":"#ffd400",
  "annotationPageLabel":"114",
  "annotationPosition":"<JSON STRING, not an object!>",
  "annotationSortIndex":"{pageIndex:05d}|{top:06d}|{left:05d}"}]
```
- Coordinates: Zotero origin is **bottom-left**; convert from top-left PDF coords with `zotero_y = pageHeight − mupdf_y`

## Deletion semantics (verified — read before deleting)

- **API `DELETE` = immediate permanent purge.** 204 → item is 404 and *never* appears in `/items/trash`. No trash step, no recovery (tested on notes, attachments, and books).
- **UI delete = trash.** Items land in `/items/trash` and are restorable (also after Zotero restarts).
- **Restore from (UI) trash:** `PATCH /items/<key>` with the full item JSON plus `"deleted": 0` (and optionally a new `parentItem`), with `If-Unmodified-Since-Version`. Verified pulling an attachment out of the trash and re-parenting it in one PATCH.

## Tags & organization

- Quality tags: `peer-reviewed` (journalArticle), `secondary source` (book/proceedings/preprint), `grey literature` (report/webpage/blogPost), `primary source` (statute)
- Stance tags: `neutral` (default) / `kritisch` / `pro` / `opinionated`
- Usage tags: `<MODUL>_<TYP>` (e.g. `ORG_HA`, `VWL_PRÄ`) — from the working folder the source came from
- Subject-Areas collections: **books only**; non-book sources never go there (they live in module folders)

## Hard rules

1. **API-DELETE is irreversible** (see semantics above) — no deletions without explicit owner instruction. Trashed items from the UI (e.g. E2E leftovers in `/items/trash`) are swept only by the owner's word
2. **Never invent metadata**: unverified fields stay empty (note the gap in `extra` if relevant)
3. After import: verify each attachment with `GET /users/0/items/<attKey>/file/view` → 302 to an existing file
4. Duplicate guard: compare md5 before importing; same size = suspicion
5. Paginate every read (see Pagination) — a single unlooped GET is a partial read
