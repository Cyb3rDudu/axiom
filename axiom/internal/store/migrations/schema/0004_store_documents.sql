-- 0004_store_documents.sql — the Store's own document metadata projection
-- (#358 mirror plane finalization): the Zotero mirror (zotero_* tables)
-- moves to the Library database; everything the Store needs to run
-- processing is denormalized HERE, at intake time, per rendition.
--
-- Reference model (ADR 0002): identities instead of joins. document_id/
-- attachment_id are opaque identity columns (the cross-component FKs were
-- already dropped by 0003); rendition_key + content_hash is the boundary
-- identity against the Library. One row per rendition (attachment), the
-- preferred processable one of a record; metadata columns carry the
-- revision contract's Bibliography shape (author strings, tag strings),
-- not the mirror's raw Zotero shapes.
--
-- repair_linked is the retention guard's repair truth: set once by the
-- Library-side repair track when a repair case references the rendition
-- (the exported repo seam — the sanctioned Library→Store write pattern);
-- never cleared, mirroring the old "any repair_cases row ever" guard.
--
-- The Store's legacy zotero_* tables stay as the frozen archive (drop is
-- documented post-soak follow-up); the bestand backfill below mints
-- projection rows from the archive so search hydration, retention anchors
-- and in-flight claims keep working from the first boot after the switch.
-- The next sync re-offers every document and overwrites the backfilled
-- rows with live mirror truth.

CREATE TABLE IF NOT EXISTS store_documents (
  document_id   UUID NOT NULL,
  attachment_id UUID NOT NULL PRIMARY KEY,
  source_id     UUID NOT NULL,
  server_id     TEXT NOT NULL DEFAULT '',
  record_key    TEXT NOT NULL,
  rendition_key TEXT NOT NULL,
  source_version BIGINT NOT NULL DEFAULT 0,
  content_hash  TEXT,
  title         TEXT NOT NULL DEFAULT '',
  creators      JSONB NOT NULL DEFAULT '[]',
  publication_year INTEGER,
  publisher     TEXT NOT NULL DEFAULT '',
  language      TEXT NOT NULL DEFAULT '',
  tags          JSONB NOT NULL DEFAULT '[]',
  citation_class TEXT NOT NULL DEFAULT 'citable'
    CONSTRAINT store_documents_citation_class_chk
    CHECK (citation_class IN ('citable','contextual')),
  content_type  TEXT NOT NULL DEFAULT '',
  item_type     TEXT NOT NULL DEFAULT '',
  filename      TEXT NOT NULL DEFAULT '',
  local_path    TEXT NOT NULL DEFAULT '',
  file_size     BIGINT,
  mtime_ms      BIGINT,
  link_mode     TEXT NOT NULL DEFAULT 'imported_file',
  preferred     BOOLEAN NOT NULL DEFAULT true,
  deleted       BOOLEAN NOT NULL DEFAULT false,
  repair_linked BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS store_documents_source_rendition_uq
  ON store_documents (source_id, rendition_key);
CREATE INDEX IF NOT EXISTS store_documents_document_idx
  ON store_documents (document_id);

-- Narrow the legacy idempotency index to the zotero lane. The original
-- lane-blind shape (attachment_id, content_hash) WHERE force_rebuild=false
-- collides with revision-lane claims the moment the sync drives revision
-- intake for the whole corpus: a re-intake after a terminal revision job
-- sets the same (attachment_id, content_hash) at claim and dies on 23505,
-- poisoning the FIFO head forever (the documented escape noted in the
-- revision claim). Legacy rows keep their guarantee; revision identity
-- has its own ACTIVE-scoped arbiter (0001/0002).
DROP INDEX IF EXISTS ingest_jobs_idempotency_idx;
CREATE UNIQUE INDEX ingest_jobs_idempotency_idx
  ON ingest_jobs (attachment_id, content_hash)
  WHERE force_rebuild = false AND intake_kind = 'zotero';

-- Bestand backfill from the frozen archive (no-op on fresh databases:
-- the core schema creates the zotero_* tables empty there). Only the
-- preferred, non-deleted attachment of a live document is a rendition the
-- Store tracks; creators/tags translate from the mirror JSONB shapes to
-- the plain-string bibliography shapes the projection carries.
INSERT INTO store_documents (
  document_id, attachment_id, source_id, server_id,
  record_key, rendition_key, source_version, content_hash,
  title, creators, publication_year, publisher, language, tags,
  citation_class, content_type, item_type, filename, local_path,
  file_size, mtime_ms, link_mode, preferred, deleted
)
SELECT
  d.id, a.id, d.source_id,
  COALESCE(s.server_id, ''),
  d.zotero_key, a.zotero_key, a.zotero_version, a.content_hash,
  d.title,
  (SELECT COALESCE(jsonb_agg(
     CASE
       WHEN COALESCE(c->>'name', '') <> '' THEN c->>'name'
       ELSE btrim(COALESCE(c->>'firstName','') || ' ' || COALESCE(c->>'lastName',''))
     END), '[]'::jsonb)
   FROM jsonb_array_elements(CASE WHEN jsonb_typeof(d.creators)='array' THEN d.creators ELSE '[]'::jsonb END) AS c
   WHERE btrim(COALESCE(c->>'name','') || ' ' || COALESCE(c->>'firstName','') || ' ' || COALESCE(c->>'lastName','')) <> ''),
  d.publication_year, COALESCE(d.publisher,''), COALESCE(d.language,''),
  (SELECT COALESCE(jsonb_agg(t->>'tag'), '[]'::jsonb)
   FROM jsonb_array_elements(CASE WHEN jsonb_typeof(d.tags)='array' THEN d.tags ELSE '[]'::jsonb END) AS t
   WHERE COALESCE(t->>'tag','') <> ''),
  COALESCE(d.citation_class, 'citable'),
  COALESCE(a.content_type,''), COALESCE(d.item_type,''),
  COALESCE(a.filename,''), COALESCE(a.local_path,''),
  a.file_size, a.mtime_ms, COALESCE(a.link_mode,'imported_file'),
  true, false
FROM zotero_documents d
JOIN zotero_attachments a ON a.document_id = d.id AND a.preferred AND NOT a.deleted
JOIN zotero_sources s ON s.id = d.source_id
WHERE NOT d.deleted
ON CONFLICT (attachment_id) DO NOTHING;

-- Historical repair cases keep their retention protection: the pre-#358
-- guard counted ANY repair_cases row for the attachment; the successor
-- flag is set-once, so the backfill seeds it for every attachment with a
-- case in the archive.
UPDATE store_documents p SET repair_linked=true
WHERE EXISTS (SELECT 1 FROM repair_cases rc WHERE rc.attachment_id = p.attachment_id);
