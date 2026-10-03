-- 0006_ingest_jobs_lease.sql
-- Additive lease protocol columns for the dispatcher work package (Gate 1).
-- Every post-claim mutation is fenced by lease_token; retry scheduling is
-- driven by next_attempt_at; input_snapshot / processing_profile / profile_hash
-- / idempotency_key freeze the immutable processor request IN THE SAME
-- transaction as the claim. All columns are additive (IF NOT EXISTS) so an
-- already-applied upgrade DB migrates safely; all are nullable.
ALTER TABLE ingest_jobs
  ADD COLUMN IF NOT EXISTS lease_token UUID,
  ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS last_heartbeat_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS cancel_requested_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS input_snapshot JSONB,
  ADD COLUMN IF NOT EXISTS processing_profile JSONB,
  ADD COLUMN IF NOT EXISTS profile_hash TEXT,
  ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

-- Claim scan (FIFO) reaches the queue head through one of two routes:
--   (a) pending and due:   status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=now())
--   (b) expired lease:     status IN ('claimed','processing') AND lease_until <= now()
-- Partial, purpose-built indexes so each route scans a small subset. The time
-- columns are INDEX COLUMNS (btrees support the <=now() range scan); the
-- partial predicate is restricted to status only because postgres forbids
-- STABLE functions such as now() inside a partial-index predicate.
CREATE INDEX IF NOT EXISTS ingest_jobs_claim_pending_idx
ON ingest_jobs (next_attempt_at, enqueued_at)
WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS ingest_jobs_claim_expired_idx
ON ingest_jobs (lease_until)
WHERE status IN ('claimed','processing');
