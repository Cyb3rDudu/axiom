// Frontmatter/TOC/references detection (#160). dudu's old-system pain: agents
// cited table-of-contents / title-page / preface / bibliography lines instead
// of body text. Detection runs at retrieval time over the candidate texts —
// retroactive over the whole existing index by construction, no backfill, no
// reingest.
//
// Since #198 item 1 the classifier lives in internal/frontmatter (one
// calibrated implementation shared with the KG gate); this file keeps the
// retrieval surface. The retrieval semantics are UNCHANGED: TOC / preface /
// bibliography only — the KG-only classes (authors, index, title lines) stay
// retrievable.
package search

import (
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/frontmatter"
)

// IsFrontmatter reports whether a chunk is TOC / references / titled-preface
// material that should not compete for citation slots (#160 semantics).
func IsFrontmatter(text string) bool {
	return frontmatter.IsFrontmatter(text)
}

// filterFrontmatter drops detected frontmatter chunks from the candidate pool
// (#160). Degradation guard: if EVERYTHING looks like frontmatter (a query
// literally about a table of contents), the filter yields rather than
// returning an empty result set — hygiene must never zero out retrieval.
func filterFrontmatter(cands []osCandidate) []osCandidate {
	kept := make([]osCandidate, 0, len(cands))
	for _, c := range cands {
		if !IsFrontmatter(c.Text) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return cands
	}
	return kept
}
