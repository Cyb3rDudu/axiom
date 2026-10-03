-- 0021 (#230): machine captions for extracted images.
--
-- Artifact attributes: additive JSONB home for #230's machine_caption /
-- caption_model / caption_path (provenance fields will grow — one generic
-- column beats three typed ones and keeps §13 additivity). Chunk captions:
-- the caption of every image a chunk references, as ADDITIONAL indexable
-- text (BM25 arm reads it via the outbox caption_text field; the dense arm
-- already embedded it at ingest). A caption is a machine claim and is
-- never appended to chunk text.

ALTER TABLE processing_artifacts
  ADD COLUMN IF NOT EXISTS attributes JSONB NOT NULL DEFAULT '{}';

ALTER TABLE processing_chunks
  ADD COLUMN IF NOT EXISTS image_captions JSONB NOT NULL DEFAULT '{}';
