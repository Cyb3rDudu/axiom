-- 0018_ingest_jobs_quality.sql
-- #175 PDF-Preflight: quality_state on the ingest job.
-- The dispatcher runs /v1/pdf/preflight at claim; this column carries the
-- structured report {verdict, finding, reason, pages, text_layer,
-- mean_chars_per_page, suspicious_patterns} so the quality decision is
-- observable on the job before/without full processing. NULL = not assessed.
-- #219: the keys were originally German (verdacht/grund); migration 0020
-- renamed the stored values to finding/reason before the client freeze.
ALTER TABLE ingest_jobs
    ADD COLUMN IF NOT EXISTS quality_state jsonb;
