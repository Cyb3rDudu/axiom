// Package library is the public Library component contract (F03, #297;
// implemented by F06 #300, bound local/HTTP by F11 #305).
//
// Library owns all bibliographic/provider truth: sources, records,
// renditions, imports. The interface is transport-neutral — signatures
// speak only DTOs from this package, its sibling contract packages, and
// the typed error world. Import rules (lint-enforced, internal/contracts):
// nothing from internal/db, nothing HTTP-ish, nothing Zotero-ish.
//
// Error semantics: every method returns *contracterr.Error (or
// *contracterr.IdempotencyMismatch) for expected failure classes;
// implementations map their internals into these — consumers never parse
// error strings.
package library

import (
	"context"
	"io"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
)

// ContractVersion freezes this package's DTO wire format. See
// revision.ContractVersion for the policy.
const ContractVersion = "v1"

// Library is the seam every Library implementation (in-process F06,
// HTTP-backed F11, test fakes) must satisfy.
type Library interface {
	// GetSource resolves one configured source. Unknown refs are
	// NotFound; blank refs InvalidArgument.
	GetSource(ctx context.Context, ref SourceRef) (Source, error)

	// StartImport begins a document intake. req carries the bibliographic
	// request plus the idempotency key; content is the rendition byte
	// stream (format is derived from content/magic bytes ONLY — never
	// from a declared extension or MIME header, so no media type rides
	// the request). Returns the initial ImportOperation; the operation
	// runs asynchronously — poll GetImport to a terminal status.
	//
	// Idempotency: same key + byte-identical request (metadata JSON and
	// content) → the SAME ImportOperation (replay, no side effects).
	// Same key + different payload → *contracterr.IdempotencyMismatch.
	// Precedence: request validation (including the content/magic-byte
	// checks) precedes idempotency evaluation — a reused key with
	// invalid input reports InvalidArgument, never IdempotencyMismatch.
	StartImport(ctx context.Context, req ImportRequest, content io.Reader) (ImportOperation, error)

	// GetImport reports the current ImportOperation state. Unknown ids
	// NotFound.
	GetImport(ctx context.Context, ref ImportRef) (ImportOperation, error)

	// OpenRendition redeems a ContentTicket from a SourceRevision and
	// returns the rendition bytes. Callers MUST verify the stream
	// against the revision's ContentHash and close the stream. Unknown
	// or expired tickets are NotFound. Tickets stay redeemable across
	// ingest retries — single-purpose means rendition-scoped, not
	// one-shot.
	OpenRendition(ctx context.Context, ticket ContentTicket) (io.ReadCloser, error)

	// ProjectCitation composes the citation projection (in-text +
	// reference form) for a record at a locator, per the record's
	// citation class and the revision's locator capabilities. Unknown
	// records NotFound; unusable locator/kind combinations
	// InvalidArgument.
	ProjectCitation(ctx context.Context, req CitationRequest) (CitationProjection, error)
}

// SourceRef identifies a source across the seam.
type SourceRef struct {
	SourceID string `json:"source_id"`
}

// ImportRef identifies an import operation.
type ImportRef struct {
	ImportID string `json:"import_id"`
}

// ContentTicket is the opaque rendition grant carried by a
// revision.SourceRevision (single-purpose capability token).
type ContentTicket string

// Source is a configured Library source (a bibliographic provider
// connection). Optionalität: SyncedAt is *time — nil means "never
// synced" (never zero-time-as-never); Provider/LibraryID are plain
// strings, empty is not expected from a real implementation but not an
// error either.
type Source struct {
	SourceID  string     `json:"source_id"`
	Provider  string     `json:"provider"`
	LibraryID string     `json:"library_id"`
	SyncedAt  *time.Time `json:"synced_at,omitempty"`
}

