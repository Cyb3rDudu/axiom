-- 0001_init.sql (SQLite dialect) — the config.sqlite runtime tables (F13 #307).
-- One file per host installation under the state root. RUNTIME
-- CONFIGURATION ONLY: the two tables below are the complete, closed
-- vocabulary — domain data (Library/Store rows) NEVER lives here; the
-- store refuses files carrying foreign tables at read time
-- (CheckRuntimeOnly — the Fachdaten-never rule with teeth).
--
-- settings: non-secret overrides, key = the canonical AXIOM_* environment
-- name (one key vocabulary across env, file, and --set). Values are
-- stored EXACTLY in their environment spelling; empty values are legal
-- only where the loader reads set-but-empty semantics
-- (AXIOM_OPENSEARCH_URL — the documented drainer disable).
--
-- secret_refs: secret REFERENCES, never values. A row records that a
-- secret key's VALUE is expected from an external source (today: the
-- process environment). The value itself stays in the OS secret store /
-- env — the inspection sonde proves zero secret bytes in file and logs.

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_refs (
  key TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
