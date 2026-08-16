# Monitoring

This chapter is for operators: how to observe what axiom is doing, at three
levels, without guessing. It is written as generic patterns and copyable
queries — no specific machine or network detail required.

## The three levels at a glance

axiom makes its internal progress observable on three levels. From coarse to
fine:

1. **Job status via API** — `which documents are done, stuck, or failing?`
2. **Stage progression** — `where is a single job right now, and how long did
   each stage take?`
3. **Phase logs** — `what exactly did a worker do, and in what order?`

Most operations start at level 1, drill into level 2 for a specific job, and use
level 3 when something is stuck and the reason isn't obvious.

## Level 1 — Job status via API

The ingestion surface reports every document's lifecycle (see the
[User Guide → Ingest](../user-guide/ingest.md) for the states in user terms):

```bash
# Overall picture
curl http://<host>:<port>/api/ingest/jobs
```

Expect a status of `pending`, `claimed`, `processing`, `completed`, `failed`,
`cancelled`, or `skipped` (the full state list, in user terms, is in the
[User Guide → Ingest](../user-guide/ingest.md)). This endpoint returns a paged
list (a `limit` query param); it has no per-job detail route. The per-job look
happens on the runner — see Level 2.

## Level 2 — Stage progression (per job)

Each job moves through named pipeline stages. The contract exposes a live stage
per processor job:

```text
validate_source → convert → chunk → embed → entities → relationships → assemble
```

To see where a job currently is, ask the **runner**, not the dispatcher:
`GET http://<runner-host>:<runner-port>/v1/jobs/{job_id}` returns the live
`stage` (plus progress). Use the job id straight from the ingest list — the
dispatcher submits that same id to the runner unchanged, so no id translation
is needed; the dispatcher's `/api/ingest/jobs` simply has no per-job route
(only the paged list). After the job completes, the same stages are
reconstructible from the persisted `manifest.stage_timings`, so post-hoc
analysis needs no live observation.

**Use it to answer:** "is it converting, embedding, or stuck early?" A job that
sits in `chunk` for a very long time points at a different cause than one stuck
in `embed`.

## Level 3 — Phase logs

For the finest grain, look at the dispatcher's phase lines. Each job is traced
through its lifecycle with explicit phase markers:

```text
claim → submit → completed → resultFetched → staged → persisted → acked
```

The `phases[…]` line itself identifies the runner (when named) and the job id,
plus phase timestamps (and an artifact count on staging). The finer-grained
identifiers — attempt counter, lease-token prefix, document/attachment id — ride
the surrounding per-job log lines, not the phase line. **No document content
is ever logged.**

**Use it to answer:** "did the dispatcher actually claim the job, and did the
result get fetched and persisted?" A job that reaches `persisted` is durable no
matter what happens next; one that never leaves `submit` never reached the
runner.

## SQL: distribution and throughput (copy-paste)

If you have database access, the job table answers the operational questions
directly. Replace the placeholders with your own identifiers. (This is optional
for everyday use; the API above is enough for normal operations.)

```sql
-- 1. A snapshot of where every job is
SELECT status, count(*) AS n
FROM ingest_jobs
GROUP BY status
ORDER BY n DESC;

-- 2. Distribution of completed work (which runner did what)
--    Replace <your-runner-label> with a real runner name, or GROUP BY runner_name.
SELECT runner_name, count(*) AS jobs,
       round(avg(extract(epoch FROM (completed_at - started_at)))::numeric / 60, 1) AS avg_minutes_per_job
FROM ingest_jobs
WHERE status = 'completed'
GROUP BY runner_name
ORDER BY jobs DESC;

-- 3. Throughput: completed per unit of time
SELECT date_trunc('hour', completed_at) AS hour,
       count(*) AS completed
FROM ingest_jobs
WHERE status = 'completed'
  AND completed_at IS NOT NULL
GROUP BY 1
ORDER BY 1;

-- 4. The long tail: the longest-running in-flight jobs
--    (a quick health proxy — see Troubleshooting for what it may mean)
SELECT id, started_at, completed_at
FROM ingest_jobs
WHERE status = 'processing'
ORDER BY started_at ASC
LIMIT <N>;
```

> **Caveat on numbers:** `completed` is set only after the result is durably
> committed — so these queries report *truth*, not hopeful status. `claimed_at`
> does not exist; the measured quantities are `started_at` and `completed_at`.

## Latency and utilization hints

- **Dispatcher overhead** is seconds per job (the gap between consecutive
  `completed_at`/`started_at` values is small when the runner keeps up).
- **GPU utilization** is best measured with a labeled sampler that records which
  runner is active (see [Deployment → runner identity](deployment.md)). Labeled
  samples let you attribute utilization to a specific runner after a batch.

Next: [Troubleshooting](troubleshooting.md) — symptom → cause → fix.
