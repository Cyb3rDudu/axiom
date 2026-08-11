// Frozen input snapshot for a claimed ingest job. The processor request built in
// Gate 2 is assembled exactly from this immutable snapshot, which is frozen in
// the same transaction as the claim. The structure mirrors the
// PROCESSOR_CONTRACT.md process-request shape so a dispatcher can deserialize it
// into a contract request without additional DB reads.
package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// FrozenInput is the durable, immutable snapshot stored in ingest_jobs.input_snapshot
// at claim time. It captures the source/document/attachment identity, all date
// facts and the full bibliographic metadata snapshot (losslessly from the
// canonical Zotero mirror), plus the processing identity. Field order follows the
// PROCESSOR_CONTRACT.md request so a dispatcher can deserialize the source/
// document/attachment/processing blocks directly; ProfileHash is snapshot
// IDENTITY (also stored in ingest_jobs.profile_hash), not part of the emit block.
type FrozenInput struct {
	ContractVersion string           `json:"contract_version"`
	JobID           string           `json:"job_id"`
	IdempotencyKey  string           `json:"idempotency_key"`
	ProfileHash     string           `json:"profile_hash"`
	Source          FrozenSource     `json:"source"`
	Document        FrozenDocument   `json:"document"`
	Attachment      FrozenAttachment `json:"attachment"`
	Processing      FrozenProcessing `json:"processing"`
}

// FrozenSource identifies the canonical Zotero source.
type FrozenSource struct {
	Type     string  `json:"type"`
	SourceID string  `json:"source_id"`
	ServerID *string `json:"server_id"`
}

// FrozenDocument identifies the document projection and carries the full
// publishable metadata snapshot.
type FrozenDocument struct {
	DocumentID       string          `json:"document_id"`
	ZoteroKey        string          `json:"zotero_key"`
	ZoteroVersion    int64           `json:"zotero_version"`
	MetadataSnapshot json.RawMessage `json:"metadata_snapshot"`
}

// FrozenAttachment carries the processable file identity and facts.
type FrozenAttachment struct {
	AttachmentID  string  `json:"attachment_id"`
	ZoteroKey     string  `json:"zotero_key"`
	ZoteroVersion int64   `json:"zotero_version"`
	ParentKey     string  `json:"parent_zotero_key,omitempty"`
	LinkMode      string  `json:"link_mode,omitempty"`
	ContentType   *string `json:"content_type"`
	Filename      *string `json:"filename"`
	LocalPath     *string `json:"local_path"`
	ContentHash   *string `json:"content_hash"`
	SizeBytes     *int64  `json:"size_bytes"`
	MtimeMS       *int64  `json:"mtime_ms"`
}

// FrozenProcessing is the processing block exactly as PROCESSOR_CONTRACT.md
// defines it: `profile` is the profile NAME (a flat string) with sibling feature
// flags, and NO profile_hash (that is snapshot identity, not wire). The
// dispatcher can deserialize this snapshot's processing block directly into a
// contract request.
type FrozenProcessing struct {
	Profile                 string `json:"profile"`
	ForceRebuild            bool   `json:"force_rebuild"`
	LanguageHint            string `json:"language_hint,omitempty"`
	ExtractImages           bool   `json:"extract_images"`
	ComputeDenseEmbeddings  bool   `json:"compute_dense_embeddings"`
	ComputeSparseEmbeddings bool   `json:"compute_sparse_embeddings"`
	ExtractEntities         bool   `json:"extract_entities"`
	ExtractRelationships    bool   `json:"extract_relationships"`
}

// decodeProcessing strictly decodes a profile JSON object into FrozenProcessing:
// unknown keys and wrong-typed values are REJECTED (not silently ignored or
// coerced), so the canonical form below — and therefore the profile hash and the
// emitted request — cannot diverge from what will actually run.
func decodeProcessing(profile []byte) (FrozenProcessing, error) {
	var p FrozenProcessing
	if len(profile) == 0 {
		return p, errors.New("profile is required for a claim")
	}
	dec := json.NewDecoder(bytes.NewReader(profile))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid processing profile: %w", err)
	}
	if p.Profile == "" {
		return p, errors.New("profile object has no \"profile\" name field")
	}
	return p, nil
}

// profileCanonical serializes a decoded FrozenProcessing to a stable, deterministic
// canonical JSON form (struct field order) suitable for hashing and storage as
// processing_profile. It returns both the canonical bytes and their SHA-256.
func profileCanonical(profile []byte) ([]byte, string, error) {
	p, err := decodeProcessing(profile)
	if err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, "", fmt.Errorf("serialize processing profile: %w", err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// metadataSnapshot returns the lossless canonical zotero_items.raw_data AS-IS.
// The caller is responsible for ensuring the canonical item exists and is active;
// when rawData is empty (no canonical item/raw metadata), metadataSnapshot returns
// nil so the caller must skip the job (CANONICAL_METADATA_MISSING) rather than
// silently build a lossy projection. Zotero is the source of truth; missing,
// deleted or drifted canonical metadata is never replaced by a runtime fallback.
func metadataSnapshot(rawData json.RawMessage) json.RawMessage {
	if len(rawData) > 0 {
		return rawData
	}
	return nil
}

// idempotencyKey derives the processor idempotency key from the frozen identity.
// For a normal job it is attachment_id:frozen_content_hash:profile_hash, so an
// unchanged source reuses the processor result. A force rebuild deliberately
// changes content and/or identity and must never reuse an old processor result,
// so the job_id (unique per rebuild job) is folded in: two separate force jobs
// for the same attachment/hash/profile therefore always get distinct keys.
func idempotencyKey(jobID, attachmentID string, contentHash *string, profileHash string, forceRebuild bool) string {
	h := "<nil>"
	if contentHash != nil && *contentHash != "" {
		h = *contentHash
	}
	if forceRebuild {
		return fmt.Sprintf("%s:%s:%s:force-%s", attachmentID, h, profileHash, jobID)
	}
	return fmt.Sprintf("%s:%s:%s", attachmentID, h, profileHash)
}
