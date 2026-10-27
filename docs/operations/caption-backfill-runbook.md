# Image-Caption Backfill Runbook — partial caption recovery (#387)

**Status:** operational runbook · **Firing authority:** operator, when an active snapshot shows missing machine captions (the `IMAGE_CAPTIONS_INCOMPLETE` warning, or a pre-#387 rebuild that completed green with caption gaps)
**Scope:** regenerating ONLY the missing machine captions of an already-active snapshot. No re-ingest, no GPU conversion pass, no chunk re-chunking — image extraction is done, the artifact bytes are in the store.

This is the cheap repair lane for the #385 caption wave's core blocker (#387):
caption phases were losing requests silently (dropped API failures, budget
tail cutoffs) while jobs completed green. Since #387 the losses are visible
(warning entries with reason buckets, retry with backoff at ingest), and
this runbook recovers the historical gaps without the ~2h full re-ingest
per book.

## Root cause (for the record, #387)

Two mechanisms produced the two observed gap patterns:

- **Tail cutoff** (Capital Markets: 416 refs, 67 missing, chunks ~61+ uniformly empty): the `image_captions` stage budget (`AXIOM_PROCESSOR_IMAGE_CAPTIONS_BUDGET_SECONDS`, default 900 s) expired mid-pass; the stage stopped between images ("honest partial") and the remaining images were skipped. The reason lived only in `manifest.stage_completion.image_captions_reason = STAGE_BUDGET_EXCEEDED` — never in the warnings manifest, so the job looked green.
- **Scattered gaps** (PEN-200: 1183 refs, 559 missing from chunk 4 onward, varying run to run): every per-image captioner exception — 429 rate limits, 5xx, timeouts — was caught inside `caption_image`, logged at WARNING, and returned as an uncaptioned image with no retry and no warning entry. The run-to-run variance is the signature of nondeterministic API errors being swallowed.

The `captions: hydration failed (serving without images): context canceled` log line is the SERVING-side caption hydration (search path) reacting to a cancelled request context — a symptom of reading a half-captioned snapshot, not a cause of the gaps.

## What #387 changed at ingest

- Per-image misses are bucketed (`api_error`, `rate_limited`, `timeout`, `budget_exceeded`, `no_image_data`, `not_provisioned`) and emitted as an `IMAGE_CAPTIONS_INCOMPLETE` entry in the snapshot warnings manifest (`captioned`/`total`/`missing`/`reasons`/`missing_refs` capped at 200).
- Transient failures (429/5xx/timeout/transport) are retried with bounded exponential backoff (`AXIOM_PROCESSOR_IMAGE_CAPTION_RETRIES`, default 3; `AXIOM_PROCESSOR_IMAGE_CAPTION_BACKOFF_SECONDS`, default 1 s, cap 10 s). Permanent 4xx errors are not retried.

## The backfill procedure

All state-changing steps are marked **[MUTATES]**. Host-neutral placeholders as in the rebuild-wave runbook.

### 1. Measure the gap (read-only)

```sql
-- uncaptioned extracted_image artifacts of a document's active snapshot
SELECT a.ref, a.sha256, a.media_type, a.storage_path
FROM processing_artifacts a
JOIN processing_snapshots s ON s.id = a.snapshot_id
WHERE s.document_id = :doc_id AND s.active
  AND a.kind = 'extracted_image'
  AND (a.attributes->>'machine_caption') IS NULL
ORDER BY a.ref;
```

Post-#387 snapshots also carry the count in
`processing_snapshots.warnings` (`IMAGE_CAPTIONS_INCOMPLETE`).

### 2. Feed the misses to the backfill engine **[MUTATES caption cache only]**

On a worker host with the caption env (`AXIOM_CAPTION_API_BASE/_KEY/_MODEL`),
artifact-store read access, and the caption cache mounted
(`AXIOM_CAPTION_CACHE_DIR` — important: the hash-gate cache makes every
previously captioned image free and persists this run's captions for the
next rebuild):

```bash
psql "$DATABASE_URL" -At -F' ' -c "
  SELECT a.ref, a.sha256, a.media_type, a.storage_path
  FROM ... (the query above)" \
| while read ref sha media path; do
    jq -n --arg r "$ref" --arg s "$sha" --arg m "$media" --rawfile b "$path" \
      '{ref:$r, sha256:$s, media_type:$m, image_b64:($b|@base64)}'
  done \
| python -m axiom_compute_worker.compute_core.image_caption_backfill_cli \
    > backfill-result.json
echo $?   # 0 = complete, 2 = partial (reasons itemized per ref), 1 = engine failure
```

Each output row is `{"ref","caption","model","path"}` (success) or
`{"ref","reason"}` (honest miss). The stderr summary names the reason
counts. Re-run for the remaining refs (rate-limit windows) until 0 or the
remainder is acceptable and itemized.

### 3. Apply the recovered captions **[MUTATES]**

Apply successes to the artifact attributes and the referencing chunks'
`image_captions` (same columns the ingest path writes; generate the SQL
from `backfill-result.json`, e.g. with `jq`), inside one transaction per
snapshot. Schema references: `processing_artifacts.attributes
.machine_caption/caption_model/caption_path` (#230), `processing_chunks
.image_captions` (`ref → caption`).

### 4. Re-embed the newly captioned chunks (dense arm)

Feed the affected chunks through the existing caption-backfill dense
CLI (#257, `axiom_compute_worker.compute_core.caption_backfill_cli`) so
the dense vectors see the new caption text — same engine the wave already
uses.

### 5. Verify

Re-run the measurement query (step 1): missing count must be 0 (or the
itemized remainder). Search-path spot check on an affected document.

## Done-definition for the wave unblock (#385)

PEN-200 (559) and Capital Markets (67) backfilled to 0 — or remainder
itemized per reason; one fresh large-book force-rebuild showing a
warnings manifest consistent with actual caption coverage; `make
ci-local` green.
