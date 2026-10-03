-- 0001_library.sql (SQLite dialect — see package doc for the type-mapping rules)
-- Library component tables (F06 #300), SQLite dialect (F12 #306).
-- Same tables, same columns, same constraints as the PostgreSQL set
-- (internal/library/pglib/schema) under SQLite's type rules:
--   UUID -> TEXT (canonical lowercase, app-minted)
--   JSONB -> TEXT (canonical JSON)
--   TIMESTAMPTZ -> TEXT (fixed "2006-01-02T15:04:05.000000Z", UTC,
--                 microsecond-aligned — lexicographic order equals
--                 chronological order, the DM03 form)
--   BIGSERIAL -> INTEGER PRIMARY KEY AUTOINCREMENT
--   DOUBLE PRECISION -> REAL, BOOLEAN -> INTEGER 0/1
-- Own ledger (library_schema_migrations), own file: library.sqlite is
-- NEVER attached to another database and never shares tables with the
-- Store component.

CREATE TABLE IF NOT EXISTS library_imports (
  import_id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  payload_hash TEXT NOT NULL,
  record_type TEXT NOT NULL,
  request_json TEXT NOT NULL,
  status TEXT NOT NULL,
  staging_sha256 TEXT NOT NULL,
  staging_size INTEGER NOT NULL,
  media_type TEXT NOT NULL,
  failure_code TEXT,
  failure_message TEXT,
  record_provider_id TEXT,
  rendition_provider_id TEXT,
  collection_provider_id TEXT,
  record_id TEXT,
  rendition_id TEXT,
  revision_id INTEGER,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS library_import_events (
  import_id TEXT NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  kind TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '{}',
  at TEXT NOT NULL,
  PRIMARY KEY (import_id, seq)
);

CREATE TABLE IF NOT EXISTS library_import_steps (
  import_id TEXT NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  step TEXT NOT NULL,
  state TEXT NOT NULL,
  provider_ref TEXT,
  detail TEXT NOT NULL DEFAULT '{}',
  attempts INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (import_id, step)
);

CREATE TABLE IF NOT EXISTS library_external_identifiers (
  kind TEXT NOT NULL CHECK (kind IN ('doi','isbn')),
  normalized_value TEXT NOT NULL,
  record_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (kind, normalized_value)
);

CREATE TABLE IF NOT EXISTS library_metadata_provenance (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  import_id TEXT NOT NULL REFERENCES library_imports(import_id) ON DELETE CASCADE,
  field TEXT NOT NULL,
  source TEXT NOT NULL,
  resolver_version TEXT NOT NULL DEFAULT '',
  confidence REAL NOT NULL DEFAULT 1.0,
  applied INTEGER NOT NULL,
  value TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS library_metadata_provenance_import
  ON library_metadata_provenance (import_id, field);

CREATE TABLE IF NOT EXISTS library_source_revisions (
  source_id TEXT NOT NULL,
  record_id TEXT NOT NULL,
  rendition_id TEXT NOT NULL DEFAULT '',
  revision_id INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  media_type TEXT NOT NULL,
  bibliography TEXT NOT NULL,
  locator_capabilities TEXT NOT NULL,
  content_ticket TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT 'import',
  created_at TEXT NOT NULL,
  PRIMARY KEY (source_id, record_id, rendition_id, revision_id)
);
CREATE INDEX IF NOT EXISTS library_source_revisions_latest
  ON library_source_revisions (source_id, record_id, rendition_id, revision_id DESC);
