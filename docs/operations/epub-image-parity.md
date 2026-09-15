# EPUB image parity — corpus diagnostic harness (#274)

The dev fix (#274, release train with #245) makes EPUB ingest interleave
`![](...)` image markers exactly like the PDF path: figure-wrapped HTML
images become markdown markers at their text position, references resolve
to `image-XXXX` artifact refs, machine captions attach to referencing
chunks, and document figure captions pair positionally.

**Corpus reprocessing is an ops decision, not part of the dev deliverable.**
This page is the repeatable harness ops runs when scheduling it.

## When to run

- Before scheduling the EPUB reprocessing wave: establishes the baseline
  (expected: 0 image-marker chunks on every EPUB — the gap that motivated
  #274).
- After the wave: acceptance gate — every image-bearing EPUB must show
  marker chunks and non-empty `image_refs`.

## Diagnostic: image linkage per EPUB

```bash
psql axiom_db -c "
SELECT d.title,
       count(c.id)                                AS chunks_total,
       count(*) FILTER (WHERE c.text LIKE '%![%](%') AS chunks_with_marker,
       count(*) FILTER (WHERE c.image_refs <> '[]'::jsonb)
                                                    AS chunks_with_refs,
       count(*) FILTER (WHERE c.image_captions IS NOT NULL
                          AND c.image_captions <> '{}'::jsonb)
                                                    AS chunks_with_machine_caps,
       count(*) FILTER (WHERE c.figure_captions IS NOT NULL
                          AND c.figure_captions <> '{}'::jsonb)
                                                    AS chunks_with_figcaps
  FROM zotero_documents d
  JOIN LATERAL (
        -- snapshot AND its attachment from ONE lateral, so the chunk
        -- set and the content_type filter always describe the same
        -- snapshot (the unique index is per (document, attachment,
        -- profile), not per document — twin attachments could otherwise
        -- desynchronize the two subselects).
        SELECT s.id AS snap_id, s.attachment_id
          FROM processing_snapshots s
         WHERE s.document_id = d.id AND s.active
         ORDER BY s.created_at DESC LIMIT 1
       ) snap ON true
  JOIN processing_chunks c ON c.snapshot_id = snap.snap_id
  JOIN zotero_attachments a ON a.id = snap.attachment_id
 WHERE a.content_type = 'application/epub+zip'
   AND NOT d.deleted
 GROUP BY d.id, d.title
 ORDER BY chunks_with_marker DESC, d.title;
"
```

Read as one row per EPUB:

| Column | Meaning |
|---|---|
| `chunks_with_marker` | chunks whose text interleaves an image marker |
| `chunks_with_refs` | chunks with non-empty `image_refs` (marker → artifact link) |
| `chunks_with_machine_caps` | chunks carrying machine captions (`#230`) |
| `chunks_with_figcaps` | chunks carrying document figure captions (`#268`) |

## Acceptance gate (post-reprocessing)

The wave counts as accepted when, for every EPUB row:

- `chunks_with_marker > 0` **iff** the book ships images (see artifact
  census below), and
- `chunks_with_marker = chunks_with_refs` for that row (every marker
  resolved to an artifact — an unresolved ref is a terminal ingest fail,
  not a silent gap).

PDF regression companion (the PDF path must be untouched by the #274
change): the same query with `content_type = 'application/pdf'` must
return the pre-wave numbers — PDF corpus figures may not move.

### Corpus counter-probe (pre-reprocessing, offline)

`axiom_ng_runner/scripts/epub_image_census.py` runs the branch worker
over every `.epub` in a directory tree (no database involved) and
gates on the same two invariants per book: zero raw `<figure`/`<img>`
remains and zero unresolved chunk refs. This is the offline regression
bar the #274 review rounds used — fixture-only validation missed two
corpus regressions that this probe caught.

```bash
.venv/bin/python axiom_ng_runner/scripts/epub_image_census.py <zotero-storage-root>
```

Recorded baseline (175 Zotero EPUBs, pandoc 3.7, 2026-09-15):
5,570 markers / 0 raw remains / 0 unresolved refs / 5,367 artifacts.
Re-run and compare before any pandoc bump.

## Artifact census (which EPUBs ship images at all)

```bash
psql axiom_db -c "
SELECT d.title,
       count(*) FILTER (WHERE ar.kind = 'extracted_image') AS images,
       count(*) FILTER (WHERE ar.attributes ? 'machine_caption') AS captioned
  FROM zotero_documents d
  JOIN LATERAL (
        SELECT s.id, s.attachment_id FROM processing_snapshots s
         WHERE s.document_id = d.id AND s.active LIMIT 1
       ) snap ON true
  JOIN processing_artifacts ar ON ar.snapshot_id = snap.id
  JOIN zotero_attachments a ON a.id = snap.attachment_id
 WHERE a.content_type = 'application/epub+zip' AND NOT d.deleted
 GROUP BY d.id, d.title
 ORDER BY images DESC;
"
```

An EPUB with `images > 0` and `chunks_with_marker = 0` is the #274 gap
class (pre-fix generations). After the wave no such row may exist.

## Notes

- The queries target the ACTIVE snapshot per document (the format switch
  chain #228/#232 keeps exactly one active), so twin EPUB/PDF documents
  report the format the retrieval contract actually serves.
- EPUB citations stay APA 7 sections only (#245) — page fields never
  return to the client contract, regardless of image parity.

## See also

- [Rebuild Wave Runbook](rebuild-wave-runbook.md) — the firing sequence
  when the reprocessing wave is scheduled.
