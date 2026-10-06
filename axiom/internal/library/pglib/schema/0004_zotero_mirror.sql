-- 0004_zotero_mirror.sql — the Zotero mirror's home on the Library
-- database (#358 mirror plane finalization). The production library
-- database already carries these tables (the DM09 cutover copy); this
-- file exists so FRESH library databases (E2E rides, new deployments)
-- get the same schema — every statement is idempotent against the copy.
-- The definitions mirror the core schema (internal/db 0002/0003/0004/
-- 0012/0013/0014/0023) MINUS every Store-side artifact: no ingest_jobs
-- FKs, no ingest idempotency index — those live on the Store database.

CREATE TABLE IF NOT EXISTS zotero_sources (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  base_url TEXT NOT NULL,
  library_id TEXT NOT NULL DEFAULT 'users/0',
  server_id TEXT,
  schema_version INTEGER,
  last_modified_version BIGINT NOT NULL DEFAULT 0,
  canonical_last_modified_version BIGINT NOT NULL DEFAULT 0,
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
  canonical_item_id UUID,
  citation_class TEXT NOT NULL DEFAULT 'citable'
    CONSTRAINT zotero_documents_citation_class_chk
    CHECK (citation_class IN ('citable','contextual')),
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
  canonical_item_id UUID,
  repair_attempts INT NOT NULL DEFAULT 0,
  preferred BOOLEAN NOT NULL DEFAULT false,
  deleted BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);
CREATE INDEX IF NOT EXISTS zotero_attachments_key_idx
ON zotero_attachments (source_id, zotero_key);

CREATE TABLE IF NOT EXISTS zotero_items (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  source_id UUID NOT NULL REFERENCES zotero_sources(id) ON DELETE CASCADE,
  zotero_key TEXT NOT NULL,
  zotero_version BIGINT NOT NULL,
  item_type TEXT NOT NULL,
  parent_key TEXT,
  raw_envelope JSONB NOT NULL,
  raw_data JSONB NOT NULL,
  deleted BOOLEAN NOT NULL DEFAULT false,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);
CREATE INDEX IF NOT EXISTS zotero_items_parent_idx ON zotero_items (source_id, parent_key);
CREATE INDEX IF NOT EXISTS zotero_items_itemtype_idx ON zotero_items (source_id, item_type);

CREATE TABLE IF NOT EXISTS zotero_collections (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  source_id UUID NOT NULL REFERENCES zotero_sources(id) ON DELETE CASCADE,
  zotero_key TEXT NOT NULL,
  name TEXT NOT NULL,
  parent_key TEXT,
  raw_envelope JSONB NOT NULL,
  deleted BOOLEAN NOT NULL DEFAULT false,
  synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_id, zotero_key)
);
CREATE INDEX IF NOT EXISTS zotero_collections_parent_idx ON zotero_collections (source_id, parent_key);

CREATE TABLE IF NOT EXISTS zotero_item_collections (
  item_id UUID NOT NULL REFERENCES zotero_items(id) ON DELETE CASCADE,
  collection_id UUID NOT NULL REFERENCES zotero_collections(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, collection_id)
);

CREATE TABLE IF NOT EXISTS zotero_selections (
  document_id UUID PRIMARY KEY REFERENCES zotero_documents(id) ON DELETE CASCADE,
  mode TEXT NOT NULL CHECK (mode IN ('included','excluded')),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS zotero_collection_selections (
  collection_key TEXT PRIMARY KEY,
  mode TEXT NOT NULL CHECK (mode IN ('included','excluded')),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Repair track (Library-owned since F08): the enum is guarded because
-- PostgreSQL has no CREATE TYPE IF NOT EXISTS.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'repair_status') THEN
    CREATE TYPE repair_status AS ENUM (
      'rejected', 'queued', 'in_repair', 'healed', 'failed', 'blocked_for_dudu'
    );
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS repair_cases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  attachment_id uuid REFERENCES zotero_attachments(id),
  document_id uuid REFERENCES zotero_documents(id),
  status repair_status NOT NULL DEFAULT 'rejected',
  attempts int NOT NULL DEFAULT 0,
  suspicion_class text NOT NULL DEFAULT '',
  analysis jsonb NOT NULL DEFAULT '{}',
  plan jsonb NOT NULL DEFAULT '{}',
  plan_version int NOT NULL DEFAULT 0,
  verify_score numeric NOT NULL DEFAULT 0,
  verify_contradictions int NOT NULL DEFAULT 0,
  verdict text NOT NULL DEFAULT '',
  blocked_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS repair_cases_one_open_per_attachment
  ON repair_cases (attachment_id)
  WHERE status IN ('rejected', 'queued', 'in_repair');
CREATE INDEX IF NOT EXISTS repair_cases_status_idx ON repair_cases (status, created_at);

-- Canonical back-references (core 0004 shape). Assumption: the Library
-- database either carries the core schema underneath (the cutover copy —
-- core 0004 already added these FKs, making this block a no-op) or is
-- migrated pglib-only (greenfield); the block owns the FKs in the latter
-- case. A database recorded at 0004 BEFORE this block landed keeps
-- whatever constraint state it had — pglib-alone greenfields from that
-- era lack the FKs (the parity IT pins the current shape).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_zotero_documents_canonical_item' AND conrelid = 'zotero_documents'::regclass) THEN
    ALTER TABLE zotero_documents
      ADD CONSTRAINT fk_zotero_documents_canonical_item
      FOREIGN KEY (canonical_item_id) REFERENCES zotero_items(id) ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_zotero_attachments_canonical_item' AND conrelid = 'zotero_attachments'::regclass) THEN
    ALTER TABLE zotero_attachments
      ADD CONSTRAINT fk_zotero_attachments_canonical_item
      FOREIGN KEY (canonical_item_id) REFERENCES zotero_items(id) ON DELETE CASCADE;
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS zotero_write_audit (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid REFERENCES repair_cases(id),
  attachment_id uuid REFERENCES zotero_attachments(id),
  action text NOT NULL,
  detail jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
