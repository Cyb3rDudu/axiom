// Post-rerank ranking hygiene (#160): near-duplicate collapse within one
// document (the TOC line vs. chapter-start twin) and a per-book diversity cap
// so a Top-N shows breadth instead of five copies of one chapter.
package search

import (
	"strings"
)

// nearDuplicateThreshold is the Jaccard cutoff above which two same-document
// chunks count as near-duplicates.
const nearDuplicateThreshold = 0.8

// ponytail: O(n²) pairwise jaccard, tokenSet rebuilt per pair — memoize per
// chunk if fetchN ever grows past 64.
//
// collapseNearDuplicates folds near-identical chunks of the SAME document into
// their higher-ranked twin (ranked list order is authoritative — rerank score
// descending, RRF order as tiebreak). Returns the pruned list and, per kept
// chunk id, how many duplicates were folded in (surfaced on the hit as the
// collapse hint).
func collapseNearDuplicates(cands []osCandidate) ([]osCandidate, map[string]int) {
	kept := make([]osCandidate, 0, len(cands))
	folded := map[string]int{}
	for _, c := range cands {
		dup := false
		for i, k := range kept {
			if k.DocumentID == c.DocumentID && jaccard(k.Text, c.Text) > nearDuplicateThreshold {
				keptID := kept[i].ID
				folded[keptID]++
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, c)
		}
	}
	return kept, folded
}

// diversify enforces at most maxPerBook chunks per document in rank order,
// refilling from the remainder so Top-N stays full (a book-heavy ranking
// demotes its own excess hits past other books' hits instead of shrinking the
// result). maxPerBook <= 0 disables the rule entirely (matrix lever).
func diversify(cands []osCandidate, maxPerBook int) []osCandidate {
	if maxPerBook <= 0 {
		return cands
	}
	out := make([]osCandidate, 0, len(cands))
	rest := make([]osCandidate, 0)
	counts := map[string]int{}
	for _, c := range cands {
		if counts[c.DocumentID] < maxPerBook {
			counts[c.DocumentID]++
			out = append(out, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(out, rest...)
}

// jaccard over lowercase word tokens; 1.0 for identical texts.
func jaccard(a, b string) float64 {
	if a == b {
		return 1
	}
	as, bs := tokenSet(a), tokenSet(b)
	if len(as) == 0 || len(bs) == 0 {
		return 0
	}
	inter := 0
	for t := range as {
		if _, ok := bs[t]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(as)+len(bs)-inter)
}

func tokenSet(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum || r >= 0x80 {
			return false
		}
		return true
	}) {
		set[t] = struct{}{}
	}
	return set
}
