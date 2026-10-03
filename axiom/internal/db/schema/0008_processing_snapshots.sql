-- 0008_processing_snapshots.sql
-- Processing-snapshot identity, activation and provenance (Gate 4).
--
-- A processing snapshot is the immutable, durable record produced by ONE
-- successful processor run. Snapshot IDENTITY (work-order §10.1) is the tuple
--   (attachment_id, content_hash, processor_name, processor_version, profile_hash)
-- so replaying the same completed result returns the existing snapshot instead
-- of duplicating work. A force rebuild creates a new snapshot row under a fresh
-- identity/generation only.
--
-- Only ONE snapshot is active per (document_id, attachment_id, profile_hash)
-- scope (§10.2). Switching the active snapshot happens in the SAME transaction
-- that inserts the replacement rows (Gate 4 persists chunks/embeddings/entities
-- and flips the active flag in one commit). Old snapshots stay immutable for
-- audit/recovery; a separate retention policy may remove them later.
--
-- pgvector is required for dense embeddings (0009); the extension is created
-- here, idempotently, so 0009 can rely on it. All tables are additive
-- (IF NOT EXISTS) so an already-applied upgrade database migrates safely.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS processing_snapshots (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  -- Snapshot identity (§10.1).
  attachment_id     UUID NOT NULL,
  content_hash      TEXT NOT NULL,
  processor_name    TEXT NOT NULL,
  processor_version TEXT NOT NULL,
  profile_hash      TEXT NOT NULL,
  -- Activation scope: one active snapshot per (document_id, attachment_id, profile_hash).
  document_id       UUID NOT NULL,
  -- Provenance / manifest (contract §10 processor block + manifest).
  profile           TEXT NOT NULL,
  models            JSONB NOT NULL DEFAULT '{}'::jsonb,
  manifest          JSONB NOT NULL DEFAULT '{}'::jsonb,
  warnings          JSONB NOT NULL DEFAULT '[]'::jsonb,
  source_verified   BOOLEAN NOT NULL DEFAULT false,
  -- The ingest job that produced this snapshot (for traceability; not part of identity).
  ingest_job_id     UUID,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Activation flag (§10.2). Exactly one row per scope has this set to true;
  -- the persist transaction deactivates the previous active row and activates
  -- the new one atomically.
  active            BOOLEAN NOT NULL DEFAULT false,
  -- Generation distinguishes explicit force-rebuild snapshots that share the
  -- identity tuple but are deliberately superseded. Defaults to 1.
  generation        INTEGER NOT NULL DEFAULT 1
);

-- Identity uniqueness: replaying a completed result must hit this row (§10.1).
CREATE UNIQUE INDEX IF NOT EXISTS processing_snapshots_identity_uq
ON processing_snapshots (attachment_id, content_hash, processor_name, processor_version, profile_hash);

-- At most one ACTIVE snapshot per scope. A partial unique index enforces this
-- invariant at the DB level regardless of concurrency (§10.2).
CREATE UNIQUE INDEX IF NOT EXISTS processing_snapshots_active_scope_uq
ON processing_snapshots (document_id, attachment_id, profile_hash)
WHERE active = true;

-- Fast lookup of the active snapshot for a document/attachment.
CREATE INDEX IF NOT EXISTS processing_snapshots_scope_idx
ON processing_snapshots (document_id, attachment_id, profile_hash, active);

-- Foreign keys to the Zotero mirror are added separately (and guarded) so an
-- upgrade database that already has the columns migrates without re-resolving.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.table_constraints
    WHERE constraint_name = 'processing_snapshots_attachment_id_fkey'
      AND table_name = 'processing_snapshots'
  ) THEN
    ALTER TABLE processing_snapshots
      ADD CONSTRAINT processing_snapshots_attachment_id_fkey
      FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.table_constraints
    WHERE constraint_name = 'processing_snapshots_document_id_fkey'
      AND table_name = 'processing_snapshots'
  ) THEN
    ALTER TABLE processing_snapshots
      ADD CONSTRAINT processing_snapshots_document_id_fkey
      FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE;
  END IF;
END $$;

-- Durable derived artifacts with VERIFIED digests (contract §13). Original
-- PDF/EPUB files are NEVER stored here (source files stay in Zotero). An
-- artifact row exists only after axiom-ng has fetched, hashed and length-checked
-- the processor's bytes (Gate 4 verifies media_type/size_bytes/sha256 before
-- committing).
CREATE TABLE IF NOT EXISTS processing_artifacts (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id   UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  -- Job-local ref echoed from the processor result (contract §10 'ref'); unique
  -- within a snapshot.
  ref           TEXT NOT NULL,
  kind          TEXT NOT NULL,
  media_type    TEXT NOT NULL,
  sha256        TEXT NOT NULL,
  size_bytes    BIGINT NOT NULL,
  retention     TEXT NOT NULL DEFAULT 'durable',
  -- Final durable path under AXIOMNG_ARTIFACT_ROOT (staged then atomically renamed,
  -- same filesystem; crash-safe cleanup removes unreferenced staging files).
  storage_path  TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (snapshot_id, ref)
);

CREATE INDEX IF NOT EXISTS processing_artifacts_snapshot_idx
ON processing_artifacts (snapshot_id);
