// Package repo: atomic processing-snapshot persistence (Gate 4, work-order §10).
//
// PersistResult is the production ResultPersister (replacing the Gate-2
// errPersister). It runs the full §14 validation, then in ONE caller-owned
// transaction:
//
//  1. inserts the new immutable processing_snapshots row (identity §10.1) — or
//     returns the existing snapshot if the identity replays (idempotent);
//  2. inserts chunks, dense/sparse embeddings, entities + mentions, chunk/entity
//     relationships, verified durable artifacts — mapping job-local refs to
//     durable ids;
//  3. verifies row counts and references against the validated result;
//  4. deactivates the previous active snapshot and activates the new one (§10.2);
//  5. inserts the OpenSearch outbox entry (§10.3 — NEVER calls OpenSearch here);
//  6. calls MarkCompletedTx to do the fenced lease completion in the SAME tx;
//  7. commits once.
//
// On any failure the whole transaction rolls back and the previous active
// snapshot survives untouched (§10.2/§14.4). Artifacts are streamed + hashed +
// length-checked by the dispatcher before PersistResult is called; this layer
// only records the verified digest/size/path (artifact bytes are committed via
// an atomic rename on the same filesystem by the dispatcher, see work-order §7).
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/jackc/pgx/v5"
)

// ArtifactRecord is a verified durable artifact (digest/size/path checked by the
// dispatcher before persist). PersistResult records these transactionally.
type ArtifactRecord struct {
	Ref       string
	Kind      string
	MediaType string
	SHA256    string
	SizeBytes int64
	Retention string
	// StoragePath is the final durable path under AXIOM_ARTIFACT_ROOT.
	StoragePath string
}

// PersistOptions carries the verified artifacts and the capability dimension
// (an int — Hivemind Gate-3 hint) alongside the result bytes.
type PersistOptions struct {
	CapDim    int
	Artifacts []ArtifactRecord
}

// PersistResult is the production persistence entry point. It returns the
// durable snapshot id (or an error). It is the implementation of
// dispatcher.ResultPersister for the repo.
func (r *Repo) PersistResult(ctx context.Context, jobID string, raw []byte, opts PersistOptions) (string, error) {
	res, err := DecodeProcessorResult(raw)
	if err != nil {
		return "", &ValidationError{Code: "RESULT_INVALID", Message: err.Error()}
	}

	// Read the claim-time frozen input + the job's durable identity for validation.
	frozen, ident, err := r.loadJobForPersist(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("persist %s: load job: %w", jobID, err)
	}
	if err := ValidateProcessorResult(res, frozen, opts.CapDim); err != nil {
		return "", err
	}
	// §14.4: verified durable artifacts must match the result's declarations.
	// The dispatcher hashes the fetched bytes and builds ArtifactRecords; this
	// check refuses a record whose ref/sha256/size_bytes/media_type diverge
	// from the result (digest or size mismatch) or a result artifact that has
	// no verified record. Runs BEFORE any row is inserted so nothing rolls back
	// a partial snapshot.
	if err := validateArtifactsMatch(res, opts.Artifacts); err != nil {
		return "", err
	}

	snapshotID, err := r.persistTx(ctx, jobID, ident, res, opts.Artifacts, frozen.Processing.ForceRebuild)
	if err != nil {
		return "", err
	}
	return snapshotID, nil
}

