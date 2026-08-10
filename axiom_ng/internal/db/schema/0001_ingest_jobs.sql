-- 0001_ingest_jobs.sql
-- Queue that orchestrates Zotero document processing. The queue is the
-- source of truth for ingest state: status is advanced by claim, and workers
-- obtain an exclusive lease before processing.
--
-- Foreign keys to zotero_* tables are added in a later migration once those
-- tables exist; here we keep the queue self-contained so it can build and run
-- independently of the Zotero schema layer.
CREATE TYPE ingest_job_status AS ENUM (
  'pending',
  'claimed',
  'processing',
  'completed',
  'failed',
  'cancelled',
  'skipped'
);

CREATE TABLE IF NOT EXISTS ingest_jobs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  status ingest_job_status NOT NULL DEFAULT 'pending',
  content_hash TEXT,
  force_rebuild BOOLEAN NOT NULL DEFAULT false,
  claimed_by TEXT,
  lease_until TIMESTAMPTZ,
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  error_code TEXT,
  error_message TEXT,
  processor_name TEXT,
  processor_version TEXT,
  result JSONB NOT NULL DEFAULT '{}',
  enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency/index sizing land together with the zotero_* FK columns in the
-- next migration, once attachment identity is known. For now index by status
-- so the lease-based claim scan stays fast.
CREATE INDEX IF NOT EXISTS ingest_jobs_status_idx
ON ingest_jobs (status, enqueued_at);
