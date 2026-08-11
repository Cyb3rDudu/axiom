-- 0004_zotero_canonical_integrity.sql
-- Enforce referential integrity for the canonical mirror and reset the
-- canonical cursor so a corrected round of the canonical sync fully repairs
-- and backfills the lossless mirror.
--
-- 0003 was already applied live; additive-only changes here. The canonical
-- cursor reset to 0 forces the next RunCanonical to do a full snapshot (not an
-- incremental delta), which is safe because the corrected code only marks
-- items as deleted when FullSnapshot is true and derives projections from the
-- complete active zotero_items state.

-- Foreign keys from the document/attachment projections to their canonical item.
ALTER TABLE zotero_documents
  DROP CONSTRAINT IF EXISTS fk_zotero_documents_canonical_item,
  ADD CONSTRAINT fk_zotero_documents_canonical_item
    FOREIGN KEY (canonical_item_id) REFERENCES zotero_items(id) ON DELETE CASCADE;

ALTER TABLE zotero_attachments
  DROP CONSTRAINT IF EXISTS fk_zotero_attachments_canonical_item,
  ADD CONSTRAINT fk_zotero_attachments_canonical_item
    FOREIGN KEY (canonical_item_id) REFERENCES zotero_items(id) ON DELETE CASCADE;

-- Memberhip mapping integrity.
ALTER TABLE zotero_item_collections
  DROP CONSTRAINT IF EXISTS fk_item_collections_collection,
  ADD CONSTRAINT fk_item_collections_collection
    FOREIGN KEY (collection_id) REFERENCES zotero_collections(id) ON DELETE CASCADE;

-- Backfill canonical_item_id for existing projection rows from the canonical
-- item mirror (idempotent).
UPDATE zotero_documents d
SET canonical_item_id = i.id
FROM zotero_items i
WHERE i.source_id = d.source_id AND i.zotero_key = d.zotero_key
  AND d.canonical_item_id IS DISTINCT FROM i.id;

UPDATE zotero_attachments a
SET canonical_item_id = i.id
FROM zotero_items i
WHERE i.source_id = a.source_id AND i.zotero_key = a.zotero_key
  AND a.canonical_item_id IS DISTINCT FROM i.id;

-- Controlled reset of the canonical cursor so the corrected code performs a
-- full snapshot and reconciles/backfills the mirror exactly once.
UPDATE zotero_sources SET canonical_last_modified_version = 0;