// deactivateSiblingsTx flips every OTHER active snapshot of the attachment to
// inactive (#125 scope) and returns their ids so the caller can plan outbox
// tombstones (#127) in the same transaction.
func deactivateSiblingsTx(ctx context.Context, tx pgx.Tx, ident jobIdentity, keepID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		UPDATE processing_snapshots SET active=false, updated_at=now()
		WHERE document_id=$1 AND attachment_id=$2 AND active=true AND id<>$3
		RETURNING id::text`,
		ident.documentID, ident.attachmentID, keepID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// enqueueOutboxTx plans an OpenSearch outbox operation for a snapshot inside
// the persist transaction (#127): 'delete' tombstones a deactivated
// generation's chunk docs, 'index' re-materializes a reactivated one. The
// drainer's obsolete-op guards make execution order-insensitive.
func enqueueOutboxTx(ctx context.Context, tx pgx.Tx, snapshotID, operation string, ident jobIdentity) error {
	return enqueueOutboxTxWithChunks(ctx, tx, snapshotID, operation, ident, nil)
}

// enqueueOutboxTxWithChunks carries FROZEN chunk ids in the payload — used
// by force-replace only: after the force replace the drainer cannot read the
// old generation's chunk ids from PG (they are CASCADE-deleted in the same
// tx, so OutboxChunkIDs would return nothing — or worse, the reactivated
// row's NEW ids), so the tombstone must freeze them or the superseded
// generation's OpenSearch docs leak forever (the precision-wave #127-class
// leak: 102,957 OS docs vs 35,286 active chunks). These frozen ids are
// provably the dead generation's because force-replace deletes the old chunk
// ROWS and the re-insert in the same tx receives brand-new fresh UUIDs — the
// canonical invariant the drainer's frozen-id bypass leans on.
func enqueueOutboxTxWithChunks(ctx context.Context, tx pgx.Tx, snapshotID, operation string, ident jobIdentity, chunkIDs []string) error {
	payload := map[string]any{
		"snapshot_id":   snapshotID,
		"document_id":   ident.documentID,
		"attachment_id": ident.attachmentID,
		"operation":     operation,
	}
	if len(chunkIDs) > 0 {
		payload["chunk_ids"] = chunkIDs
	}
	pl, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO opensearch_outbox (snapshot_id, operation, payload)
		VALUES ($1, $2, $3::jsonb)`, snapshotID, operation, pl)
	return err
}

