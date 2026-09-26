// Package revision defines SourceRevision — the FIRST-CLASS bridge DTO of
// the Library→Store seam (F03, #297; consumers F06 #300, F09 #303).
//
// A SourceRevision is the ONLY thing Store consumes from Library: the
// normalized, versioned description of one rendition's content plus an
// opaque content ticket. Zotero/provider identities never cross this
// seam — SourceID and RevisionID are opaque Library-assigned strings to
// Store.
//
// Transport-neutral (see internal/contracts lint): stdlib plus sibling
// contract packages only.
package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
)

// ContractVersion freezes this package's DTO wire format (field set and
// names). Additive changes keep "v1" plus a golden update in the same PR;
// breaking changes bump to "v2" and carry a translation period.
const ContractVersion = "v1"

// Bibliography is the normalized bibliographic record a Library publishes
// with every revision — the contract-world shape of the proven
// /api/search "source" block (doc_id/title/authors/year/… plus the
// citation class). Store stores it verbatim at intake and hydrates search
// hits and passages from it.
//
// Optionalität: Year is *int because "no year" must stay distinguishable
// from a (meaningless) 0 — nil means unknown. Tags is nil-able; nil
// marshals as absent (omitempty). Title/Authors/Publisher/Language are
// plain strings: empty string IS the honest "unknown" (matches the
// frozen public source block, where they always appear).
type Bibliography struct {
	// RecordID is the Library record identity (the "doc_id" of the
	// 0.1.x public contract) — how clients link hits back to records.
	RecordID string   `json:"record_id"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors"`
	// Year: nil = unknown (never guessed); pointer per the NULL rule.
	Year      *int   `json:"year,omitempty"`
	Publisher string `json:"publisher"`
	Language  string `json:"language,omitempty"`
	// Tags: absent (nil) = none; never an empty non-nil slice on the wire.
	Tags []string `json:"tags,omitempty"`
	// CitationClass: one of the CitationClass* constants — whether the
	// record may be a citation target at all. ALWAYS present and
	// required: producers must materialize it explicitly (Validate
	// rejects empty); "citable" is the ordinary value for citable
	// records, not a zero-value default that omission would produce.
	CitationClass string `json:"citation_class"`
}

// Citation class vocabulary (the frozen SourceView citation_class).
const (
	CitationClassCitable    = "citable"
	CitationClassContextual = "contextual"
)

// Page-trust levels of the citation ladder (0.1.x page_source vocabulary;
// #173/#280): folio_verified is the only level a client may cite as a
// printed page without annotation; folio_interpolated is citable with its
// origin label; physical_only renders as "PDF-S."; none means no stable
// pages at all.
const (
	TrustFolioVerified     = "folio_verified"
	TrustFolioInterpolated = "folio_interpolated"
	TrustPhysicalOnly      = "physical_only"
	TrustNone              = "none"
)

// PageCapability says the rendition exposes citable print pages and at
// which trust level (Trust* constants).
type PageCapability struct {
	Trust string `json:"trust"`
}

// LocatorCapabilities declares which locator kinds citations may use on
// this rendition. Page is nil iff there are no citable pages; EPubCFI is
// a bool (present-but-false carries no extra meaning).
type LocatorCapabilities struct {
	Page    *PageCapability `json:"page,omitempty"`
	EPubCFI bool            `json:"epub_cfi"`
}

// MediaType values in circulation today (the active-snapshot format
// factor of the 0.1.x citation contract). Not a closed enum: intake
// validates non-empty, future formats extend this set additively.
const (
	MediaTypePDF  = "application/pdf"
	MediaTypeEPUB = "application/epub+zip"
)

// hashHex matches sha256 as Store requires it: 64 lowercase hex chars.
var hashHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SourceRevision is the versioned intake artifact: everything Store needs
// to (a) validate content identity, (b) hydrate bibliographic context,
// (c) fetch the bytes via the Library seam.
//
// Optionalität: all scalar fields are required non-empty, and the
// Bibliography members RecordID and CitationClass are required values
// too (Validate) — an intake artifact must be complete or it is
// invalid, never half-present. Bibliography's other members keep
// their own optionalität (see there).
type SourceRevision struct {
	// SourceID identifies the Library source (opaque to Store).
	SourceID string `json:"source_id"`
	// RevisionID identifies THIS revision of the source's content;
	// Library guarantees monotonicity per source within a contract
	// version (how — version counter, timestamp, hash — is a Library
	// implementation detail).
	RevisionID string `json:"revision_id"`
	// RenditionID identifies the rendition (the concrete file/form) this
	// revision describes — the opaque sibling of Bibliography.RecordID.
	// Additive F09 (#303): Store dedups intake per rendition+content; the
	// id is Library-assigned and opaque here (never a Zotero semantic).
	RenditionID string `json:"rendition_id"`
	// ContentHash is sha256 (lowercase hex) over the rendition bytes the
	// ContentTicket grants. Consumers MUST verify fetched content
	// against it.
	ContentHash string `json:"content_hash"`
	// MediaType is the rendition format (MediaType* constants or an
	// additive future value).
	MediaType string `json:"media_type"`
	// Bibliography is the normalized record this rendition belongs to.
	Bibliography Bibliography `json:"bibliography"`
	// LocatorCapabilities declares which locators citations may use.
	LocatorCapabilities LocatorCapabilities `json:"locator_capabilities"`
	// ContentTicket is an opaque, single-purpose grant: the exact string
	// Library.OpenRendition expects. Store treats it as a capability
	// token, never interprets it.
	ContentTicket string `json:"content_ticket"`
}

// Validate enforces the intake contract (F09: "Store accepts ingest only
// from a valid Source Revision"). Violations are InvalidArgument contract
// errors — the caller maps them across the seam unchanged.
func (r SourceRevision) Validate() error {
	switch {
	case r.SourceID == "":
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: source_id is empty")
	case r.RevisionID == "":
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: revision_id is empty")
	case !hashHex.MatchString(r.ContentHash):
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: content_hash must be lowercase sha256 hex (64 chars)")
	case r.MediaType == "":
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: media_type is empty")
	case r.ContentTicket == "":
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: content_ticket is empty")
	case r.Bibliography.RecordID == "":
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "source revision: bibliography.record_id is empty")
	case r.Bibliography.CitationClass != CitationClassCitable && r.Bibliography.CitationClass != CitationClassContextual:
		return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, fmt.Sprintf("source revision: bibliography.citation_class must be %q or %q, got %q", CitationClassCitable, CitationClassContextual, r.Bibliography.CitationClass))
	}
	return nil
}

// HashContent is the canonical content digest helper: sha256 over bytes,
// lowercase hex — the exact form ContentHash requires. Shared so Library
// (producer) and any verifying consumer cannot drift apart.
func HashContent(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
