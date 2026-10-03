-- 0002_library_writer_lease.sql (F07 #301, SQLite dialect, F12 #306)
-- The single-writer declaration and the provider adapter's durable
-- bookkeeping. Same shape as the PostgreSQL twin; cross-process safety
-- rides on the IMMEDIATE transaction the engine wraps every lease
-- statement in (see lease acquire), plus the file's busy_timeout — no
-- advisory locks exist in SQLite and none are needed.

CREATE TABLE IF NOT EXISTS library_writer_leases (
  scope TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS library_write_audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  scope TEXT NOT NULL,
  operation TEXT NOT NULL,
  anchor TEXT NOT NULL,
  provider_ref TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL,
  readback TEXT NOT NULL DEFAULT '{}',
  at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS library_write_audit_scope_at
  ON library_write_audit (scope, at);

CREATE TABLE IF NOT EXISTS library_provider_anchors (
  scope TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('record','rendition')),
  anchor TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  provider_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  PRIMARY KEY (scope, kind, anchor)
);
