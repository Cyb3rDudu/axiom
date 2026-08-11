// Frozen input snapshot for a claimed ingest job. The processor request built in
// Gate 2 is assembled exactly from this immutable snapshot, which is frozen in
// the same transaction as the claim. The structure mirrors the
// PROCESSOR_CONTRACT.md process-request shape so a dispatcher can deserialize it
// into a contract request without additional DB reads.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// FrozenInput is the durable, immutable snapshot stored in ingest_jobs.input_snapshot
// at claim time. It captures the source/document/attachment identity, all date
// facts and the full bibliographic metadata snapshot (losslessly from the
// canonical Zotero mirror), plus the processing identity.
type FrozenInput struct {
	ContractVersion string           `json:"contract_version"`
	JobID           string           `json:"job_id"`
	IdempotencyKey  string           `json:"idempotency_key"`
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

// FrozenProcessing records the processing identity frozen at claim time so the
// dispatcher can assemble the contract request's processing block. Profile is the
// structured profile object (e.g. {"profile":"full-rag-v1",...flags}), stored as
// JSON (not a stringified string) so a dispatcher can read its fields.
type FrozenProcessing struct {
	Profile      json.RawMessage `json:"profile"`
	ForceRebuild bool            `json:"force_rebuild"`
	ProfileHash  string          `json:"profile_hash"`
}

// metadataSnapshot builds the metadata_snapshot JSON for a claimed document. If
// the canonical mirror has the document's raw_data it is returned AS-IS
// (lossless: typed creators, typed tags, collections and every Zotero field are
// preserved, and nothing is overwritten by a normalized projection). When there
// is no canonical item the normalized projection columns are used as a
// best-effort snapshot. In both cases missing values stay missing (NULL preserved)
// and the snapshot is never enriched or guessed.
func metadataSnapshot(rawData json.RawMessage, doc zoteroDocFacts) json.RawMessage {
	if len(rawData) > 0 {
		return rawData
	}

	merged := map[string]any{}
	putStr := func(k string, v *string) {
		if v != nil {
			merged[k] = *v
		}
	}
	putInt := func(k string, v *int) {
		if v != nil {
			merged[k] = *v
		}
	}
	if doc.ItemType != "" {
		merged["itemType"] = doc.ItemType
	}
	putStr("title", doc.Title)
	if doc.Creators != nil {
		merged["creators"] = doc.Creators
	}
	putStr("abstract_note", doc.Abstract)
	putInt("publication_year", doc.PublicationYear)
	putStr("publication_date", doc.PublicationDate)
	putStr("publisher", doc.Publisher)
	putStr("isbn", doc.ISBN)
	putStr("doi", doc.DOI)
	putStr("url", doc.URL)
	putStr("language", doc.Language)
	if doc.Tags != nil {
		merged["tags"] = doc.Tags
	}
	if doc.Collections != nil {
		merged["collections"] = doc.Collections
	}
	if doc.Metadata != nil {
		// edition/volume/issue/pages/issn/extra/relations live in metadata JSONB.
		for k, v := range doc.Metadata {
			if v != nil {
				merged[k] = v
			}
		}
	}
	b, _ := json.Marshal(merged)
	return b
}

// zoteroDocFacts carries the normalized document projection fields needed to
// build a complete metadata_snapshot.
type zoteroDocFacts struct {
	ItemType        string
	Title           *string
	Creators        json.RawMessage
	Abstract        *string
	PublicationYear *int
	PublicationDate *string
	Publisher       *string
	ISBN            *string
	DOI             *string
	URL             *string
	Language        *string
	Tags            json.RawMessage
	Collections     json.RawMessage
	Metadata        map[string]any
}

// canonicalProfile deterministically canonicalizes a processing profile so its
// SHA-256 can be derived without trusting a caller-supplied hash. It marshals a
// compact, key-sorted JSON form.
func canonicalProfile(profile json.RawMessage) (string, error) {
	if len(profile) == 0 {
		return "", errors.New("profile is required for a claim")
	}
	var v any
	if err := json.Unmarshal(profile, &v); err != nil {
		return "", fmt.Errorf("profile is not valid JSON: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("canonicalize profile: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
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
