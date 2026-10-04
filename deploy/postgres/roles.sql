-- roles.sql — DM07 #316: the component runtime roles and their grants.
--
-- Pure SQL (no psql meta-commands): the DM07 drill executes this very
-- file (axiom/internal/composition/roles_drill_it_test.go), so the
-- script and the tested grants cannot drift. Run it with any client:
--
--   psql -v ON_ERROR_STOP=1 -f deploy/postgres/roles.sql
--
-- THE MODEL
--   axiom_library  the Library component's runtime role: DML on the
--                  Library-owned tables (the library_* set, the Zotero
--                  mirror zotero_*, and the repair track's tables —
--                  repair_cases, zotero_write_audit are Library-owned
--                  per the DM04 boundary).
--   axiom_store    the Store component's runtime role: DML on the
--                  store tables (queue, snapshots, chunks, KG read
--                  model, outbox) and its migration ledgers.
--   Runtime roles are DML-ONLY: no CREATE/ALTER (DDL is window work —
--   migrations run as the admin/deployer role in a maintenance window;
--   boot-time ledger checks no-op on existing tables, so a component
--   boots clean as its role once the schema is current). No cross-DB
--   access: each component database revokes CONNECT from PUBLIC and
--   grants it to the component roles only. Cross-component queries
--   fail at the role level — the least-privilege teeth the DM07 drill
--   proves.
--
-- PASSWORDS NEVER LIVE HERE: the roles are created LOGIN-capable but
-- passwordless; the cutover window sets passwords via
--   ALTER ROLE axiom_library PASSWORD '…'   (administrator, out of band)
-- and the dev drill does the same with its throwaway passwords. No
-- credential bytes in this repo, ever.
--
-- WHEN/WHERE TO RUN
--   Dev mirrors and drills: any time, as superuser/admin. The
--   reference/production database: ONLY in the DM09 cutover window,
--   together with the administrator (explicit project decision — the
--   interim keeps running on the single DSN; the component DSNs flip
--   in the same window).
--
--   On the interim shared database BOTH roles receive CONNECT (two
--   pools, one database); after the physical split each component
--   database runs this script against its own database. Idempotent:
--   re-running against a provisioned database changes nothing.
--
--   Roles are CLUSTER-GLOBAL: a drill run leaves the two roles (with
--   throwaway passwords) behind on the server — disposable in CI
--   containers; on shared dev servers clean up with
--   DROP ROLE axiom_library, axiom_store;
--
-- TABLE LISTS ARE THE CONTRACT — the sets are frozen by the DM06 FK
-- drop and the DM04 ownership boundary. A new table ships with a
-- migration and a grant line HERE (window work reviews both together;
-- there is deliberately no blanket default-privilege grant that would
-- silently hand future tables to the wrong component).

-- 1. The roles (idempotent create; no passwords here — see header).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'axiom_library') THEN
    EXECUTE format('CREATE ROLE %I LOGIN', 'axiom_library');
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'axiom_store') THEN
    EXECUTE format('CREATE ROLE %I LOGIN', 'axiom_store');
  END IF;
END $$;

-- 2. Database access: PUBLIC loses CONNECT; the component roles get
--    it (current_database() keeps the script shape-independent).
DO $$
DECLARE db text := current_database();
BEGIN
  EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', db);
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO axiom_library, axiom_store', db);
END $$;
GRANT USAGE ON SCHEMA public TO axiom_library;
GRANT USAGE ON SCHEMA public TO axiom_store;

-- 3. Library grants — the Library-owned set (DM04 boundary): the
--    acquisition/identity tables, the Zotero mirror, the repair
--    track's cases and write audit, and the library ledger. The DO
--    loop grants exactly the named tables THAT EXIST here (the interim
--    shared database carries both sets; a cutover library database
--    carries only the Library set — a store-table grant would abort on
--    the missing relation). The LIST is the contract; existence only
--    shapes which grants land.
DO $$
DECLARE
  tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY[
    'library_imports',
    'library_import_events',
    'library_import_steps',
    'library_external_identifiers',
    'library_metadata_provenance',
    'library_provider_anchors',
    'library_source_revisions',
    'library_write_audit',
    'library_writer_leases',
    'library_schema_migrations',
    'zotero_sources',
    'zotero_items',
    'zotero_collections',
    'zotero_item_collections',
    'zotero_documents',
    'zotero_attachments',
    'zotero_selections',
    'zotero_collection_selections',
    'zotero_write_audit',
    'repair_cases'
  ]
  LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = current_schema() AND table_name = tbl) THEN
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO axiom_library', tbl);
    END IF;
  END LOOP;
END
$$;

-- 4. Store grants — the processing/retrieval set (DM06 froze the
--    boundary: no FK, no reads onto the mirror) plus both store-side
--    migration ledgers. Same existence-tolerant shape as section 3.
DO $$
DECLARE
  tbl text;
BEGIN
  FOREACH tbl IN ARRAY ARRAY[
    'ingest_jobs',
    'schema_migrations',
    'store_schema_migrations',
    'processing_snapshots',
    'processing_chunks',
    'processing_chunk_dense_embeddings',
    'processing_chunk_sparse_embeddings',
    'processing_chunk_relationships',
    'processing_entities',
    'processing_entity_mentions',
    'processing_entity_relationships',
    'processing_artifacts',
    'kg_relation_triples',
    'kg_relation_evidence_docs',
    'kg_entity_roots',
    'kg_superseded_entities',
    'opensearch_outbox'
  ]
  LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = current_schema() AND table_name = tbl) THEN
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO axiom_store', tbl);
    END IF;
  END LOOP;
END
$$;

-- 5. Sequences: none ship today (UUID PKs); the blanket grant keeps
--    future window-added serials working for both components without
--    handing either the other's tables.
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO axiom_library;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO axiom_store;

-- 6. The negative teeth, made explicit (re-running stays idempotent):
--    NOTHING above grants either role the other's tables. The drill
--    proves the matrix: cross-component SELECT/INSERT denied,
--    own-component DML allowed, cross-database CONNECT denied.