// ImportRequest is the metadata part of a document intake (the JSON
// "request" part of F06's multipart contract — content travels as the
// separate StartImport stream, hence no content field here).
//
// Optionalität: Source is a pointer — nil for non-web intake; for
// record_type "webpage" it is required (OriginalURL non-empty).
// MetadataHints fields are plain strings, empty = no hint.
// Enrichment fields are *bool — nil means the default (true); an explicit
// false opts the caller out (a bare bool could not distinguish the two).
type ImportRequest struct {
	IdempotencyKey string            `json:"idempotency_key"`
	RecordType     string            `json:"record_type"`
	Target         ImportTarget      `json:"target"`
	Source         *ImportSourceInfo `json:"source,omitempty"`
	MetadataHints  MetadataHints     `json:"metadata_hints"` // always present (structs ignore omitempty)
	Enrichment     EnrichmentFlags   `json:"enrichment"`
}

// ImportTarget says where the imported record lands. Exactly one of
// CollectionID / CollectionPath must be set (XOR) unless neither is (file
// lands unfiled); CollectionPath is hierarchical, parent-first.
type ImportTarget struct {
	LibraryID      string   `json:"library_id"`
	CollectionID   string   `json:"collection_id,omitempty"`
	CollectionPath []string `json:"collection_path,omitempty"`
	// CreateMissing: create absent CollectionPath segments (opt-in,
	// default false — a typo must never grow a collection tree).
	CreateMissing bool `json:"create_missing"`
}

// ImportSourceInfo is the origin capture for web-sourced intake.
// AccessedAt/CapturedAt are pointers: nil = not captured (never
// zero-time).
type ImportSourceInfo struct {
	OriginalURL string     `json:"original_url"`
	AccessedAt  *time.Time `json:"accessed_at,omitempty"`
	CapturedAt  *time.Time `json:"captured_at,omitempty"`
}

// MetadataHints are optional caller-supplied identifiers that steer (but
// never override) metadata resolution.
type MetadataHints struct {
	DOI   string `json:"doi,omitempty"`
	ISBN  string `json:"isbn,omitempty"`
	Title string `json:"title,omitempty"`
}

// EnrichmentFlags toggles external metadata resolvers. Nil pointer =
// default ON (see ImportRequest.Optionalität).
type EnrichmentFlags struct {
	Crossref    *bool `json:"crossref,omitempty"`
	OpenLibrary *bool `json:"open_library,omitempty"`
}

// ImportStatus is the durable import state machine (F06 vocabulary).
// awaiting_* states pause for a caller decision (confirm/retry rides the
// F06 HTTP surface; the F03 interface is read/state-only).
type ImportStatus string

const (
	ImportReceived            ImportStatus = "received"
	ImportInspecting          ImportStatus = "inspecting"
	ImportResolvingMetadata   ImportStatus = "resolving_metadata"
	ImportAwaitingConfirm     ImportStatus = "awaiting_confirmation"
	ImportEnsuringCollections ImportStatus = "ensuring_collections"
	ImportCreatingRecord      ImportStatus = "creating_record"
	ImportUploadingRendition  ImportStatus = "uploading_rendition"
	ImportVerifying           ImportStatus = "verifying"
	ImportCommitted           ImportStatus = "committed"
	ImportRetryableFailed     ImportStatus = "retryable_failed"
	ImportTerminalFailed      ImportStatus = "terminal_failed"
)

// Terminal reports whether the state machine left the running set
// (committed or failed). Pollers stop here.
func (s ImportStatus) Terminal() bool {
	return s == ImportCommitted || s == ImportRetryableFailed || s == ImportTerminalFailed
}

// ImportCandidate is one resolution candidate offered for a decision.
type ImportCandidate struct {
	CandidateID string `json:"candidate_id"`
	// Origin: where the candidate came from — "document" | "crossref" |
	// "open_library" | "provider_existing".
	Origin  string `json:"origin"`
	Summary string `json:"summary"`
}

// ImportDecision is an open question the operation waits on (non-empty
// Decisions iff status awaiting_confirmation).
type ImportDecision struct {
	DecisionID string            `json:"decision_id"`
	Subject    string            `json:"subject"`
	Candidates []ImportCandidate `json:"candidates"`
}

