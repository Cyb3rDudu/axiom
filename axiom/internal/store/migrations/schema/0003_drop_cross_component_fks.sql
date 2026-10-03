-- 0003_drop_cross_component_fks.sql — DM06 #315: removes the FIVE
-- cross-component foreign keys (ingest_jobs.source/document/attachment_id,
-- processing_snapshots.document/attachment_id → the zotero_* mirror),
-- the last physical knot between the Store and Library schemas. After
-- this file, the components are separated at the database level; the
-- reference guarantee moves to the contract layer (revision intake:
-- REVISION_REF_UNRESOLVED obsolescence; legacy claim: PARENT_REMOVED /
-- ATTACHMENT_REMOVED). Library deletions stop cascade-deleting Store
-- rows — retention becomes explicit, per the DM topology.
--
-- Teeth, in order:
--   1. Orphan audit BEFORE any drop: a non-NULL reference pointing at a
--      missing Library row aborts the migration with a full diagnosis
--      (all broken references, with counts) — never a silent drop over
--      orphans. NULL references are legitimate (revision-lane jobs
--      resolve their FKs at claim time).
--   2. Drops by EXACT constraint name, no CASCADE: nothing unnamed can
--      fall. Idempotent per se (IF EXISTS) for the bootstrap-repair
--      re-run (ledger wiped, schema already correct).
--   3. Postcondition AFTER the drops: exactly zero foreign keys remain
--      from the two Store tables onto the three Library tables — if a
--      sixth cross-component FK appears (core drift), the migration
--      aborts instead of leaving a half-separated topology behind.

DO $$
DECLARE
  r record;
  broken text := '';
BEGIN
  FOR r IN
    SELECT 'ingest_jobs.source_id → zotero_sources' AS ref, count(*) AS orphans
      FROM ingest_jobs j
      WHERE j.source_id IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM zotero_sources t WHERE t.id = j.source_id)
    UNION ALL
    SELECT 'ingest_jobs.document_id → zotero_documents', count(*)
      FROM ingest_jobs j
      WHERE j.document_id IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM zotero_documents t WHERE t.id = j.document_id)
    UNION ALL
    SELECT 'ingest_jobs.attachment_id → zotero_attachments', count(*)
      FROM ingest_jobs j
      WHERE j.attachment_id IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM zotero_attachments t WHERE t.id = j.attachment_id)
    UNION ALL
    SELECT 'processing_snapshots.document_id → zotero_documents', count(*)
      FROM processing_snapshots s
      WHERE s.document_id IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM zotero_documents t WHERE t.id = s.document_id)
    UNION ALL
    SELECT 'processing_snapshots.attachment_id → zotero_attachments', count(*)
      FROM processing_snapshots s
      WHERE s.attachment_id IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM zotero_attachments t WHERE t.id = s.attachment_id)
  LOOP
    IF r.orphans > 0 THEN
      broken := broken || format('%s: %s orphans; ', r.ref, r.orphans);
    END IF;
  END LOOP;
  IF broken <> '' THEN
    RAISE EXCEPTION 'orphan guard refused to drop the cross-component FKs — resolve orphans first: %', broken;
  END IF;
END $$;

ALTER TABLE ingest_jobs        DROP CONSTRAINT IF EXISTS fk_ingest_jobs_source;
ALTER TABLE ingest_jobs        DROP CONSTRAINT IF EXISTS fk_ingest_jobs_document;
ALTER TABLE ingest_jobs        DROP CONSTRAINT IF EXISTS fk_ingest_jobs_attachment;
ALTER TABLE processing_snapshots DROP CONSTRAINT IF EXISTS processing_snapshots_document_id_fkey;
ALTER TABLE processing_snapshots DROP CONSTRAINT IF EXISTS processing_snapshots_attachment_id_fkey;

DO $$
DECLARE
  leftover text := '';
  r record;
BEGIN
  FOR r IN
    SELECT conrelid::regclass::text || '.' || a.attname || ' → ' || confrelid::regclass::text AS ref
      FROM pg_constraint c
      JOIN LATERAL unnest(c.conkey) AS k(attnum) ON true
      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
      WHERE c.contype = 'f'
        AND c.conrelid IN ('ingest_jobs'::regclass, 'processing_snapshots'::regclass)
        AND c.confrelid IN ('zotero_sources'::regclass, 'zotero_documents'::regclass, 'zotero_attachments'::regclass)
  LOOP
    leftover := leftover || r.ref || '; ';
  END LOOP;
  IF leftover <> '' THEN
    RAISE EXCEPTION 'cross-component FKs remain after the drop (inventory drift beyond the allowlisted five): %', leftover;
  END IF;
END $$;
