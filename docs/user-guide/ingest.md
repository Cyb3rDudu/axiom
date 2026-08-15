# Ingest

Ingest is the pipeline that turns a `pending` job into a processed, indexed
result. This chapter follows one document through that lifecycle — in user
language, with a SQL recipe at the end for those who like to peek under the hood.

## A job's life, in user terms

Every document axiom processes is one *job*. A job moves through a few states:

```text
pending → claimed → processing → completed
                                 ↘ failed
                                 ↘ cancelled
```

- **pending** — waiting in line. It will be picked up as the pipeline has
  capacity.
- **processing** — actively being worked on: converted, chunked, embedded,
  extracted. The chapter was "claimed" by a worker, so no other worker touches
  it.
- **completed** — done and durably stored. The document's chunks are in the
  store and the search index is updated.
- **failed** — hit a problem (usually visible in the error). Most failures have
  a clear cause; see the troubleshooting note below.
- **cancelled / skipped** — you (or the system) asked it to stop, or it became
  obsolete (e.g. the attachment was removed).

A job is never lost mid-flight: if the processing service restarts, the job
simply gets picked up again where the contract allows — it does **not** silently
turn into a fake "completed".

## Watch your jobs

The simplest way to observe the lifecycle is a status query:

```bash
curl http://127.0.0.1:8011/api/ingest/jobs
```

You'll see each job with its document, current status, and timing. A freshly
synced library shows a run of `pending` jobs that turn `processing` and, for
small documents, `completed` within seconds-to-a-minute; larger scanned books
take longer (they're compute-heavy).

For a quick per-job detail, look at the same endpoint filtered to one job you
care about — it includes the stage it's in.

> **Do I have to watch SQL to use axiom?** No. The status query above is enough
> for normal use. The SQL below is only for the curious or for operators.

### SQL recipe (for the interested)

If you have database access, the job table tells the whole story:

```sql
-- Where each job is, at a glance
SELECT status, count(*), min(started_at), max(completed_at)
FROM ingest_jobs
GROUP BY status;

-- Distribution of completed work (which runner did what)
SELECT runner_name, count(*), avg(completed_at - started_at)
FROM ingest_jobs
WHERE status = 'completed'
GROUP BY runner_name;
```

Recall that `completed` is set only after the result is durably committed — so
this view is the *truth*, not a hopeful status.

## What a "profile" is

A *processing profile* decides how much of the pipeline a job runs. The default
in the reference setup is `full-rag-v1`, which turns on the full flow:
conversion, chunking, **dense + sparse embeddings**, **entity extraction**, and
**relationship extraction** (plus images).

For a first test you can start with the same profile and it "just works" on the
default compute. When you later wire a real GPU runner, the same profile drives
the heavier models. Profiles are applied per job from the job's stored options;
the profile **name alone doesn't toggle features** — the concrete options do, so
two "full-rag" runs are explicit about what they computed.

## Does a job survive a restart? Yes.

If axiom or the runner restarts mid-run, you keep your place:

- A **pending/claimed** job gets picked up again by a healthy worker.
- A **processing** job is not lied about — if it didn't finish + commit, it does
  not show `completed`. It is reassigned and redone as the contract permits.
- A **completed** job is already durable; nothing needs re-running.

In short: restarting services is safe. That's the reassurance-demo part of the
design — no silent data loss, no phantom "done".

## Troubleshooting (common three)

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| Job stuck in `pending` | No worker enabled, or dispatcher off | Ensure the runner is up and `AXIOM_DISPATCHER_ENABLED=true`; see Quickstart. |
| Job `failed` with a source error | Attachment path not allowed, or file missing | Confirm `AXIOM_PROCESSOR_ALLOWED_SOURCE_ROOTS` covers your Zotero storage; resync to re-create the job. |
| Job `failed` after a long run | Model/compute backend mismatch or OOM | Match the compute backend between runner and profile; for the reference config nothing heavy is needed. |

> A `failed` job can usually be retried by triggering a fresh sync/process for
> that attachment — reprocessing is not blind: the hash gate means only changed
> or invalidated documents are redone.

Next: [Retrieval](retrieval.md) — searching what you've indexed.
