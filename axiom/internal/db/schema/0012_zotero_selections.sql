-- 0012 (#166): client-controlled ingest selection. The projection stays a
-- full mirror of the Zotero library; only JOB CREATION is gated. No row =
-- default behavior (everything is selected — today's semantics).
CREATE TABLE IF NOT EXISTS zotero_selections (
  document_id UUID PRIMARY KEY REFERENCES zotero_documents(id) ON DELETE CASCADE,
  -- 'included' is explicit bookkeeping (same effect as no row); 'excluded'
  -- holds the document: no ingest job is created, existing chunks stay
  -- searchable (removal/tombstone is a different feature, #166 Nicht-Ziel).
  mode        TEXT NOT NULL CHECK (mode IN ('included','excluded')),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
