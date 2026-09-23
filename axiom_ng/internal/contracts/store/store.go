// Package store is the public Store component contract (F03, #297;
// implemented by F09 #303, bound local/HTTP by F11 #305).
//
// Store owns all processed-corpus truth: revision intake, snapshots,
// chunks, embeddings, search and passages. Intake accepts versioned
// SourceRevisions only (revision package) — Zotero/provider identities
// are opaque external references here. Transport-neutral like its
// Library sibling: DTOs + typed errors only, nothing from internal/db,
// HTTP, or provider code (lint-enforced).
//
// The Search/Passage DTOs mirror the frozen v0.1.18 public API field
// names (F01 goldens) so the F11 HTTP adapter can serialize these DTOs
// without breaking the baseline — with exactly two deliberate ADR-0001
// renames: doc_id → record_id (revision.Bibliography.RecordID) and
// attachment_id → rendition_id (Passage.RenditionID). The public-facing
// adapter layer owns that field-name translation; these DTOs are the
// component-internal wire. Optionalität here is witness-locked —
// additive changes only, with a golden update in the same PR.
package store

import (
	"context"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
)

// ContractVersion freezes this package's DTO wire format. See
// revision.ContractVersion for the policy.
const ContractVersion = "v1"

// MaxTopN is the retrieval overfetch ceiling (frozen v0.1.18 behavior:
// top_n above the rerank cap is rejected). Contract-stable; a change is
// a golden-touching contract change, not a tuning knob.
const MaxTopN = 64

// Store is the seam every Store implementation (in-process F09,
// HTTP-backed F11, test fakes) must satisfy.
type Store interface {
	// IngestRevision starts durable intake of one source revision. The
	// revision's ContentTicket is how the implementation fetches bytes
	// (via the Library seam) — fetching is an implementation concern,
	// not part of this signature. Returns the initial IngestJob; the job
	// runs asynchronously — eventual visibility through Search is the
	// terminal observable (v1 deliberately has no job-status poll
	// method). A Store MAY choose to return jobs already committed for
	// small synchronous implementations.
	//
	// Idempotency: same key + identical revision (canonical JSON of the
	// DTO) → the SAME IngestJob (replay, no side effects). Same key +
	// different revision → *contracterr.IdempotencyMismatch. An invalid
	// revision (revision.Validate) → InvalidArgument. Content fetched
	// via the ticket that does not hash to the revision's ContentHash →
	// Conflict (the revision describes bytes the Library cannot serve).
	// Precedence: revision validation precedes idempotency evaluation,
	// which precedes content verification — a reused key with an
	// invalid revision reports InvalidArgument; a reused key with a
	// diverged-but-valid revision reports IdempotencyMismatch, never
	// the hash Conflict.
	IngestRevision(ctx context.Context, req IngestRevisionRequest) (IngestJob, error)

	// Search runs hybrid retrieval. Blank query or top_n above the
	// overfetch cap → InvalidArgument; top_n <= 0 → default (10).
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)

	// GetPassage resolves one chunk with its ±1 neighbors and unified
	// source block. Unknown chunk ids NotFound.
	GetPassage(ctx context.Context, ref PassageRef) (Passage, error)
}

// IngestRevisionRequest is the Store intake request.
type IngestRevisionRequest struct {
	IdempotencyKey string                  `json:"idempotency_key"`
	Revision       revision.SourceRevision `json:"revision"`
}

// IngestStatus is the Store-side intake state machine. F09 maps its
// internal job states (pending/claimed/processing/…) onto these.
type IngestStatus string

const (
	IngestReceived        IngestStatus = "received"
	IngestProcessing      IngestStatus = "processing"
	IngestCommitted       IngestStatus = "committed"
	IngestRetryableFailed IngestStatus = "retryable_failed"
	IngestTerminalFailed  IngestStatus = "terminal_failed"
)

// Terminal reports whether the job left the running set.
func (s IngestStatus) Terminal() bool {
	return s == IngestCommitted || s == IngestRetryableFailed || s == IngestTerminalFailed
}

// IngestFailure mirrors library.ImportFailure semantics: a stable
// machine Code plus a human Message; retryability is the status, not a
// bool.
type IngestFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IngestJob is the intake state at a point in time. RevisionID and
// ContentHash echo the ingested revision (what callers poll with and
// verify against). Optionalität: Failure is non-nil iff status is
// *_failed.
type IngestJob struct {
	JobID       string         `json:"job_id"`
	Status      IngestStatus   `json:"status"`
	RevisionID  string         `json:"revision_id"`
	ContentHash string         `json:"content_hash"`
	Attempt     int            `json:"attempt"`
	MaxAttempts int            `json:"max_attempts"`
	Failure     *IngestFailure `json:"failure,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at"` // UTC, RFC3339 µs (DM03-compatible)
}

// SearchRequest is the retrieval request (frozen public shape).
type SearchRequest struct {
	Query string `json:"query"`
	TopN  int    `json:"top_n"`
	// Filters is optional (nil = none).
	Filters *SearchFilters `json:"filters,omitempty"`
}

