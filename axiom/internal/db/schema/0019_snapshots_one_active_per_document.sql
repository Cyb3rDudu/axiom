-- 0019 (#228): enforce the one-active-snapshot-per-DOCUMENT invariant at
-- the DB level — the 0011 discipline one level up. A document with a PDF
-- and an EPUB attachment could legally hold two active snapshots: retrieval
-- served the same content twice and the KG ingested both extractions
-- (mentions/evidence double-counted). Production was clean only
-- incidentally (the preferred selector enqueues one format).
--
-- Sweep prerequisite (mirrors the 0011 rollout discipline): the index can
-- only be created on a corpus with ZERO violations. Sweep (read-only):
--   SELECT document_id, count(*) FROM processing_snapshots
--   WHERE active GROUP BY document_id HAVING count(*) > 1;
-- Verified 0 rows on axiom_db before this migration shipped (2026-08-31,
-- #228 acceptance).
--
-- The persist path deactivates every OTHER active snapshot of the DOCUMENT
-- (document-scoped deactivateSiblingsTx) before activating the winner in
-- the same transaction, so a well-behaved writer never holds two active
-- rows per document; a mixed-binary or rogue writer (attachment-scoped
-- deactivation) FAILS loudly here instead of building duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS processing_snapshots_one_active_per_document_uq
  ON processing_snapshots (document_id)
  WHERE active = true;
