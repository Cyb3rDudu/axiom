// dedup.go — duplicate detection over the FULLY PAGINATED provider
// catalog (F06, #300). Runs before every write. The catalog is the truth:
// first-page or first-match shortcuts are exactly the error the ladder
// forbids for metadata — the scan follows every page.
//
// Binding scenarios:
//   - gleiches Record + gleiches PDF     → membership only
//   - gleiches Record ohne diese Rendition → rendition am Parent ergänzen
//   - mehrdeutige Übereinstimmung        → NIE automatisch zusammenführen
//     (decision with the candidate records)
package library

import (
	"context"
	"fmt"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
)

// PlacementPlan is the dedup outcome the saga executes.
type PlacementPlan struct {
	// LinkProviderRecordID: non-empty → reuse this existing record
	// (dedup hit or confirmed choice); empty → create a new one.
	LinkProviderRecordID string
	// AddRendition: the content hash is not yet a rendition of the target
	// record → the saga must upload it.
	AddRendition bool
	// ExistingAttachmentID: the identical rendition already exists under
	// the target record (membership-only scenario).
	ExistingAttachmentID string
}

// DedupScan walks the ENTIRE catalog (every page — pagination is not
// optional) and matches the incoming record + rendition content hash
// against the comparison matrix: DOI, ISBN, normalized
// title+author+year+type, rendition content hash.
func DedupScan(ctx context.Context, cat CatalogReader, incoming CatalogRecord, contentHash string) (PlacementPlan, *Decision, error) {
	plan := PlacementPlan{AddRendition: true}
	var (
		recordHits []CatalogRecord // distinct records matching the matrix
		rendHit    *CatalogRecord  // record already carrying this content hash
	)
	token := ""
	for {
		page, err := cat.ListRecords(ctx, token)
		if err != nil {
			return plan, nil, err
		}
		for i := range page.Records {
			r := page.Records[i]
			if r.ProviderRecordID == incoming.ProviderRecordID && incoming.ProviderRecordID != "" {
				// The catalog row IS the incoming record (re-entry after
				// our own partial write): counts as the link target.
				recordHits = appendUniqueRecord(recordHits, r)
			}
			if contentHash != "" && hasRendition(r, contentHash) {
				cp := r
				rendHit = &cp
				recordHits = appendUniqueRecord(recordHits, r)
			} else if recordMatches(incoming, r) {
				recordHits = appendUniqueRecord(recordHits, r)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}

	distinct := distinctRecords(recordHits)
	switch {
	case len(distinct) == 0:
		return plan, nil, nil // fresh record + rendition
	case rendHit != nil && len(distinct) == 1:
		// Same record + same PDF → membership only.
		plan.LinkProviderRecordID = rendHit.ProviderRecordID
		plan.AddRendition = false
		plan.ExistingAttachmentID = findRendition(*rendHit, contentHash)
		return plan, nil, nil
	case len(distinct) == 1:
		// Same record, no such rendition → rendition am Parent ergänzen.
		plan.LinkProviderRecordID = distinct[0].ProviderRecordID
		return plan, nil, nil
	}

	// Mehrdeutig: never merge automatically — offer the records.
	d := &Decision{DecisionID: "dec-duplicate", Subject: "duplicate"}
	for _, r := range distinct {
		d.Candidates = append(d.Candidates, DecisionCandidate{
			CandidateID: r.ProviderRecordID,
			Origin:      "provider_existing",
			Summary: fmt.Sprintf("%s (%s%s)", firstNonEmpty(r.Title, "untitled"),
				authorYearSummary(r), suffixNonEmpty(r.ProviderRecordID, " · ")),
		})
	}
	return plan, d, nil
}

// recordMatches is the record-level comparison matrix (identifier exact,
// or normalized title+author+year+type).
func recordMatches(incoming, existing CatalogRecord) bool {
	if incoming.DOI != "" && existing.DOI != "" && NormalizeDOI(incoming.DOI) == NormalizeDOI(existing.DOI) {
		return true
	}
	if incoming.ISBN != "" && existing.ISBN != "" && NormalizeISBN(incoming.ISBN) == NormalizeISBN(existing.ISBN) {
		return true
	}
	if normalizeTitle(incoming.Title) != "" &&
		normalizeTitle(incoming.Title) == normalizeTitle(existing.Title) &&
		authorSignature(incoming) == authorSignature(existing) &&
		yearOf(incoming) == yearOf(existing) &&
		incoming.RecordType == existing.RecordType {
		return true
	}
	return false
}

func hasRendition(r CatalogRecord, contentHash string) bool {
	return findRendition(r, contentHash) != ""
}

func findRendition(r CatalogRecord, contentHash string) string {
	for _, a := range r.Renditions {
		if a.ContentHash == contentHash {
			return a.ProviderAttachmentID
		}
	}
	return ""
}

func normalizeTitle(t string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(t) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		// non-ASCII letters and punctuation collapse into nothing — the
		// normalized form only has to be STABLE, not linguistically pure.
	}
	return b.String()
}

func authorSignature(r CatalogRecord) string {
	var names []string
	for _, c := range r.Authors {
		names = append(names, strings.ToLower(c.LastName+c.Name))
	}
	return strings.Join(names, "|")
}

func yearOf(r CatalogRecord) int {
	if r.Year == nil {
		return 0
	}
	return *r.Year
}

func authorYearSummary(r CatalogRecord) string {
	who := "unknown"
	if len(r.Authors) > 0 {
		who = firstNonEmpty(r.Authors[0].LastName, r.Authors[0].Name, "unknown")
	}
	if y := yearOf(r); y > 0 {
		return fmt.Sprintf("%s %d", who, y)
	}
	return who
}

func appendUniqueRecord(xs []CatalogRecord, r CatalogRecord) []CatalogRecord {
	for _, x := range xs {
		if x.ProviderRecordID == r.ProviderRecordID {
			return xs
		}
	}
	return append(xs, r)
}

func distinctRecords(xs []CatalogRecord) []CatalogRecord {
	seen := map[string]bool{}
	var out []CatalogRecord
	for _, x := range xs {
		if seen[x.ProviderRecordID] {
			continue
		}
		seen[x.ProviderRecordID] = true
		out = append(out, x)
	}
	return out
}

// resolveAmbiguityConflict maps a provider collection conflict (same-named
// siblings) into a contract error — never a guess.
func siblingConflictError(name string) error {
	return contracterr.New(contracterr.ComponentLibrary, contracterr.ClassConflict,
		"collection path ambiguous: same-named siblings under one parent are a conflict, not a pick ("+name+")")
}
