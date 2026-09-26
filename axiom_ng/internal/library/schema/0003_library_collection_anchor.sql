-- 0003_library_collection_anchor.sql
-- F07 review round (#301): collection creations join the anchor ledger.
-- The CHECK from 0002 only knew record|rendition; a crash between the
-- Zotero collection POST and the audit append left a mutation whose
-- audit row no retry ever re-emitted (ResolvePath resolves the existing
-- segment and moves on). With kind 'collection', the retry's
-- existing-segment branch ADOPTS the torn segment into the ledger and
-- appends the recovered audit row.

ALTER TABLE library_provider_anchors DROP CONSTRAINT IF EXISTS library_provider_anchors_kind_check;
ALTER TABLE library_provider_anchors ADD CONSTRAINT library_provider_anchors_kind_check
  CHECK (kind IN ('record','rendition','collection'));
