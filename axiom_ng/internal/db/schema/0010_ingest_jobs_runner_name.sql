-- 0010 (#122): runner identity per job — the basis for the TC2 scale proof
-- (which runner processed which book in what time, answerable via SQL).
-- Named runner_name, NOT processor_name: that column already exists (0001)
-- and holds the processor SOFTWARE identity written at completion from the
-- result payload — two meanings in one column would clobber each other.
-- Additive: existing rows keep NULL; the claim UPDATE fills it for new claims.
ALTER TABLE ingest_jobs ADD COLUMN IF NOT EXISTS runner_name text;
