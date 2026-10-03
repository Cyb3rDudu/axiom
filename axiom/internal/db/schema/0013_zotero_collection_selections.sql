-- 0013 (#166 NACHSCHÄRFUNG): collection-level selection — the PRIMARY choice
-- layer (dudu's methodology lives in collections: Subject Areas + module
-- folders). Document rows stay the fine control WITHIN the collection result
-- (doc-exclude beats collection-include; collection-exclude beats doc-include
-- — the documented cascade). No FK on purpose: collections are re-synced
-- (delete/recreate with the same key) and selections must survive that.
CREATE TABLE IF NOT EXISTS zotero_collection_selections (
  collection_key TEXT PRIMARY KEY,
  mode           TEXT NOT NULL CHECK (mode IN ('included','excluded')),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
