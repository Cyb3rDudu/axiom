-- 0016_superseded_entities.sql (#199 W2): archived loser evidence for
-- entity consolidation. Consolidation deletes loser entity rows after moving
-- mentions/relations; this table preserves the deleted row's semantic evidence
-- (type/description/form/source scope) so type history remains queryable.

CREATE TABLE IF NOT EXISTS kg_superseded_entities (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  survivor_entity_id  UUID REFERENCES processing_entities(id) ON DELETE SET NULL,
  loser_entity_id     UUID NOT NULL,
  loser_ref           TEXT NOT NULL,
  loser_text          TEXT NOT NULL,
  loser_canonical_form TEXT,
  loser_type          TEXT,
  loser_description   TEXT,
  loser_snapshot_id   UUID NOT NULL,
  loser_document_id   UUID NOT NULL,
  mention_count       INTEGER NOT NULL DEFAULT 0,
  operation           TEXT NOT NULL DEFAULT 'entity_consolidation',
  archived_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS kg_superseded_entities_survivor_loser_uq
  ON kg_superseded_entities (survivor_entity_id, loser_entity_id, operation);

CREATE INDEX IF NOT EXISTS kg_superseded_entities_survivor_idx
  ON kg_superseded_entities (survivor_entity_id, archived_at);

CREATE INDEX IF NOT EXISTS kg_superseded_entities_loser_idx
  ON kg_superseded_entities (loser_entity_id);

CREATE INDEX IF NOT EXISTS kg_superseded_entities_form_idx
  ON kg_superseded_entities (coalesce(loser_canonical_form, loser_text));
