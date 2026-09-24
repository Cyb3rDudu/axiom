// ports.go — the provider ports of the Library component (F06, #300).
// F07 implements these against Zotero; F06 proves the component against
// deterministic fakes (fakes.go). Capability-honest by design: a port
// that is not wired is reported as explicitly unsupported (Unavailable),
// never emulated.
package library

import (
	"context"
	"errors"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
)

// ErrProviderConflict marks provider-side refusals that are conflicts in
// contract terms (e.g. same-named sibling collections — never guessed).
var ErrProviderConflict = errors.New("provider conflict")

// ---------------------------------------------------------------------------
// Catalog (read side)

// CatalogRecord is one catalog entry: the record plus its renditions, as
// the duplicate-detection matrix consumes it.
type CatalogRecord struct {
	ProviderRecordID string
	RecordID         string // Library identity when known ("" = not yet linked)
	RecordType       string
	Title            string
	Authors          []Creator
	Year             *int
	DOI              string // normalized
	ISBN             string // normalized
	Renditions       []CatalogRendition
	// Collections: provider collection ids the record is filed under
	// (the verify step observes memberships through these).
	Collections []string
}

// CatalogRendition is one rendition known to the catalog.
type CatalogRendition struct {
	ProviderAttachmentID string
	RenditionID          string
	ContentHash          string
	MediaType            string
	Filename             string
}

// CatalogPage is one page of the catalog listing.
type CatalogPage struct {
	Records []CatalogRecord
	// NextPageToken: "" = end of catalog. Readers MUST follow every page —
	// the catalog is the truth for duplicate detection; first-page-only
	// matching is exactly the error the verify ladder forbids for
	// metadata.
	NextPageToken string
}

// CatalogReader lists the FULL provider catalog, paginated. The loop
// lives in dedup.go so every consumer inherits full pagination.
type CatalogReader interface {
	ListRecords(ctx context.Context, pageToken string) (CatalogPage, error)
}

// ---------------------------------------------------------------------------
// Write side (all idempotent — provider ids are the dedup anchors)

// RecordDraft is the record to ensure at the provider. ExternalKey is the
// dedup anchor: the provider must return the SAME provider id for the
// same external key (crash between provider write and step bookkeeping
// must not double-create — the #293 lesson).
type RecordDraft struct {
	ExternalKey string // import-stable (derived from idempotency key + payload hash)
	RecordType  string
	Title       string
	Authors     []Creator
	Year        *int
	Publisher   string
	Language    string
	DOI         string
	ISBN        string
}

// RenditionDraft is the rendition file to ensure under a parent record.
// Idempotent by (parent, ContentHash): re-uploading identical bytes under
// the same parent yields the same provider attachment id.
type RenditionDraft struct {
	ParentProviderID string
	ContentHash      string
	MediaType        string
	Filename         string
	// StagingPath hands the bytes to the provider (F07: file upload; the
	// provider never gets a DB BLOB).
	StagingPath string
}

// RecordWriter ensures parent records.
type RecordWriter interface {
	EnsureRecord(ctx context.Context, d RecordDraft) (providerID string, err error)
}

// RenditionWriter ensures rendition files and collection memberships.
type RenditionWriter interface {
	EnsureRendition(ctx context.Context, d RenditionDraft) (providerID string, err error)
	// EnsureMembership idempotently files a record under a collection.
	EnsureMembership(ctx context.Context, providerRecordID, providerCollectionID string) error
}

// CollectionWriter resolves/creates collection paths parent-first.
type CollectionWriter interface {
	// ResolvePath resolves segments under the root, parent-first.
	// createMissing=false with an absent segment → NotFound contract
	// error (a typo must never land the file unfiled or guess a sibling).
	// Same-named siblings under one parent → conflict error, never a pick.
	ResolvePath(ctx context.Context, segments []string, createMissing bool) (providerCollectionID string, err error)
}

// ---------------------------------------------------------------------------
// Metadata verify ladder (resolvers)

// ResolveQuery is one resolver lookup. DOI/ISBN are normalized identifiers
// (exact lookup first); Title/Authors/Year/RecordType feed fuzzy search.
type ResolveQuery struct {
	DOI        string
	ISBN       string
	Title      string
	Authors    []string
	Year       *int
	RecordType string
}

// Candidate is one resolver hit. Fields carry ONLY what the resolver can
// actually vouch for — empty string = the resolver does not know (never
// invented). Confidence ranks candidates within one resolver.
type Candidate struct {
	CandidateID string
	Fields      ResolvedFields
	Confidence  float64
}

// ResolvedFields is the resolver's field proposal (title/authors/year/
// publisher/language/type — identifiers echo when confirmed).
type ResolvedFields struct {
	Title      string
	Authors    []string
	Year       *int
	Publisher  string
	Language   string
	RecordType string
	DOI        string
	ISBN       string
}

// BibliographicResolver is one ladder rung's source. Name/Version feed the
// provenance rows ("crossref v1.2", "open_library v1").
type BibliographicResolver interface {
	Name() string
	Version() string
	// Resolve performs the resolver's best lookup. Implementations put
	// exact-identifier matching before fuzzy search themselves (DOI vor
	// unscharfer Suche).
	Resolve(ctx context.Context, q ResolveQuery) ([]Candidate, error)
}

// DocumentInspector extracts metadata candidates from the document
// content itself — ladder rung 1 (title page / imprint). F06: a
// deterministic fake; real PDF/EPUB extraction is F07's concern behind
// the same port.
type DocumentInspector interface {
	// Inspect returns document-verified fields. Fields it cannot read
	// stay empty — never guessed.
	Inspect(ctx context.Context, mediaType string, stagingPath string) (ResolvedFields, error)
}

// ---------------------------------------------------------------------------
// Ports bundle — what the service is constructed with. Every port may be
// nil; the corresponding operations then report Unavailable ("port not
// wired"), never emulate.

type Ports struct {
	Catalog     CatalogReader
	Records     RecordWriter
	Renditions  RenditionWriter
	Collections CollectionWriter
	// Resolvers in LADDER ORDER after the document rung: identifier/Crossref
	// before Open Library. The web-research fallback rung has no F06
	// implementation — its absence is the honest state (documented in the
	// #300 design comment; R21 brings the tooling).
	Resolvers []BibliographicResolver
	Documents DocumentInspector
}

func (p Ports) unavailable(what string) error {
	return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassUnavailable,
		"library port not wired: "+what+" (F07 wires Zotero behind the ports; fake wiring arrives with the dev-env flag)")
}
