// Package library is the Library component (F06, #300): the bibliographic
// domain behind the F03 contracts. It owns the durable import state
// machine, duplicate detection over the provider catalog, the metadata
// verify ladder with field provenance, hashed staging, and source
// revision publication.
//
// Provider access is PORT-ONLY (ports.go). F06 proves everything against
// deterministic fakes (fakes.go); F07 ports Zotero behind these ports and
// F11 binds them over HTTP. Read paths delegate thinly onto the existing
// Zotero mirror tables (same physical DB) — strangler, no rewrite.
//
// The package is env-free by design (usage lint, #298): configuration
// arrives constructed (internal/config + composition root).
package library

import (
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
)

// Domain model — the Library's own vocabulary. F03 DTOs are the transport
// form of these; provider shapes never leak in and DB rows never leak out.

// Creator is one person or institution responsible for a record. The
// single-field Name (fieldMode 1) marks institutional creators.
type Creator struct {
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	Name        string `json:"name,omitempty"` // institutional creator
	CreatorType string `json:"creator_type,omitempty"`
}

// Record is one bibliographic record (the domain twin of a Zotero parent
// item / the F03 revision.Bibliography carrier).
type Record struct {
	RecordID      string // Library-assigned identity (provider-opaque)
	RecordType    string // book | journalArticle | conferencePaper | report | webpage | …
	Title         string
	Authors       []Creator
	Year          *int
	Publisher     string
	Language      string
	DOI           string
	ISBN          string
	Institution   string // report-type origin
	URL           string // webpage origin
	CitationClass string // revision.CitationClass* vocabulary
	Tags          []string
}

// Rendition is one concrete file of a record.
type Rendition struct {
	RenditionID string // Library-assigned identity
	RecordID    string
	ContentHash string // sha256 lowercase hex
	MediaType   string // revision.MediaType* vocabulary
	Filename    string // schema filename (naming.go)
}

// Collection is one node of the provider's collection tree.
type Collection struct {
	CollectionID string // provider-assigned id
	Name         string
	ParentID     string // "" = root
}

// Membership places a record in a collection.
type Membership struct {
	CollectionID string
	RecordID     string
}

// SourceRevisionDomain is the persisted, versioned intake artifact — the
// domain twin of revision.SourceRevision plus publication bookkeeping.
// Store consumes these from F09 on; the row IS the publication.
type SourceRevisionDomain struct {
	SourceID            string
	RecordID            string
	RenditionID         string
	RevisionID          int64 // monotonic per (SourceID, RecordID)
	ContentHash         string
	MediaType           string
	Bibliography        revision.Bibliography
	LocatorCapabilities revision.LocatorCapabilities
	ContentTicket       string
	Origin              string // import | sync | heal
	CreatedAt           time.Time
}

// now returns the DM03-compatible time form (UTC, microsecond-aligned) —
// every produced timestamp goes through here.
func now(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}
