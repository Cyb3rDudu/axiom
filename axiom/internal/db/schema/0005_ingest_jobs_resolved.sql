-- 0005_ingest_jobs_resolved.sql
-- Adds resolved_at to ingest_jobs so a failed job can be deduplicated only
-- against an UNRESOLVED identical current failure. When a file comes back and a
-- pending job is enqueued, prior failed jobs for that attachment are resolved;
-- a later real failure therefore creates a fresh (unresolved) failed job
-- instead of being suppressed by a stale historical failure.
ALTER TABLE ingest_jobs ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS ingest_jobs_failed_unresolved_idx
ON ingest_jobs (attachment_id, error_code) WHERE status='failed' AND resolved_at IS NULL;