// freezeChunkIDsTx reads a snapshot's CURRENT chunk ids inside the replace
// tx, strictly before the force-replace branch deletes the rows — after the
// tx the drainer can no longer read them (rows gone; the re-inserted
// generation carries fresh UUIDs). #212.
func freezeChunkIDsTx(ctx context.Context, tx pgx.Tx, snapshotID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM processing_chunks WHERE snapshot_id=$1`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// persistTx runs the single fenced transaction. force = the claim-time
// force_rebuild flag: a force re-run with UNCHANGED identity REPLACES the
// snapshot content in place (identity uniqueness forbids a second row) —
// the pre-fix replay silently discarded fresh results (the W9 chapter
// re-run void: 44 completed, 0 stamps).
func (r *Repo) persistTx(ctx context.Context, jobID string, ident jobIdentity, res *processor.Result, arts []ArtifactRecord, force bool) (string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// #198 item 1 — KG frontmatter gate: entities/relationships whose
	// evidence sits in gated frontmatter section classes never persist.
	// Runs BEFORE any insert so verifyCounts and the insert loops see the
	// filtered arrays (idempotent on identity replays: a replay returns
	// early below without inserting, and a filtered res re-gates clean).
	gateKgFrontmatter(res)

	// Identity replay (§10.1): "replaying the same completed result must return the
	// existing snapshot and remain safe." Identity is the tuple
	// (attachment_id, content_hash, processor_name, processor_version, profile_hash)
	// INDEPENDENT of the active flag — the unique index covers identity alone.
	// So we look up WITHOUT `active=true`: an inactive match (e.g. a superseded
	// snapshot being re-asserted by a re-processed job with the same identity)
	// must NOT fall through to INSERT, which would hit processing_snapshots_identity_uq
	// and roll the whole transaction back with a DB error instead of clean idempotency.
	//
	// Decision on an inactive identity match (Hivemind schema checkpoint): REACTIVATE
	// it (§10.1 mandates returning the existing snapshot). The generation mechanism
	// is reserved for explicit force-rebuilds with a NEW identity (new hash/profile),
	// not for identity collisions. Reactivation deactivates any other currently-active
	// snapshot in the same scope first so the partial unique index is never violated.
	var existingID string
	var existingActive bool
	replacing := false
	err = tx.QueryRow(ctx, `
		SELECT id::text, active FROM processing_snapshots
		WHERE attachment_id=$1 AND content_hash=$2
		  AND processor_name=$3 AND processor_version=$4 AND profile_hash=$5
		LIMIT 1`,
		ident.attachmentID, ident.contentHash, res.Processor.Name, res.Processor.Version, ident.profileHash,
	).Scan(&existingID, &existingActive)
	switch {
	case err == nil:
		if force {
			// Force replace-in-place (#194-class, W9 void): tombstone the old
			// chunk docs, CASCADE-clear the children (entities/chunks take
			// embeddings, mentions, relationships, chunk-relationships with
			// them; artifacts are cleared separately), bump the generation and
			// refresh the provenance columns — then the shared child-insert +
			// activation path below runs against the SAME row.
			sib, serr := deactivateSiblingsTx(ctx, tx, ident, existingID)
			if serr != nil {
				return "", fmt.Errorf("force replace deactivate siblings: %w", serr)
			}
			for _, s := range sib {
				if oerr := enqueueOutboxTx(ctx, tx, s, OutboxOpDelete, ident); oerr != nil {
					return "", fmt.Errorf("force replace sibling tombstone: %w", oerr)
				}
			}
			// Freeze the OLD chunk ids into the delete tombstone BEFORE the rows
			// are deleted below — the structural #212 fix (the precision-wave
			// leak: 102,957 OS docs vs 35,286 active chunks, ~57.7k stale docs
			// were serving).
			oldIDs, ferr := freezeChunkIDsTx(ctx, tx, existingID)
			if ferr != nil {
				return "", fmt.Errorf("force replace freeze old chunk ids: %w", ferr)
			}
			if oerr := enqueueOutboxTxWithChunks(ctx, tx, existingID, OutboxOpDelete, ident, oldIDs); oerr != nil {
				return "", fmt.Errorf("force replace tombstone old docs: %w", oerr)
			}
			if _, derr := tx.Exec(ctx, `
				DELETE FROM processing_artifacts WHERE snapshot_id=$1`, existingID); derr != nil {
				return "", fmt.Errorf("force replace clear artifacts: %w", derr)
			}
			if _, derr := tx.Exec(ctx, `
				DELETE FROM processing_entities WHERE snapshot_id=$1`, existingID); derr != nil {
				return "", fmt.Errorf("force replace clear entities: %w", derr)
			}
			if _, derr := tx.Exec(ctx, `
				DELETE FROM processing_chunks WHERE snapshot_id=$1`, existingID); derr != nil {
				return "", fmt.Errorf("force replace clear chunks: %w", derr)
			}
			manifestJSON, _ := json.Marshal(res.Manifest)
			modelsJSON, _ := json.Marshal(res.Processor.Models)
			warningsJSON, _ := json.Marshal(res.Warnings)
			if _, uerr := tx.Exec(ctx, `
				UPDATE processing_snapshots SET
				  generation = generation + 1, active = true, updated_at = now(),
				  profile = $2, models = $3, manifest = $4, warnings = $5,
				  source_verified = $6, ingest_job_id = $7
				WHERE id = $1`,
				existingID, res.Processor.Profile, modelsJSON, manifestJSON, warningsJSON,
				res.Source.Verified, jobID); uerr != nil {
				return "", fmt.Errorf("force replace refresh row: %w", uerr)
			}
			// Fall through to the shared child-insert path with the SAME row.
			replacing = true
			break
		}
		// Idempotent replay of the SAME completed result. If the existing snapshot
		// is inactive, reactivate it (deactivating any other active one in scope
		// first to preserve the <=1 active invariant). Its content is identical
		// (same identity => same bytes), so no row re-insert is needed.
		if !existingActive {
			// #125: deactivate across profiles — readers count actives per
			// ATTACHMENT; a profile-hash change (force_rebuild flag lives in the
			// canonical block) must still supersede the old active snapshot.
			// #127: each deactivation plans an outbox tombstone, the reactivation
			// plans a re-index — the index mirrors the active flip atomically.
			siblings, err := deactivateSiblingsTx(ctx, tx, ident, existingID)
			if err != nil {
				return "", fmt.Errorf("replay deactivate other: %w", err)
			}
			for _, sib := range siblings {
				if err := enqueueOutboxTx(ctx, tx, sib, OutboxOpDelete, ident); err != nil {
					return "", fmt.Errorf("replay tombstone: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE processing_snapshots SET active=true, updated_at=now() WHERE id=$1`, existingID); err != nil {
				return "", fmt.Errorf("replay reactivate: %w", err)
			}
			if err := enqueueOutboxTx(ctx, tx, existingID, OutboxOpIndex, ident); err != nil {
				return "", fmt.Errorf("replay reindex: %w", err)
			}
		}
		// #118-smoke root cause (closes the #126(a) mystery): the replay branch
		// previously returned WITHOUT fence-completing the job row — the snapshot
		// was correct but ingest_jobs stayed 'processing', the lease expired, the
		// job was re-claimed and the resubmit hit the ACKed runner (ARTIFACTS_EXPIRED
		// today, the artifact-404 wall pre-#126). A replay IS a valid completion:
		// same fenced semantics as the insert path. An empty lease token means the
		// row is already unleased/completed — nothing to fence (idempotent §10.1);
		// fence loss is likewise tolerated.
		if ident.leaseRef.LeaseToken != "" {
			if err := r.MarkCompletedTx(ctx, tx, ident.leaseRef, res.Processor.Name, res.Processor.Version, existingID); err != nil && !errors.Is(err, ErrLostLease) {
				return "", fmt.Errorf("replay mark completed: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit replay: %w", err)
		}
		return existingID, nil
	case errors.Is(err, pgx.ErrNoRows):
		// proceed to insert
	default:
		return "", fmt.Errorf("lookup existing snapshot: %w", err)
	}

	// 1. Insert the new immutable snapshot row (inactive until step 4) —
	// unless the force path above is replacing an existing identity row.
	var snapshotID string
	if replacing {
		snapshotID = existingID
	} else {
		manifestJSON, _ := json.Marshal(res.Manifest)
		modelsJSON, _ := json.Marshal(res.Processor.Models)
		warningsJSON, _ := json.Marshal(res.Warnings)
		err = tx.QueryRow(ctx, `
			INSERT INTO processing_snapshots
			  (attachment_id, content_hash, processor_name, processor_version, profile_hash,
			   document_id, profile, models, manifest, warnings, source_verified, ingest_job_id, active)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,false)
			RETURNING id::text`,
			ident.attachmentID, ident.contentHash, res.Processor.Name, res.Processor.Version, ident.profileHash,
			ident.documentID, res.Processor.Profile, modelsJSON, manifestJSON, warningsJSON,
			res.Source.Verified, jobID,
		).Scan(&snapshotID)
		if err != nil {
			return "", fmt.Errorf("insert snapshot: %w", err)
		}
	}

	// 2a. Chunks + dense/sparse embeddings. Build job-local->durable id maps.
	chunkIDs := make(map[string]string, len(res.Chunks))
	for _, c := range res.Chunks {
		var cid string
		locJSON, _ := json.Marshal(c.Locator)
		secJSON, _ := json.Marshal(c.Structure.SectionTitles)
		imgJSON, _ := json.Marshal(c.ImageRefs)
		err = tx.QueryRow(ctx, `
			INSERT INTO processing_chunks
			  (snapshot_id, chunk_index, text, locator, section_titles,
			   start_paragraph_index, end_paragraph_index, token_count, image_refs)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING id::text`,
			snapshotID, c.Index, c.Text, locJSON, secJSON,
			ptrToInt(c.Structure.StartParagraphIndex), ptrToInt(c.Structure.EndParagraphIndex),
			c.TokenCount, imgJSON,
		).Scan(&cid)
		if err != nil {
			return "", fmt.Errorf("insert chunk %d: %w", c.Index, err)
		}
		chunkIDs[c.Ref] = cid

		if c.Embeddings.Dense != nil {
			vecStr := denseVectorLiteral(c.Embeddings.Dense.Values)
			if _, err := tx.Exec(ctx, `
				INSERT INTO processing_chunk_dense_embeddings (chunk_id, model, dimensions, vector)
				VALUES ($1,$2,$3,$4::vector)`,
				cid, c.Embeddings.Dense.Model, c.Embeddings.Dense.Dimensions, vecStr); err != nil {
				return "", fmt.Errorf("insert dense embedding chunk %d: %w", c.Index, err)
			}
		}
		if c.Embeddings.Sparse != nil {
			valsJSON, _ := json.Marshal(c.Embeddings.Sparse.Values)
			if _, err := tx.Exec(ctx, `
				INSERT INTO processing_chunk_sparse_embeddings (chunk_id, model, values)
				VALUES ($1,$2,$3)`,
				cid, c.Embeddings.Sparse.Model, valsJSON); err != nil {
				return "", fmt.Errorf("insert sparse embedding chunk %d: %w", c.Index, err)
			}
		}
	}

	// 2b. Entities + mentions.
	// W5 (#199 S3b): the role/group-noun gate corrects the type AT INGEST —
	// new books arrive with generic role nouns typed CONCEPT instead of
	// PERSON/ORGANIZATION. The post-hoc -normalize-entity-types remains as
	// a migration for existing stock only.
	entityIDs := make(map[string]string, len(res.Entities))
	for _, e := range res.Entities {
		var eid string
		et := gateEntityType(e.CanonicalForm, e.Text, e.Type)
		err = tx.QueryRow(ctx, `
			INSERT INTO processing_entities
			  (snapshot_id, ref, text, canonical_form, type, description)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id::text`,
			snapshotID, e.Ref, e.Text, e.CanonicalForm, et, e.Description,
		).Scan(&eid)
		if err != nil {
			return "", fmt.Errorf("insert entity %s: %w", e.Ref, err)
		}
		entityIDs[e.Ref] = eid
		for _, m := range e.Mentions {
			durableChunk := chunkIDs[m.ChunkRef]
			if durableChunk == "" {
				return "", verrf("ENTITY_MENTION_CHUNK_UNRESOLVED", "entity %s mention chunk %s unresolved at persist", e.Ref, m.ChunkRef)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO processing_entity_mentions
				  (entity_id, chunk_id, start_char, end_char, confidence)
				VALUES ($1,$2,$3,$4,$5)`,
				eid, durableChunk, m.StartChar, m.EndChar, m.Confidence); err != nil {
				return "", fmt.Errorf("insert entity mention: %w", err)
			}
		}
	}

	// 2c. Relationships with mandatory evidence for non-sequential (§12).
	for _, rel := range res.ChunkRelationships {
		src := chunkIDs[rel.SourceChunkRef]
		tgt := chunkIDs[rel.TargetChunkRef]
		if src == "" || tgt == "" {
			return "", verrf("CHUNK_REL_REF_UNRESOLVED", "chunk relationship endpoint unresolved")
		}
		evIDs, err := resolveEvidenceIDs(ctx, tx, snapshotID, rel.EvidenceChunkRefs, chunkIDs)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_chunk_relationships
			  (snapshot_id, source_chunk_id, target_chunk_id, type, strength, evidence_chunk_ids)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			snapshotID, src, tgt, rel.Type, rel.Strength, evIDs); err != nil {
			return "", fmt.Errorf("insert chunk relationship: %w", err)
		}
	}
	for _, rel := range res.EntityRelationships {
		src := entityIDs[rel.SourceEntityRef]
		tgt := entityIDs[rel.TargetEntityRef]
		if src == "" || tgt == "" {
			return "", verrf("ENTITY_REL_REF_UNRESOLVED", "entity relationship endpoint unresolved")
		}
		evIDs, err := resolveEvidenceIDs(ctx, tx, snapshotID, rel.EvidenceChunkRefs, chunkIDs)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_entity_relationships
			  (snapshot_id, source_entity_id, target_entity_id, type, strength, evidence_chunk_ids, extractor)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			snapshotID, src, tgt, rel.Type, rel.Strength, evIDs, rel.Extractor); err != nil {
			return "", fmt.Errorf("insert entity relationship: %w", err)
		}
	}

	// 2d. Verified durable artifacts.
	for _, a := range arts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO processing_artifacts
			  (snapshot_id, ref, kind, media_type, sha256, size_bytes, retention, storage_path)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			snapshotID, a.Ref, a.Kind, a.MediaType, a.SHA256, a.SizeBytes, a.Retention, a.StoragePath); err != nil {
			return "", fmt.Errorf("insert artifact %s: %w", a.Ref, err)
		}
	}

	// 3. Verify counts against the actual inserted rows (defence-in-depth).
	if err := verifyCounts(ctx, tx, snapshotID, res, len(arts)); err != nil {
		return "", err
	}

	// 4. Atomic active-snapshot switch: deactivate previous, activate new (§10.2).
	// #125: the deactivation is scoped per ATTACHMENT, not per profile_hash —
	// force_rebuild (and any profile change) freezes a different profile_hash,
	// and the old per-profile scope left the previous generation active next
	// to the new one (TC2: ESGBS counted 68 = 34+34 chunks). Latest persist
	// wins; superseded generations stay queryable, just inactive.
	// #127: every deactivation plans an outbox tombstone so OpenSearch stops
	// serving superseded generations (same tx — the index flip mirrors the
	// active flip).
	siblings, err := deactivateSiblingsTx(ctx, tx, ident, snapshotID)
	if err != nil {
		return "", fmt.Errorf("deactivate previous snapshot: %w", err)
	}
	for _, sib := range siblings {
		if err := enqueueOutboxTx(ctx, tx, sib, OutboxOpDelete, ident); err != nil {
			return "", fmt.Errorf("tombstone superseded snapshot: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_snapshots SET active=true, updated_at=now() WHERE id=$1`, snapshotID); err != nil {
		return "", fmt.Errorf("activate new snapshot: %w", err)
	}

	// 5. OpenSearch outbox entry (§10.3) — no OpenSearch call in this tx.
	if err := enqueueOutboxTx(ctx, tx, snapshotID, OutboxOpIndex, ident); err != nil {
		return "", fmt.Errorf("insert outbox: %w", err)
	}

	// 6. Fenced completion in the SAME transaction (lease predicate + source
	// advisory lock handled by MarkCompletedTx). The replace path shares the
	// replay's fence-loss tolerance (#118-smoke class): a force re-drive whose
	// lease expired mid-run still replaces durably; the job row legitimately
	// stays 'processing' for the re-claim loop.
	if err := r.MarkCompletedTx(ctx, tx, ident.leaseRef, res.Processor.Name, res.Processor.Version, snapshotID); err != nil &&
		!(replacing && errors.Is(err, ErrLostLease)) {
		return "", fmt.Errorf("mark completed: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return snapshotID, nil
}

// jobIdentity is the durable identity needed for snapshot insert + fenced completion.
type jobIdentity struct {
	attachmentID string
	documentID   string
	contentHash  string
	profileHash  string
	leaseRef     LeaseRef
}

// loadJobForPersist reads the frozen input (for validation) and the durable
// identity + lease ref (for snapshot insert + fenced MarkCompletedTx).
func (r *Repo) loadJobForPersist(ctx context.Context, jobID string) (*FrozenInput, jobIdentity, error) {
	var (
		inputSnap                []byte
		profileHash              string
		attachmentID, documentID string
		contentHash              *string
		claimedBy, leaseToken    string
	)
	err := r.pool.QueryRow(ctx, `
		SELECT input_snapshot, COALESCE(profile_hash,''),
		       attachment_id::text, document_id::text,
		       (input_snapshot->'attachment'->>'content_hash')::text,
		       COALESCE(claimed_by,''), COALESCE(lease_token::text,'')
		FROM ingest_jobs WHERE id=$1`, jobID).Scan(
		&inputSnap, &profileHash, &attachmentID, &documentID, &contentHash, &claimedBy, &leaseToken)
	if err != nil {
		return nil, jobIdentity{}, err
	}
	var frozen FrozenInput
	if err := json.Unmarshal(inputSnap, &frozen); err != nil {
		return nil, jobIdentity{}, fmt.Errorf("decode frozen input: %w", err)
	}
	ch := ""
	if contentHash != nil {
		ch = *contentHash
	}
	ident := jobIdentity{
		attachmentID: attachmentID,
		documentID:   documentID,
		contentHash:  ch,
		profileHash:  profileHash,
		leaseRef:     LeaseRef{JobID: jobID, WorkerID: claimedBy, LeaseToken: leaseToken},
	}
	return &frozen, ident, nil
}

// resolveEvidenceIDs turns job-local evidence chunk refs into a JSONB array of
// durable chunk ids, verifying each resolves.
func resolveEvidenceIDs(ctx context.Context, tx pgx.Tx, snapshotID string, refs []string, chunkIDs map[string]string) ([]byte, error) {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		id := chunkIDs[ref]
		if id == "" {
			return nil, verrf("RELATIONSHIP_EVIDENCE_UNRESOLVED", "evidence chunk %s unresolved", ref)
		}
		out = append(out, id)
	}
	b, _ := json.Marshal(out)
	return b, nil
}

// verifyCounts double-checks the inserted row counts against the validated
// result arrays (defence-in-depth before the active flip).
func verifyCounts(ctx context.Context, tx pgx.Tx, snapshotID string, res *processor.Result, nArt int) error {
	var nChunks, nEnt, nChunkRel, nEntRel, nArtDB int
	row := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM processing_chunks WHERE snapshot_id=$1),
		  (SELECT count(*) FROM processing_entities WHERE snapshot_id=$1),
		  (SELECT count(*) FROM processing_chunk_relationships WHERE snapshot_id=$1),
		  (SELECT count(*) FROM processing_entity_relationships WHERE snapshot_id=$1),
		  (SELECT count(*) FROM processing_artifacts WHERE snapshot_id=$1)`, snapshotID)
	if err := row.Scan(&nChunks, &nEnt, &nChunkRel, &nEntRel, &nArtDB); err != nil {
		return fmt.Errorf("verify counts: %w", err)
	}
	if nChunks != len(res.Chunks) || nEnt != len(res.Entities) ||
		nChunkRel != len(res.ChunkRelationships) || nEntRel != len(res.EntityRelationships) ||
		nArtDB != nArt {
		return verrf("PERSIST_COUNT_MISMATCH",
			"inserted (chunks=%d ent=%d chunkRel=%d entRel=%d art=%d) != validated (%d/%d/%d/%d/%d)",
			nChunks, nEnt, nChunkRel, nEntRel, nArtDB,
			len(res.Chunks), len(res.Entities), len(res.ChunkRelationships), len(res.EntityRelationships), nArt)
	}
	return nil
}

// denseVectorLiteral formats a dense vector for pgvector's textual cast:
// "[0.1,0.2,0.3]". Uses strconv.AppendFloat into a scratch buffer (NOT the
// output slice) to avoid the double-append aliasing bug fmt.Appendf caused.
func denseVectorLiteral(vals []float32) string {
	var scratch [32]byte
	b := make([]byte, 0, len(vals)*12)
	b = append(b, '[')
	for i, v := range vals {
		if i > 0 {
			b = append(b, ',')
		}
		num := strconv.AppendFloat(scratch[:0], float64(v), 'g', -1, 32)
		b = append(b, num...)
	}
	b = append(b, ']')
	return string(b)
}

func ptrToInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// gateEntityType (#199 W5/S3b): the ingest-side typing gate. Generic
// role/group nouns (the typing rule lexicon) arrive as CONCEPT regardless
// of what the extractor claimed — the external tester found stakeholders
// typed PERSON from the extractor. This is a persistTx gate, not a
// post-hoc migration: new books are correct from the start.
func gateEntityType(canonicalForm, text, claimedType string) string {
	form := canonicalForm
	if form == "" {
		form = text
	}
	if form == "" {
		return claimedType
	}
	norm := strings.ToLower(strings.Join(strings.Fields(form), " "))
	for _, role := range typingBareForms {
		if norm == role {
			return "CONCEPT"
		}
	}
	// Plural-head check (short forms ending in a role-plural head)
	for _, head := range typingPluralHeads {
		if strings.HasSuffix(norm, " "+head) &&
			(claimedType == "PERSON" || claimedType == "ORGANIZATION") {
			return "CONCEPT"
		}
	}
	return claimedType
}
