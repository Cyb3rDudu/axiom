// intake_claim.go — the revision-lane claim (F09 #303, #358): resolves a
// revision-typed job against the Store's OWN document projection (the
// denormalized rendition row written at intake). Zotero keys stay opaque
// external references throughout; the revision's OWN truth (hash,
// bibliography, capabilities, ticket) is the frozen contract artifact,
// the projection contributes the durable identities + the file facts the
// processor wire needs. The legacy mirror-read lane died with the mirror's
// move to the Library database — legacy-lane jobs are obsoleted with
// LEGACY_LANE_RETIRED by the claim.
package repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/revision"
	"github.com/jackc/pgx/v5"
)

// loadAndLockRevisionState is the revision lane of the claim: same
// locking discipline (per-source advisory lock FIRST, then the rendition
// row FOR UPDATE), same obsolescence contract (non-empty reason = skip).
// Differences from the pre-#358 mirror-read lane:
//
//   - the rendition row is the Store's store_documents projection (no
//     Library database access at claim time — the identities and file
//     facts were denormalized at intake);
//   - the citation class (contextual KG gate) comes from the projection
//     (the intake wrote the revision's contract truth there);
//   - no legacy-lane collision guard: the legacy idempotency index is
//     zotero-lane-scoped since store migration 0004, so a revision claim
//     can never collide with it.
func (r *Repo) loadAndLockRevisionState(ctx context.Context, tx pgx.Tx, c *candidate) (*frozenState, string, error) {
	s := &frozenState{}
	if c.revSourceID == "" || c.revRecordID == "" || c.revRendition == "" || len(c.revJSON) == 0 {
		return nil, "REVISION_IDENTITY_MISSING", nil
	}
	var fr revision.SourceRevision
	if err := json.Unmarshal(c.revJSON, &fr); err != nil {
		return nil, "REVISION_JSON_INVALID", nil
	}
	if fr.SourceID != c.revSourceID || fr.Bibliography.RecordID != c.revRecordID || fr.RenditionID != c.revRendition {
		return nil, "REVISION_IDENTITY_MISMATCH", nil
	}

	// Same serialization against the sync's store-effect phase as the
	// legacy lane had against the canonical sync: the advisory xact lock
	// on the source key excludes the sync's projection writes (same
	// database, same key namespace).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(c.revSourceID)); err != nil {
		return nil, "", fmt.Errorf("acquire source lock: %w", err)
	}

	s.source = claimSourceRow{id: c.revSourceID}

	var parentKey, linkMode string
	err := tx.QueryRow(ctx, `
		SELECT document_id::text, attachment_id::text, server_id,
		       record_key, source_version, content_hash,
		       COALESCE(content_type,''), COALESCE(filename,''), COALESCE(local_path,''),
		       file_size, mtime_ms, COALESCE(link_mode,'imported_file'),
		       preferred, deleted
		FROM store_documents
		WHERE source_id = $1::uuid AND rendition_key = $2 FOR UPDATE`,
		c.revSourceID, c.revRendition).Scan(
		&s.document.id, &s.attachment.id, &s.source.serverID,
		&s.document.zoteroKey, &s.attachment.zoteroVersion, &s.attachment.contentHash,
		&s.attachment.contentType, &s.attachment.filename, &s.attachment.localPath,
		&s.attachment.fileSize, &s.attachment.mtimeMS, &linkMode,
		&s.attachment.preferred, &s.attachment.deleted)
	if err == pgx.ErrNoRows {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock rendition projection: %w", err)
	}
	// The rendition row carries document + attachment identity together
	// (one row per rendition — the chain check is structural).
	s.attachment.documentID = s.document.id
	s.attachment.sourceID = c.revSourceID
	s.attachment.zoteroKey = c.revRendition
	s.document.sourceID = c.revSourceID
	parentKey = s.document.zoteroKey
	s.document.parentKey, s.document.linkMode = &parentKey, &linkMode

	if s.attachment.deleted {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}
	// Preferred parity with completion (MarkCompletedTx requires the
	// preferred rendition): a non-preferred rendition must be obsoleted
	// HERE — otherwise it burns full processing runs and ends
	// LEASE_EXHAUSTED (claim allowing what completion forbids).
	if !s.attachment.preferred {
		return nil, "ATTACHMENT_NOT_PREFERRED", nil
	}

	// Hash currency: the revision's ContentHash is the intake truth. The
	// projection's current hash must agree (non-forced) — a moved-on
	// projection means a newer revision was published and this job is
	// stale.
	hash := s.attachment.contentHash
	if hash == nil || *hash == "" {
		return nil, "CONTENT_HASH_MISSING", nil
	}
	if !c.forceRebuild && fr.ContentHash != "" && fr.ContentHash != *hash {
		return nil, "CONTENT_HASH_CHANGED", nil
	}

	// The revision's own citation class rules the KG gate (contract truth;
	// the projection carries the intake-time class so both agree).
	if fr.Bibliography.CitationClass == "contextual" {
		s.document.contextual = true
	} else {
		s.document.contextual = false
	}
	s.document.zoteroVersion = s.attachment.zoteroVersion
	if bib, err := json.Marshal(fr.Bibliography); err == nil {
		s.document.rawData = bib // the publishable metadata this lane freezes
	}
	s.revision = &fr
	return s, "", nil
}

// buildRevisionFrozenInput assembles the revision lane's durable snapshot:
// the SAME FrozenInput wire the legacy lane froze (so the dispatcher's
// request build, the persist validation and the source-serving endpoint
// work unchanged), populated from the revision + the resolved projection
// row, PLUS the additive intake marker and revision block.
func buildRevisionFrozenInput(c *candidate, s *frozenState, proc FrozenProcessing, profileHash, idemKey string) []byte {
	fr := *s.revision
	fi := FrozenInput{
		ContractVersion: "1.0",
		Intake:          "revision",
		Revision:        s.revision,
		JobID:           c.id,
		IdempotencyKey:  idemKey,
		ProfileHash:     profileHash,
		Source: FrozenSource{
			Type:     "zotero", // transitional: the mirror source; F10's seam rename owns the vocabulary
			SourceID: s.source.id,
			ServerID: s.source.serverID,
		},
		Document: FrozenDocument{
			DocumentID:    s.document.id,
			ZoteroKey:     s.document.zoteroKey, // opaque external reference (the record key)
			ZoteroVersion: s.document.zoteroVersion,
			MetadataSnapshot: func() json.RawMessage {
				b, _ := json.Marshal(fr.Bibliography)
				return b
			}(),
		},
		Attachment: FrozenAttachment{
			AttachmentID:  s.attachment.id,
			ZoteroKey:     s.attachment.zoteroKey, // opaque external reference (the rendition key)
			ZoteroVersion: s.attachment.zoteroVersion,
			ParentKey:     derefStr(s.document.parentKey),
			LinkMode:      derefStr(s.document.linkMode),
			ContentType:   s.attachment.contentType,
			Filename:      s.attachment.filename,
			LocalPath:     s.attachment.localPath,
			ContentHash:   s.attachment.contentHash,
			SizeBytes:     s.attachment.fileSize,
			MtimeMS:       s.attachment.mtimeMS,
		},
		Processing: proc,
	}
	b, _ := json.Marshal(fi)
	return b
}
