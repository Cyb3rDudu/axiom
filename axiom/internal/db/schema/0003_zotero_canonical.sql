-- 0003_zotero_canonical.sql
-- Lossless canonical Zotero mirror.
--
-- zotero_items is the source of truth for every Zotero object (parents,
-- attachments, notes, annotations). It keeps the full item envelope as JSONB
-- (raw_envelope) and the item's data object (raw_data), plus a small set of
-- queryable columns. zotero_documents and zotero_attachments remain
-- normalised projections fed from this table. All rows reference the existing
-- zotero_sources row.
--
-- A SEPARATE cursor (canonical_last_modified_version) drives the canonical
-- sync, independent of the existing document/attachment cursor which already
-- sits around 181. The canonical cursor defaults to 0 so the first canonical
-- sync is a guaranteed full sync that snapshots every item.

ALTER TABLE zotero_sources
  ADD COLUMN IF NOT EXISTS canonical_last_modified_version BIGINT NOT NULL DEFAULT 0;

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

-- Collections with their full raw envelope and parent hierarchy.
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

-- Optional many-to-many mapping (queried from item data.collections keys).
CREATE TABLE IF NOT EXISTS zotero_item_collections (
  item_id UUID NOT NULL REFERENCES zotero_items(id) ON DELETE CASCADE,
  collection_id UUID NOT NULL REFERENCES zotero_collections(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, collection_id)
);

-- Back-reference from normalised projections to their canonical item.
ALTER TABLE zotero_documents ADD COLUMN IF NOT EXISTS canonical_item_id UUID;
ALTER TABLE zotero_attachments ADD COLUMN IF NOT EXISTS canonical_item_id UUID;
