-- 0023 (#255): contextual source class.
--
-- zotero_documents.citation_class — the two voices of the corpus:
--   citable   literature (default; citation ladder applies)
--   contextual lecture slides/transcripts — searchable and chat-retrievable
--             at FULL rank, but never a citation target (provenance line
--             instead; "Folie N" for slide pages)
-- Recomputed on EVERY canonical sync from collection membership
-- (AXIOM_CONTEXTUAL_COLLECTIONS, resolved by path to zotero_key so renames
-- survive) plus a tag override (AXIOM_CONTEXTUAL_TAGS; only ever forces
-- contextual, never citable). Zotero stays the source of truth; the column
-- is a projection, never hand-edited.

ALTER TABLE zotero_documents
  ADD COLUMN IF NOT EXISTS citation_class TEXT NOT NULL DEFAULT 'citable'
  CONSTRAINT zotero_documents_citation_class_chk
  CHECK (citation_class IN ('citable','contextual'));