// ImportResult is the committed outcome. Revision is the published
// SourceRevision — the artifact Store ingests (the Library→Store bridge).
type ImportResult struct {
	RecordID    string                  `json:"record_id"`
	RenditionID string                  `json:"rendition_id"`
	Revision    revision.SourceRevision `json:"revision"`
}

// ImportFailure is the terminal/retryable failure detail. Retryability
// lives in the status (retryable_failed vs terminal_failed), not a bool
// here — one source of truth.
type ImportFailure struct {
	// Code is a stable machine code (e.g. "SOURCE_NOT_FOUND",
	// "CONVERSION_FAILED") — the only string callers may branch on.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImportOperation is the state of one intake at a point in time.
// Optionalität: exactly one of Result (committed) / Failure (*_failed) is
// set; Decisions is non-empty iff awaiting_confirmation. Fields is the
// per-field provenance trail (F06, additive v1: absent on operations
// that carry no provenance — existing wire forms stay byte-identical).
type ImportOperation struct {
	ImportID  string            `json:"import_id"`
	Status    ImportStatus      `json:"status"`
	Decisions []ImportDecision  `json:"decisions,omitempty"`
	Result    *ImportResult     `json:"result,omitempty"`
	Failure   *ImportFailure    `json:"failure,omitempty"`
	Fields    []FieldProvenance `json:"fields,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"` // UTC, RFC3339 µs (DM03-compatible)
}

// FieldProvenance is one rung contribution for one normalized field —
// the verify-ladder audit trail (F06). Applied rows carry the effective
// value's source; rejected rows (Applied=false) document the attempt a
// weaker rung made on an already-verified field.
type FieldProvenance struct {
	Field           string  `json:"field"`
	Source          string  `json:"source"` // document | identifier | crossref | open_library | provider_existing | user
	ResolverVersion string  `json:"resolver_version,omitempty"`
	Confidence      float64 `json:"confidence"`
	Applied         bool    `json:"applied"`
}

// CitationRequest asks for a citation projection.
type CitationRequest struct {
	RecordID string          `json:"record_id"`
	Locator  CitationLocator `json:"locator"`
	// Style: "" means the default ("apa-7").
	Style string `json:"style,omitempty"`
}

// CitationLocator is the citation-side locator: where in the record the
// cited passage sits. Distinct from store's rendered hit LocatorView by
// design — this is a Library-owned input vocabulary (what a citation may
// address), that is a Store-rendered output view.
//
// Optionalität: pointer fields mean "not applicable to Kind"; string
// fields empty = absent. Kind drives which are required (validated by
// the implementation per the revision's locator capabilities).
type CitationLocator struct {
	// Kind: "page" | "epub_cfi".
	Kind          string `json:"kind"`
	PageStart     *int   `json:"page_start,omitempty"`
	PageEnd       *int   `json:"page_end,omitempty"`
	Chapter       string `json:"chapter,omitempty"`
	ChapterNumber *int   `json:"chapter_number,omitempty"`
	SectionTitle  string `json:"section_title,omitempty"`
	// ParagraphInChapter: 1-based APA-7 paragraph count from chapter
	// start.
	ParagraphInChapter *int   `json:"paragraph_in_chapter,omitempty"`
	CFI                string `json:"cfi,omitempty"`
	// PageSource: trust level for "page" locators (revision.Trust*
	// constants) — which page citation form is honest.
	PageSource string `json:"page_source,omitempty"`
}

// CitationProjection is the composed citation: Citation the in-text form
// ("(Mueller, 2019, S. 47)"), Reference the bibliography-list entry.
// Style echoes the effective style. Locator echoes the input verbatim.
type CitationProjection struct {
	RecordID  string          `json:"record_id"`
	Citation  string          `json:"citation"`
	Reference string          `json:"reference"`
	Style     string          `json:"style"`
	Locator   CitationLocator `json:"locator"`
}
