-- 0007_ingest_jobs_ack_pending.sql
-- Additive {ACK} recovery column for the dispatcher (Gate 2, F3).
--
-- When the processor acknowledgement fails after a completed job has been
-- durably committed, we must retry the ACK separately (the processor keeps the
-- result until acknowledged -- see PROCESSOR_CONTRACT.md section 15) WITHOUT
-- ever rerunning processing. ack_pending_at records the first failed ACK and is
-- cleared once a retried ACK succeeds. A job that is completed with a non-NULL
-- ack_pending_at is picked up by an independent ACK-retry pass and re-acked,
-- never re-processed. The column is nullable and additive so an already-applied
-- upgrade database migrates safely.
ALTER TABLE ingest_jobs
  ADD COLUMN IF NOT EXISTS ack_pending_at TIMESTAMPTZ;
