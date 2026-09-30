-- 0002_library_writer_lease.sql
-- F07 (#301): the single-writer declaration and the provider adapter's
-- durable bookkeeping. Additive only — no F06 table is touched.
--
-- The writer lease is the CROSS-PROCESS single-writer guard: a second
-- write-capable Library instance against the same provider scope is
-- rejected at start (the lease row names the live owner); liveness is a
-- renewed heartbeat, so a crashed writer's lease is taken over after the
-- TTL. Zotero's own optimistic versioning remains the last line behind it.

CREATE TABLE IF NOT EXISTS library_writer_leases (
  scope TEXT PRIMARY KEY,             -- provider-scoped identity, e.g. zotero|<base>|<libraryID>
  owner TEXT NOT NULL,                -- writer identity (host:pid:boot-nonce)
  acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per provider mutation, appended AFTER its readback succeeded
-- (inkl. resolver-entitled record writes — resolver decisions themselves
-- live in library_metadata_provenance since F06). The audit is the write
-- witness: mutations and audit rows are 1:1.
CREATE TABLE IF NOT EXISTS library_write_audit (
  id BIGSERIAL PRIMARY KEY,
  scope TEXT NOT NULL,
  operation TEXT NOT NULL,            -- ensure_record | ensure_rendition | ensure_membership | create_collection (future ops land with their emitters)
  anchor TEXT NOT NULL,               -- the idempotency anchor the mutation keyed on
  provider_ref TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL,              -- created | reused | changed | adopted | removed
  readback JSONB NOT NULL DEFAULT '{}', -- what the readback observed
  at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS library_write_audit_scope_at
  ON library_write_audit (scope, at);

-- The adapter's idempotency ledger: Zotero items carry no external key,
-- so the adapter records the anchor→provider-id mapping durably. A
-- crash between the Zotero write and this row is closed by the axiom-*
-- tag on the item (tag search re-finds it); the row is the fast path.
CREATE TABLE IF NOT EXISTS library_provider_anchors (
  scope TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('record','rendition')),
  anchor TEXT NOT NULL,               -- record: external key; rendition: parent|sha256
  provider_id TEXT NOT NULL,
  provider_version BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (scope, kind, anchor)
);
