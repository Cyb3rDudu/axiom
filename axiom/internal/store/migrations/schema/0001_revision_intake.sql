-- 0001_revision_intake.sql — the revision intake columns on
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
--
-- Identity dedup scope: (source, rendition, hash) — the rendition key is
-- only unique PER SOURCE (zotero_attachments UNIQUE (source_id,
-- zotero_key)), so the source column is load-bearing against
-- cross-source false dedup. The ACTIVE-status predicate keeps terminal
-- (skipped/failed) rows out of the arbiter: an obsoleted revision job
-- must never block a re-intake after its transient cause resolved
-- (REVISION_REF_UNRESOLVED, ATTACHMENT_NOT_PREFERRED after a preference
-- flip, …) — re-intake mints a FRESH job instead of joining the corpse.

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
-- (attachment_id, content_hash) partial unique WITH the source column and
-- scoped to ACTIVE jobs (see the header note). A re-ingest under a NEW key of the SAME
-- rendition content is a no-op while active; a changed hash enqueues
-- anew; a TERMINAL row never answers the arbiter. The #294
-- active-snapshot suppression is applied by the mint query itself.
--
-- (DBs ledgered at the first strand build carry the earlier
-- status-blind shape of this index — 0002 migrates them; the ledger
-- skips this file there.)
CREATE UNIQUE INDEX IF NOT EXISTS ingest_jobs_revision_identity_uq
  ON ingest_jobs (revision_source_id, revision_rendition_id, content_hash)
  WHERE intake_kind = 'revision' AND force_rebuild = false
    AND status IN ('pending','claimed','processing');

-- intake_kind CHECK: the two lanes are the closed set of the transition.
-- The guard accepts both the corrected and the original (typo'd)
-- constraint name AND filters on the table (the repo migration pattern).
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'ingest_jobs'::regclass
      AND conname IN ('ingest_jobs_intake_kind_chk','intake_jobs_intake_kind_chk')) THEN
    ALTER TABLE ingest_jobs
      ADD CONSTRAINT ingest_jobs_intake_kind_chk CHECK (intake_kind IN ('zotero','revision'));
  END IF;
END $$;
