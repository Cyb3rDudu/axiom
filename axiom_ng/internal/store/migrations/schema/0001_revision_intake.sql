-- 0001_store_revision_intake.sql — the revision intake columns on
-- ingest_jobs (F09 #303). Store-owned ledger (store_schema_migrations),
-- same physical database: the core schema fingerprint stays derived from
-- internal/db alone (the F01 freeze), exactly like the Library ledger
-- beside it. ADDITIVE ONLY — no renames, no drops (F12/DM06 own those).
--
-- intake_kind splits the two lanes during the strangler transition:
--   'zotero'   the legacy sync-enqueued job (attachment FK set at enqueue)
--   'revision' the F09 revision intake (attachment/document FKs resolved
--              at CLAIM from the mirror — the documented dual-read; NULL
--              FKs mean "not resolvable (yet)" and the claim obsoletes)
-- The revision identity columns keep the revision reachable from the job
-- row without joining the Library ledger; revision_json freezes the FULL
-- SourceRevision DTO verbatim (the store's copy of what it ingested).

ALTER TABLE ingest_jobs
  ADD COLUMN IF NOT EXISTS intake_kind            TEXT NOT NULL DEFAULT 'zotero',
  ADD COLUMN IF NOT EXISTS intake_idempotency_key TEXT,
  ADD COLUMN IF NOT EXISTS revision_source_id     TEXT,
  ADD COLUMN IF NOT EXISTS revision_record_id     TEXT,
  ADD COLUMN IF NOT EXISTS revision_rendition_id  TEXT,
  ADD COLUMN IF NOT EXISTS revision_no            TEXT,
  ADD COLUMN IF NOT EXISTS revision_json          JSONB;

-- Intake-key idempotency (the contract's replay/mismatch rule): one job
-- per caller key; a second mint with the same key compares revision_json.
CREATE UNIQUE INDEX IF NOT EXISTS ingest_jobs_intake_key_uq
  ON ingest_jobs (intake_idempotency_key)
  WHERE intake_kind = 'revision' AND intake_idempotency_key IS NOT NULL;

-- Revision-identity dedup, mirroring the legacy lane's
-- (attachment_id, content_hash) partial unique: a re-ingest of the SAME
-- rendition content is a no-op; a changed hash enqueues anew. The #294
-- active-snapshot suppression is applied by the mint query itself.
CREATE UNIQUE INDEX IF NOT EXISTS ingest_jobs_revision_identity_uq
  ON ingest_jobs (revision_rendition_id, content_hash)
  WHERE intake_kind = 'revision' AND force_rebuild = false;

-- The claim scan must see pending revision jobs by rendition identity.
CREATE INDEX IF NOT EXISTS ingest_jobs_revision_rendition_idx
  ON ingest_jobs (revision_rendition_id)
  WHERE intake_kind = 'revision';

-- intake_kind CHECK: the two lanes are the closed set of the transition.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'ingest_jobs_intake_kind_chk') THEN
    ALTER TABLE ingest_jobs
      ADD CONSTRAINT intake_jobs_intake_kind_chk CHECK (intake_kind IN ('zotero','revision'));
  END IF;
END $$;
