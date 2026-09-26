// intake_claim.go — the revision-lane claim (F09 #303): resolves a
// revision-typed job's opaque identities against the Zotero mirror (the
// documented dual-read; DM06/F12 abate it) and freezes a revision-typed
// input snapshot. Zotero keys stay opaque external references throughout;
// the revision's OWN truth (hash, bibliography, capabilities, ticket) is
// the frozen contract artifact, the mirror contributes only the
// transitional FK uuids + the file facts the processor wire still needs.
package repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/jackc/pgx/v5"
)

// loadAndLockRevisionState is the revision lane of loadAndLockState: same
// locking discipline (per-source advisory lock FIRST, then rows in fixed
// order source → document → attachment, FOR UPDATE), same obsolescence
// contract (non-empty reason = skip). Differences from the legacy lane:
//
//   - the document/attachment rows resolve by the revision's OPAQUE ids
//     (source uuid + record/rendition keys), not by enqueue-time FKs;
//   - the hash-stale check compares the mirror against the REVISION's
//     hash (the revision is the intake truth; a mirror that moved on
//     means a newer revision exists and this job is stale);
//   - the citation class (contextual KG gate) comes from the revision's
//     bibliography — no canonical-item read: the revision IS the metadata
//     source for this lane (lossless by contract, not by mirror lookup).
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

	// Same serialization against the canonical sync as the legacy lane.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey(c.revSourceID)); err != nil {
		return nil, "", fmt.Errorf("acquire source lock: %w", err)
	}

	var serverID *string
	err := tx.QueryRow(ctx, `
		SELECT server_id FROM zotero_sources WHERE id=$1::uuid FOR UPDATE`, c.revSourceID).Scan(&serverID)
	if err == pgx.ErrNoRows {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock source: %w", err)
	}
	s.source = zoteroSourceRow{id: c.revSourceID, serverID: serverID}

	var docParentKey, docLinkMode *string
	err = tx.QueryRow(ctx, `
		SELECT id::text, source_id::text, zotero_key, zotero_version, canonical_item_id::text, deleted,
		       COALESCE(citation_class,'citable') = 'contextual'
		FROM zotero_documents
		WHERE source_id = $1::uuid AND zotero_key = $2 AND deleted = false FOR UPDATE`,
		c.revSourceID, c.revRecordID).Scan(
		&s.document.id, &s.document.sourceID, &s.document.zoteroKey, &s.document.zoteroVersion,
		&s.document.canonicalItemID, &s.document.deleted, &s.document.contextual)
	if err == pgx.ErrNoRows {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock document: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT id::text, source_id::text, document_id::text, zotero_key, zotero_version, content_type, filename, local_path,
		       content_hash, file_size, mtime_ms, preferred, deleted, parent_zotero_key, link_mode
		FROM zotero_attachments
		WHERE source_id = $1::uuid AND zotero_key = $2 AND deleted = false FOR UPDATE`,
		c.revSourceID, c.revRendition).Scan(
		&s.attachment.id, &s.attachment.sourceID, &s.attachment.documentID, &s.attachment.zoteroKey, &s.attachment.zoteroVersion,
		&s.attachment.contentType, &s.attachment.filename, &s.attachment.localPath,
		&s.attachment.contentHash, &s.attachment.fileSize, &s.attachment.mtimeMS,
		&s.attachment.preferred, &s.attachment.deleted,
		&docParentKey, &docLinkMode)
	if err == pgx.ErrNoRows {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("lock attachment: %w", err)
	}
	// The rendition must belong to the revision's record.
	if s.attachment.documentID != s.document.id || docParentKey == nil || *docParentKey != s.document.zoteroKey {
		return nil, "REVISION_REF_UNRESOLVED", nil
	}

	// Hash currency: the revision's ContentHash is the intake truth. The
	// mirror's current hash must agree (non-forced) — a moved-on mirror
	// means a newer revision was published and this job is stale.
	hash := s.attachment.contentHash
	if hash == nil || *hash == "" {
		return nil, "CONTENT_HASH_MISSING", nil
	}
	if !c.forceRebuild && fr.ContentHash != "" && fr.ContentHash != *hash {
		return nil, "CONTENT_HASH_CHANGED", nil
	}
	s.document.parentKey, s.document.linkMode = docParentKey, docLinkMode
	// The revision's own citation class rules the KG gate (contract truth;
	// F07 derives the ledger class from the mirror so both agree — reading
	// it from the revision keeps the lane mirror-honest even before F12).
	if fr.Bibliography.CitationClass == "contextual" {
		s.document.contextual = true
	} else {
		s.document.contextual = false
	}
	if bib, err := json.Marshal(fr.Bibliography); err == nil {
		s.document.rawData = bib // the publishable metadata this lane freezes
	}
	return s, "", nil
}

// buildRevisionFrozenInput assembles the revision lane's durable snapshot:
// the SAME FrozenInput wire the legacy lane freezes (so the dispatcher's
// request build, the persist validation and the source-serving endpoint
// work unchanged), populated from the revision + the resolved mirror rows,
// PLUS the additive intake marker and revision block.
func buildRevisionFrozenInput(c *candidate, s *frozenState, proc FrozenProcessing, profileHash, idemKey string, fr revision.SourceRevision) []byte {
	fi := FrozenInput{
		ContractVersion: "1.0",
		Intake:          "revision",
		Revision:        &fr,
		JobID:           c.id,
		IdempotencyKey:  idemKey,
		ProfileHash:     profileHash,
		Source: FrozenSource{
			Type:     "zotero", // transitional: the mirror source; F10's seam rename owns the vocabulary
			SourceID: s.source.id,
			ServerID: s.source.serverID,
		},
		Document: FrozenDocument{
			DocumentID:       s.document.id,
			ZoteroKey:        s.document.zoteroKey, // opaque external reference (the record key)
			ZoteroVersion:    s.document.zoteroVersion,
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
