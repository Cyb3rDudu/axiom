package dispatcher

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
)

// frozenInput matches the frozen snapshot schema the claim persisted (see
// internal/repo/frozen.go FrozenInput). It exists here so the dispatcher builds a
// wire processor.ProcessRequest without importing the repo's internal type and
// without ever serializing profile_hash (which is snapshot identity, not wire).
type frozenInput struct {
	ContractVersion string               `json:"contract_version"`
	JobID           string               `json:"job_id"`
	IdempotencyKey  string               `json:"idempotency_key"`
	Source          frozenSource         `json:"source"`
	Document        frozenDocument       `json:"document"`
	Attachment      frozenAttachment     `json:"attachment"`
	Processing      processor.Processing `json:"processing"`
}

// SourceURLOptions carries the remote source-delivery config for one request
// build: the externally reachable base URL of axiom-ng's source endpoint, the
// shared HMAC secret, and the current claim's lease expiry (the URL's exp).
// Zero value (or empty BaseURL/Secret) builds no source_url — local delivery.
type SourceURLOptions struct {
	BaseURL    string
	Secret     string
	LeaseUntil time.Time
}

type frozenSource struct {
	Type     string `json:"type"`
	SourceID string `json:"source_id"`
	ServerID string `json:"server_id"`
}

type frozenDocument struct {
	DocumentID       string          `json:"document_id"`
	ZoteroKey        string          `json:"zotero_key"`
	ZoteroVersion    int64           `json:"zotero_version"`
	MetadataSnapshot json.RawMessage `json:"metadata_snapshot"`
}

type frozenAttachment struct {
	AttachmentID  string  `json:"attachment_id"`
	ZoteroKey     string  `json:"zotero_key"`
	ZoteroVersion int64   `json:"zotero_version"`
	ContentType   *string `json:"content_type"`
	Filename      *string `json:"filename"`
	LocalPath     *string `json:"local_path"`
	ContentHash   *string `json:"content_hash"`
	SizeBytes     *int64  `json:"size_bytes"`
	MtimeMS       *int64  `json:"mtime_ms"`
}

// ErrNotProcessable indicates the frozen snapshot is missing a field required to
// build a valid process request (e.g. no content hash / local path for a file).
type ErrNotProcessable struct{ Reason string }

func (e *ErrNotProcessable) Error() string { return "not processable: " + e.Reason }

// buildRequest converts a frozen input snapshot into a processor.ProcessRequest.
// The wire result intentionally carries no profile_hash. Empty-pointer optional
// attachment fields are mapped to zero values; a missing content_hash or
// local_path makes the job not processable (the contract requires both).
func buildRequest(raw json.RawMessage, sourceOpts SourceURLOptions) (*processor.ProcessRequest, error) {
	var f frozenInput
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse frozen input snapshot: %w", err)
	}
	if f.JobID == "" {
		return nil, &ErrNotProcessable{Reason: "missing job_id"}
	}
	if f.IdempotencyKey == "" {
		return nil, &ErrNotProcessable{Reason: "missing idempotency_key"}
	}
	if f.Processing.Profile == "" {
		return nil, &ErrNotProcessable{Reason: "missing processing profile"}
	}
	if a := f.Attachment; a.ContentHash == nil || *a.ContentHash == "" {
		return nil, &ErrNotProcessable{Reason: "missing attachment content_hash"}
	}
	// Remote source delivery: sign jobID|lease-exp into a download URL the
	// runner can pull. Requires secret + base URL + a live lease; a half
	// configuration (base without secret) builds NO url so the guard below
	// fails instead of shipping an unreadable local_path.
	var sourceURL string
	if sourceOpts.BaseURL != "" && sourceOpts.Secret != "" && !sourceOpts.LeaseUntil.IsZero() {
		exp := sourceOpts.LeaseUntil.Unix()
		sourceURL = sourceurl.BuildURL(sourceOpts.BaseURL, f.JobID, exp,
			sourceurl.Sign(sourceOpts.Secret, f.JobID, exp))
	}
	if (f.Attachment.LocalPath == nil || *f.Attachment.LocalPath == "") && sourceURL == "" {
		// No local path AND no (complete) remote delivery => not processable.
		return nil, &ErrNotProcessable{Reason: "missing attachment local_path"}
	}
	req := &processor.ProcessRequest{
		ContractVersion: contractVersionOf(f.ContractVersion),
		JobID:           f.JobID,
		IdempotencyKey:  f.IdempotencyKey,
		Source: processor.Source{
			Type:     f.Source.Type,
			SourceID: f.Source.SourceID,
			ServerID: f.Source.ServerID,
		},
		Document: processor.Document{
			DocumentID:       f.Document.DocumentID,
			ZoteroKey:        f.Document.ZoteroKey,
			ZoteroVersion:    f.Document.ZoteroVersion,
			MetadataSnapshot: f.Document.MetadataSnapshot,
		},
		Attachment: processor.Attachment{
			AttachmentID:  f.Attachment.AttachmentID,
			ZoteroKey:     f.Attachment.ZoteroKey,
			ZoteroVersion: f.Attachment.ZoteroVersion,
			ContentType:   derefString(f.Attachment.ContentType),
			Filename:      derefString(f.Attachment.Filename),
			LocalPath:     derefString(f.Attachment.LocalPath),
			SourceURL:     sourceURL, // empty unless remote delivery is configured
			ContentHash:   derefString(f.Attachment.ContentHash),
			SizeBytes:     derefInt64(f.Attachment.SizeBytes),
			MtimeMS:       derefInt64(f.Attachment.MtimeMS),
		},
		Processing: f.Processing,
	}
	return req, nil
}

func contractVersionOf(v string) string {
	if v == "" {
		return processor.ContractVersion
	}
	return v
}

// metadataTitle extracts the document title from the Zotero metadata snapshot
// (#167, B3 field). Best-effort: an empty or malformed snapshot yields "".
func metadataTitle(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Title
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
