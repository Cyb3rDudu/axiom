-- 0002_revision_identity_active_scope.sql — migrates DBs ledgered at the
-- FIRST strand build of 0001 to the active-scoped, source-keyed identity
-- index (F09 #303, review round 5).
--
-- Why a separate version: the ledger skips an already-applied file, so
-- the reshape cannot live in 0001 — a DB with the 0001 row would never
-- see it, keep the stale status-blind (rendition_id, content_hash)
-- index, and fail EVERY revision intake with 42P10 (no arbiter matching
-- the ON CONFLICT specification) or a raw 23505 on the old name. This
-- file always runs exactly once on those DBs; on fresh DBs (0001 already
-- created the new shape) the DROP+CREATE is an idempotent no-op.
--
-- Safe under the stricter old index: at most one row per identity could
-- exist status-blind, so the narrower predicate always builds.
DROP INDEX IF EXISTS ingest_jobs_revision_identity_uq;
CREATE UNIQUE INDEX IF NOT EXISTS ingest_jobs_revision_identity_uq
  ON ingest_jobs (revision_source_id, revision_rendition_id, content_hash)
  WHERE intake_kind = 'revision' AND force_rebuild = false
    AND status IN ('pending','claimed','processing');
