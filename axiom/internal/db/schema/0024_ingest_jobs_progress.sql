-- 0024_ingest_jobs_progress.sql
-- #369: coarse job progress (phase + position) on the jobs row, written by
-- the dispatcher from the runner's live status (#225 §9 progress payload).
-- Gives operators (jobs endpoint) and post-mortems the phase/position view
-- that distinguishes "silently working" from "hung" — the wave incident of
-- 2026-10-09 showed both states are indistinguishable today.
-- last_progress_at is the durable evidence stamp: the dispatcher-side
-- no-progress watchdog keys off the in-memory poll signature, this column
-- is the operator-visible trail (additive, nullable, no index needed — the
-- claim scan never reads it).
ALTER TABLE ingest_jobs
  ADD COLUMN IF NOT EXISTS progress_phase TEXT,
  ADD COLUMN IF NOT EXISTS progress_done INT,
  ADD COLUMN IF NOT EXISTS progress_total INT,
  ADD COLUMN IF NOT EXISTS last_progress_at TIMESTAMPTZ;
