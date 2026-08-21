-- 0017_kg_read_model.sql (#199 W4): materialized KG API read model.
-- Rebuildable projection over active raw KG state; raw extractor rows remain
-- the source of truth. API reads from these tables after RefreshKGReadModel.

CREATE TABLE IF NOT EXISTS kg_entity_roots (
  root_entity_id       UUID PRIMARY KEY REFERENCES processing_entities(id) ON DELETE CASCADE,
  primary_form         TEXT NOT NULL,
  primary_text         TEXT NOT NULL,
  primary_type         TEXT,
  forms                JSONB NOT NULL DEFAULT '[]'::jsonb,
  type_votes           JSONB NOT NULL DEFAULT '{}'::jsonb,
  mention_count        INTEGER NOT NULL DEFAULT 0,
  member_count         INTEGER NOT NULL DEFAULT 0,
  normalized_form      TEXT NOT NULL,
  normalized_form_nofam TEXT NOT NULL,
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS kg_entity_roots_mentions_idx
  ON kg_entity_roots (mention_count DESC, root_entity_id);
CREATE INDEX IF NOT EXISTS kg_entity_roots_norm_idx
  ON kg_entity_roots (normalized_form);
CREATE INDEX IF NOT EXISTS kg_entity_roots_norm_nofam_idx
  ON kg_entity_roots (normalized_form_nofam);

CREATE TABLE IF NOT EXISTS kg_relation_triples (
  id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  source_root_id           UUID NOT NULL REFERENCES kg_entity_roots(root_entity_id) ON DELETE CASCADE,
  target_root_id           UUID NOT NULL REFERENCES kg_entity_roots(root_entity_id) ON DELETE CASCADE,
  type                    TEXT NOT NULL,
  source_form              TEXT NOT NULL,
  target_form              TEXT NOT NULL,
  source_type              TEXT,
  target_type              TEXT,
  source_mentions          INTEGER NOT NULL DEFAULT 0,
  target_mentions          INTEGER NOT NULL DEFAULT 0,
  strength                 REAL,
  evidence_chunk_ids       JSONB NOT NULL DEFAULT '[]'::jsonb,
  evidence_count           INTEGER NOT NULL DEFAULT 0,
  triple_row_count         INTEGER NOT NULL DEFAULT 0,
  corroborating_documents  INTEGER NOT NULL DEFAULT 1,
  section_quality          REAL NOT NULL DEFAULT 1,
  confidence               REAL NOT NULL DEFAULT 0,
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (source_root_id, target_root_id, type),
  CHECK (source_root_id <> target_root_id)
);

CREATE INDEX IF NOT EXISTS kg_relation_triples_type_rank_idx
  ON kg_relation_triples (type, corroborating_documents DESC, confidence DESC);
CREATE INDEX IF NOT EXISTS kg_relation_triples_source_idx
  ON kg_relation_triples (source_root_id, corroborating_documents DESC, confidence DESC);
CREATE INDEX IF NOT EXISTS kg_relation_triples_target_idx
  ON kg_relation_triples (target_root_id, corroborating_documents DESC, confidence DESC);
CREATE INDEX IF NOT EXISTS kg_relation_triples_rank_idx
  ON kg_relation_triples (corroborating_documents DESC, confidence DESC);

CREATE TABLE IF NOT EXISTS kg_relation_evidence_docs (
  triple_id      UUID NOT NULL REFERENCES kg_relation_triples(id) ON DELETE CASCADE,
  document_id    UUID NOT NULL,
  evidence_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (triple_id, document_id)
);

CREATE INDEX IF NOT EXISTS kg_relation_evidence_docs_doc_idx
  ON kg_relation_evidence_docs (document_id, triple_id);
