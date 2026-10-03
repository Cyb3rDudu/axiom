// identifiers.go — the neutral identifier normalization and revision
// transport mapping (engine-neutral: shared by every repository
// implementation and the application).
package library

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/revision"
)

var doiPrefixRe = regexp.MustCompile(`^(https?://(dx\.)?doi\.org/|doi:)\s*`)

// NormalizeDOI canonicalizes a DOI: strip resolver prefixes, lowercase.
func NormalizeDOI(doi string) string {
	d := strings.TrimSpace(strings.ToLower(doi))
	return doiPrefixRe.ReplaceAllString(d, "")
}

// NormalizeISBN canonicalizes an ISBN: strip separators, uppercase (X
// check digit). Separators include the Unicode hyphens printers use
// (U+2010 HYPHEN, U+2011 NON-BREAKING HYPHEN — the inspector matches
// them, so the normalizer must strip them or the identifier carries the
// typo into the ledger). No checksum invention — malformed input stays
// as-is and simply never matches.
func NormalizeISBN(isbn string) string {
	s := strings.ToUpper(strings.TrimSpace(isbn))
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == ' ' || r == '\u2010' || r == '\u2011' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// RevisionFromDomain converts a domain revision into its F03 transport
// form (RevisionID rendered as decimal string — the frozen wire shape).
func RevisionFromDomain(r SourceRevisionDomain) revision.SourceRevision {
	return revision.SourceRevision{
		SourceID:            r.SourceID,
		RevisionID:          fmt.Sprintf("%d", r.RevisionID),
		RenditionID:         r.RenditionID,
		ContentHash:         r.ContentHash,
		MediaType:           r.MediaType,
		Bibliography:        r.Bibliography,
		LocatorCapabilities: r.LocatorCapabilities,
		ContentTicket:       r.ContentTicket,
	}
}
