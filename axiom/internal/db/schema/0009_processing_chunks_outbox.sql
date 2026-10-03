-- 0009_processing_chunks_outbox.sql
-- Chunks, locators, embeddings, entities, relationships and the OpenSearch
-- outbox (Gate 4). Depends on 0008 (processing_snapshots + the vector extension).
--
-- All rows are scoped to a processing_snapshots row via ON DELETE CASCADE: a
-- snapshot is immutable once active, and deactivating/superseding never deletes
-- its rows (retention removes them explicitly). Inserting these rows + flipping
-- the active snapshot + the outbox entry + MarkCompletedTx happen in ONE
-- caller-owned transaction (work-order §10.2/§10.3).
--
-- CONTRACT §11/§12 rules enforced here at the DB level where cheap:
--   - unique contiguous chunk index per snapshot (processing_chunks)
--   - unique job-local refs per snapshot (chunks, entities)
--   - entity mentions point at durable chunk ids (job-local -> durable mapping
--     happens in Go before insert)
--   - relationship evidence is enforced in Go validation (§12) since the
--     evidence array is variably required (non-sequential only)
-- The OpenSearch outbox (§10.3) is created transactionally here; a retryable
-- worker drains it. OpenSearch is NEVER called inside the snapshot transaction.
-- All tables are additive (IF NOT EXISTS).

-- Processing chunks: ordered text spans with source locators (§11).
CREATE TABLE IF NOT EXISTS processing_chunks (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id     UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  -- Zero-based index within this snapshot; contiguous and unique per snapshot.
  chunk_index     INTEGER NOT NULL,
  text            TEXT NOT NULL,
  -- Locator (§11): page_span {physical_page_start/end,page_label_start/end,source}
  -- or epub_cfi {cfi_start,cfi_end,source}. JSONB keeps the shape flexible while
  -- Go validates the required keys per format.
  locator         JSONB NOT NULL DEFAULT '{}'::jsonb,
  -- Ordered heading hierarchy + paragraph index range.
  section_titles  JSONB NOT NULL DEFAULT '[]'::jsonb,
  start_paragraph_index INTEGER,
  end_paragraph_index   INTEGER,
  token_count     INTEGER NOT NULL DEFAULT 0,
  -- Durable artifact refs (images/tables) resolved within the snapshot.
  image_refs      JSONB NOT NULL DEFAULT '[]'::jsonb,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (snapshot_id, chunk_index)
);

CREATE INDEX IF NOT EXISTS processing_chunks_snapshot_idx
ON processing_chunks (snapshot_id, chunk_index);

-- Dense embeddings (pgvector). Dimensions come from the processor capability
-- (an int) and are cross-validated against the result's declared dimension
-- before insert (contract §6/§10/§14). A fixed-width vector column would force
-- a migration per model change, so we store the model + dimensions alongside.
CREATE TABLE IF NOT EXISTS processing_chunk_dense_embeddings (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  chunk_id     UUID NOT NULL REFERENCES processing_chunks(id) ON DELETE CASCADE,
  model        TEXT NOT NULL,
  dimensions   INTEGER NOT NULL,
  vector       vector NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (chunk_id)
);

-- Sparse embeddings: bucketed key -> weight (like BM25). keys are strings,
-- values are real. One row per chunk to keep the join cheap.
CREATE TABLE IF NOT EXISTS processing_chunk_sparse_embeddings (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  chunk_id     UUID NOT NULL REFERENCES processing_chunks(id) ON DELETE CASCADE,
  model        TEXT NOT NULL,
  -- JSONB object {key(string): weight(real)}; Go validates finite numeric weights.
  values       JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (chunk_id)
);

-- Entities and their chunk occurrences (mentions). Mentions carry durable
-- chunk_ids (mapped from the processor's job-local refs by Go before insert).
CREATE TABLE IF NOT EXISTS processing_entities (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id     UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  -- Job-local ref echoed from the result; unique within the snapshot.
  ref             TEXT NOT NULL,
  text            TEXT NOT NULL,
  canonical_form  TEXT,
  type            TEXT,
  description     TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (snapshot_id, ref)
);

CREATE TABLE IF NOT EXISTS processing_entity_mentions (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  entity_id     UUID NOT NULL REFERENCES processing_entities(id) ON DELETE CASCADE,
  chunk_id      UUID NOT NULL REFERENCES processing_chunks(id) ON DELETE CASCADE,
  start_char    INTEGER NOT NULL,
  end_char      INTEGER NOT NULL,
  confidence    REAL,
  UNIQUE (entity_id, chunk_id, start_char, end_char)
);

CREATE INDEX IF NOT EXISTS processing_entity_mentions_chunk_idx
ON processing_entity_mentions (chunk_id);

-- Relationships. Chunk- and entity-relationships are scoped to the snapshot
-- through their endpoints; non-sequential relationships MUST carry evidence
-- chunk refs (§12), enforced in Go (the rule is variably required: sequential
-- types are exempt). Both tables store endpoint refs as durable ids.
CREATE TABLE IF NOT EXISTS processing_chunk_relationships (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id         UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  source_chunk_id     UUID NOT NULL REFERENCES processing_chunks(id) ON DELETE CASCADE,
  target_chunk_id     UUID NOT NULL REFERENCES processing_chunks(id) ON DELETE CASCADE,
  type                TEXT NOT NULL,
  strength            REAL,
  -- JSONB array of durable chunk ids (evidence); mandatory for non-sequential.
  evidence_chunk_ids  JSONB NOT NULL DEFAULT '[]'::jsonb,
  metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS processing_entity_relationships (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id         UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  source_entity_id    UUID NOT NULL REFERENCES processing_entities(id) ON DELETE CASCADE,
  target_entity_id    UUID NOT NULL REFERENCES processing_entities(id) ON DELETE CASCADE,
  type                TEXT NOT NULL,
  strength            REAL,
  -- JSONB array of durable chunk ids (evidence); mandatory for non-sequential (§12).
  evidence_chunk_ids  JSONB NOT NULL DEFAULT '[]'::jsonb,
  extractor           TEXT,
  metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS processing_chunk_relationships_snapshot_idx
ON processing_chunk_relationships (snapshot_id);
CREATE INDEX IF NOT EXISTS processing_entity_relationships_snapshot_idx
ON processing_entity_relationships (snapshot_id);

-- OpenSearch outbox (§10.3). One row per snapshot operation, created in the SAME
-- transaction as the snapshot rows + active flip + MarkCompletedTx. A separate
-- retryable worker drains opensearch_outbox and indexes; it MUST NOT be on the
-- snapshot-transaction path. An OpenSearch outage leaves a retryable outbox item
-- and does NOT fail the snapshot or rerun processing.
CREATE TABLE IF NOT EXISTS opensearch_outbox (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  snapshot_id   UUID NOT NULL REFERENCES processing_snapshots(id) ON DELETE CASCADE,
  operation     TEXT NOT NULL,                -- 'index' | 'delete'
  -- Payload is the minimal snapshot identity + durable ids the indexer needs.
  payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
  status        TEXT NOT NULL DEFAULT 'pending',  -- 'pending'|'done'|'failed'
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS opensearch_outbox_drain_idx
ON opensearch_outbox (status, next_attempt_at)
WHERE status = 'pending';
