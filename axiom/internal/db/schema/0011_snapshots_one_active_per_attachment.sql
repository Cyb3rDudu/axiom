-- 0011 (#125): enforce the one-active-snapshot-per-attachment invariant at
-- the DB level. #125 fixed the persist path (deactivation scoped per
-- attachment instead of per profile_hash — force_rebuild freezes a different
-- profile_hash, so the old scope left two actives: TC2 ESGBS 68 = 34+34).
-- The partial unique index from 0008 only scopes per (document, attachment,
-- profile); this stricter index makes a mixed-binary deploy (old dispatcher
-- binary still running, as happened live during the #125 production proof)
-- or any rogue writer FAIL loudly instead of silently recreating the
-- double-activation. Safe with the persist flip order: both the insert path
-- (step 4) and the identity-replay path deactivate sibling actives BEFORE
-- activating/reactivating the winner, so a transaction never holds two
-- active rows for the same attachment.
CREATE UNIQUE INDEX IF NOT EXISTS processing_snapshots_one_active_per_attachment_uq
  ON processing_snapshots (attachment_id)
  WHERE active = true;
