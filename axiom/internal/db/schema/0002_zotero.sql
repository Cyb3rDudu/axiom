-- 0002_zotero.sql
-- Zotero mirror tables and the foreign keys that tie the ingest queue to a
-- concrete Zotero library, document and attachment.
--
-- Zotero stays the source of truth for documents, metadata, tags and
-- collections. axiom-ng mirrors a local library here to decouple processing
-- from the live Zotero database.

CREATE TABLE IF NOT EXISTS zotero_sources (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  base_url TEXT NOT NULL,
  library_id TEXT NOT NULL DEFAULT 'users/0',
  server_id TEXT,
  schema_version INTEGER,
  last_modified_version BIGINT NOT NULL DEFAULT 0,
  last_sync_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (base_url, library_id)
);

CREATE TABLE IF NOT EXISTS zotero_documents (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  source_id UUID NOT NULL REFERENCES zotero_sources(id) ON DELETE CASCADE,
  zotero_key TEXT NOT NULL,
  zotero_version BIGINT NOT NULL,
  item_type TEXT NOT NULL,
  title TEXT NOT NULL,
  creators JSONB NOT NULL DEFAULT '[]',
  abstract_note TEXT,
  publication_year INTEGER,
  publication_date TEXT,
  publisher TEXT,
  isbn TEXT,
  doi TEXT,
  url TEXT,
  language TEXT,
  metadata JSONB NOT NULL DEFAULT '{}',
  tags JSONB NOT NULL DEFAULT '[]',
  collections JSONB NOT NULL DEFAULT '[]',
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);

CREATE TABLE IF NOT EXISTS zotero_attachments (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  source_id UUID NOT NULL REFERENCES zotero_sources(id) ON DELETE CASCADE,
  document_id UUID NOT NULL REFERENCES zotero_documents(id) ON DELETE CASCADE,
  zotero_key TEXT NOT NULL,
  zotero_version BIGINT NOT NULL,
  parent_zotero_key TEXT NOT NULL,
  link_mode TEXT NOT NULL,
  content_type TEXT NOT NULL,
  filename TEXT NOT NULL,
  file_uri TEXT,
  local_path TEXT,
  content_hash TEXT,
  file_size BIGINT,
  mtime_ms BIGINT,
  preferred BOOLEAN NOT NULL DEFAULT false,
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);

-- Tie the ingest queue to the mirrored Zotero objects.
ALTER TABLE ingest_jobs
  ADD COLUMN IF NOT EXISTS source_id UUID,
  ADD COLUMN IF NOT EXISTS document_id UUID,
  ADD COLUMN IF NOT EXISTS attachment_id UUID;

ALTER TABLE ingest_jobs
  DROP CONSTRAINT IF EXISTS fk_ingest_jobs_source,
  ADD CONSTRAINT fk_ingest_jobs_source
    FOREIGN KEY (source_id) REFERENCES zotero_sources(id) ON DELETE CASCADE;

ALTER TABLE ingest_jobs
  DROP CONSTRAINT IF EXISTS fk_ingest_jobs_document,
  ADD CONSTRAINT fk_ingest_jobs_document
    FOREIGN KEY (document_id) REFERENCES zotero_documents(id) ON DELETE CASCADE;

ALTER TABLE ingest_jobs
  DROP CONSTRAINT IF EXISTS fk_ingest_jobs_attachment,
  ADD CONSTRAINT fk_ingest_jobs_attachment
    FOREIGN KEY (attachment_id) REFERENCES zotero_attachments(id) ON DELETE CASCADE;

-- Re-ingesting the same attachment with the same content hash is a no-op
-- unless force_rebuild is true.
DROP INDEX IF EXISTS ingest_jobs_idempotency_idx;
CREATE UNIQUE INDEX ingest_jobs_idempotency_idx
ON ingest_jobs (attachment_id, content_hash)
WHERE force_rebuild = false;

-- Look up attachments quickly by their Zotero key.
CREATE INDEX IF NOT EXISTS zotero_attachments_key_idx
ON zotero_attachments (source_id, zotero_key);