// SearchFilters narrows all recall arms (nil DocumentIDs = no filter).
type SearchFilters struct {
	// DocumentIDs filters by the record identity — Source.RecordID /
	// Passage.DocumentID, the same id under both names.
	DocumentIDs []string `json:"document_ids,omitempty"`
}

// SearchArms records which recall arms contributed.
type SearchArms struct {
	Dense  bool `json:"dense"`
	BM25   bool `json:"bm25"`
	Sparse bool `json:"sparse,omitempty"`
}

// Image is one chunk image with its captions (frozen public shape):
// Ref the durable artifact ref, Marker the filename as the text marker
// carries it (empty when positional resolution was unsafe), captions
// machine (never citable) vs figure (from the document, citable).
type Image struct {
	Ref            string `json:"ref"`
	Marker         string `json:"marker,omitempty"`
	MachineCaption string `json:"machine_caption,omitempty"`
	FigureCaption  string `json:"figure_caption,omitempty"`
}

// Source is the bibliographic provenance block on hits and passages:
// the revision's Bibliography (hydrated verbatim at intake) plus the
// format factor the client actually sees. ContentType is ALWAYS present
// (no omitempty — an absent key would be indistinguishable from an old
// server; empty string is the honest "unknown"), mirroring the frozen
// public contract.
type Source struct {
	revision.Bibliography
	ContentType string `json:"content_type"`
}

// Locator is the human-usable rendered locator (frozen public shape:
// kind/label/chapter/cfi plus the page-trust and APA-section fields).
// Optionalität per the frozen witness: pointer scalars are "not
// applicable"; strings empty = absent; ParagraphPages is the raw
// per-paragraph page map, nil on pre-map generations.
type Locator struct {
	Kind               string     `json:"kind"` // "page" | "epub_cfi"
	Label              string     `json:"label"`
	Chapter            string     `json:"chapter,omitempty"`
	CFI                string     `json:"cfi,omitempty"`
	ChapterNumber      *int       `json:"chapter_number,omitempty"`
	PageSource         string     `json:"page_source,omitempty"`
	PageStart          *int       `json:"page_start,omitempty"`
	PageEnd            *int       `json:"page_end,omitempty"`
	ParagraphPages     [][]string `json:"paragraph_pages,omitempty"`
	ParagraphInChapter *int       `json:"paragraph_in_chapter,omitempty"`
	SectionTitle       string     `json:"section_title,omitempty"`
}

// SearchHit is one ranked answer with provenance (frozen public shape).
type SearchHit struct {
	ChunkID string   `json:"chunk_id"`
	Text    string   `json:"text"`
	Score   float64  `json:"score"`
	Source  Source   `json:"source"`
	Locator Locator  `json:"locator"`
	Section []string `json:"section"`
	// CaptionText: the chunk's captions as one source-labeled string;
	// omitted when the chunk has no captions.
	CaptionText string `json:"caption_text,omitempty"`
	// Images: omitted only when the chunk carries no images at all.
	Images []Image `json:"images,omitempty"`
	// CollapsedNearDuplicates: same-document near-duplicates folded into
	// this hit (0 = none).
	CollapsedNearDuplicates int `json:"collapsed_near_duplicates,omitempty"`
}

// SearchResult is the retrieval response (frozen public shape:
// query/top_n/reranked/arms/hits/took_ms).
type SearchResult struct {
	Query    string      `json:"query"`
	TopN     int         `json:"top_n"`
	Reranked bool        `json:"reranked"` // false = fusion order after rerank failure/skip
	Arms     SearchArms  `json:"arms"`
	Hits     []SearchHit `json:"hits"`
	TookMS   int64       `json:"took_ms"`
}

// PassageRef identifies one passage (the chunk id of the frozen public
// route /api/passage/{id}).
type PassageRef struct {
	ChunkID string `json:"chunk_id"`
}

// PassageNeighbor is one adjacent chunk (chunk_index ±1, same rendition,
// rendition boundary respected).
type PassageNeighbor struct {
	ChunkID    string   `json:"chunk_id"`
	ChunkIndex int      `json:"chunk_index"`
	Text       string   `json:"text"`
	Section    []string `json:"section"`
	Locator    Locator  `json:"locator"`
}

// Passage is the one-round-trip citation primitive (frozen public
// shape): the chunk, its ±1 neighbors, and the unified source block.
// DocumentID/SnapshotID/RenditionID replace the 0.1.x attachment-facing
// ids (ADR-0001: no Zotero vocabulary crosses the seam); RenditionID is
// the Library rendition the snapshot chain came from.
type Passage struct {
	ChunkID        string            `json:"chunk_id"`
	DocumentID     string            `json:"document_id"`
	SnapshotID     string            `json:"snapshot_id"`
	RenditionID    string            `json:"rendition_id"`
	ChunkIndex     int               `json:"chunk_index"`
	Text           string            `json:"text"`
	Section        []string          `json:"section"`
	Locator        Locator           `json:"locator"`
	Source         Source            `json:"source"`
	Neighbors      []PassageNeighbor `json:"neighbors"`
	ParagraphPages [][]string        `json:"paragraph_pages,omitempty"`
	CaptionText    string            `json:"caption_text,omitempty"`
	Images         []Image           `json:"images,omitempty"`
}
