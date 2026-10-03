-- 0015_entity_aliases.sql (#198-3): flexion alias binding.
-- Variants of a lemma family (Nachhaltigkeitsbericht/-berichte/-berichts)
-- stay separate entity rows (exact-form consolidation #197 deliberately
-- keeps different forms apart) but carry alias_of = family survivor so the
-- graph can lead the family as ONE node with a forms list. Additive,
-- nullable — pre-#198-3 data reads exactly as before.

ALTER TABLE processing_entities
  ADD COLUMN IF NOT EXISTS alias_of UUID REFERENCES processing_entities(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS processing_entities_alias_of_idx
  ON processing_entities (alias_of)
  WHERE alias_of IS NOT NULL;
