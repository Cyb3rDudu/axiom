-- 0022 (#257): document-native figure captions.
--
-- Chunk-level figure_captions: the "Figure N …" / "Fig. N …" lines of a
-- chunk's own text, paired with the referenced image refs, extracted
-- deterministically at ingest (no model). Unlike image_captions (machine
-- claims, never citable), a figure caption IS document text and stays
-- citable in its locator context — hence a SEPARATE column, never merged.
-- The outbox folds both into the labeled caption_text index field
-- ([machine image caption: …] / [document figure caption: …]).

ALTER TABLE processing_chunks
  ADD COLUMN IF NOT EXISTS figure_captions JSONB NOT NULL DEFAULT '{}';
