-- 0003_library_collection_anchor.sql (F07 review round, SQLite dialect)
-- Collection creations join the anchor ledger: the CHECK widens to
-- record|rendition|collection. SQLite cannot ALTER a CHECK constraint —
-- the standard rebuild (new table, copy, drop, rename) runs inside the
-- migration's transaction (SQLite DDL is transactional, so it is atomic
-- like its PostgreSQL twin).

CREATE TABLE library_provider_anchors_new (
  scope TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('record','rendition','collection')),
  anchor TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  provider_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  PRIMARY KEY (scope, kind, anchor)
);
INSERT INTO library_provider_anchors_new (scope, kind, anchor, provider_id, provider_version, created_at)
  SELECT scope, kind, anchor, provider_id, provider_version, created_at FROM library_provider_anchors;
DROP TABLE library_provider_anchors;
ALTER TABLE library_provider_anchors_new RENAME TO library_provider_anchors;
