// Frontmatter/TOC/references detection (#160). dudu's old-system pain: agents
// cited table-of-contents / title-page / preface / bibliography lines instead
// of body text. Detection runs at retrieval time over the candidate texts —
// retroactive over the whole existing index by construction, no backfill, no
// reingest.
//
// Corpus reality (validated against axiom-ng-chunks-v1, 2026-08-17): Marker
// output is markdown; heading lines become their own mini-chunks
// ("### **Inhaltsverzeichnis**"), TOC pages render as markdown TABLES
// ("| 3 | Title<br>Author | 23  |"), references as dash-lists with dense
// 4-digit years. section_titles trails are UNRELIABLE in both directions —
// lagging on real TOC pages and sticking over whole books (whole-corpus
// scan: 654 prose false-positives via trail) — so the detector is TEXT-ONLY.
//
// PRECISION FIRST: a false positive deletes real content from the candidate
// pool. Every pattern below must be something body text does not do; the
// flip-list probe asserts the aggressive side stays a manual lever, not a
// heuristic accident.
package search

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// Frontmatter heading, standalone first line. Markdown decoration
	// (####, **) and OCR trailing dashes tolerated.
	fmHeadingRe = regexp.MustCompile(`(?i)^(inhaltsverzeichnis|table of contents|contents|vorwort|preface|geleitwort|foreword|literaturverzeichnis|bibliografie|bibliographie|bibliography|references|quellenverzeichnis|literatur)-*\s*:?\s*$`)
	// Markdown table row whose LAST cell is a bare 1-4 digit page number
	// ("| 1 | Road to Excellence … | 3   |").
	tocTableRowRe = regexp.MustCompile(`^\s*\|.*\|\s*\d{1,4}\s*\|?\s*$`)
	tableRowRe    = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	// Classic dot-leader TOC line ("5.1.2 Zieldefinition …… 148").
	dotLeaderRe = regexp.MustCompile(`\.{4,}(…|\s)*\d{1,4}\s*$`)
	// Numbered heading → page number line.
	tocLineRe    = regexp.MustCompile(`^\s*(\d+(\.\d+)*|[A-Z]\.(?:\d+\.?)+)[.)]?\s+\S.*?[.…]?\s*\.{0,3}\s*\d{1,4}\s*$`)
	yearRe       = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	citeMarkerRe = regexp.MustCompile(`(?i)(\bin:|, vol\.\s|, no\.\s|\bpp?\.\s*\d|\bS\.\s?\d|\bNr\.\s?\d|\bVerlag\b|\bAufl\.|\bISBN|https?://|\[\d+\]|©)`)
)

// normalizeHeading strips markdown decoration from a heading line.
func normalizeHeading(l string) string {
	l = strings.TrimSpace(l)
	l = strings.TrimLeft(l, "#> ")
	l = strings.ReplaceAll(l, "*", "")
	l = strings.ReplaceAll(l, "_", "")
	return strings.TrimSpace(l)
}

// looksLikePageNumber reports whether the last |-separated cell of a table row
// is a bare small integer (a page number cell).
func lastCellIsPage(row string) bool {
	cells := strings.Split(row, "|")
	if len(cells) < 2 {
		return false
	}
	last := strings.TrimSpace(cells[len(cells)-2]) // trailing "" after final |
	if last == "" && len(cells) >= 3 {
		last = strings.TrimSpace(cells[len(cells)-3])
	}
	n, err := strconv.Atoi(strings.TrimSuffix(last, "."))
	return err == nil && n >= 0 && n <= 3000
}

// IsFrontmatter reports whether a chunk is TOC / references / titled-preface
// material that should not compete for citation slots.
func IsFrontmatter(text string) bool {
	raw := strings.Split(text, "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return false
	}

	// 1) Heading chunk: standalone frontmatter heading as first line.
	if fmHeadingRe.MatchString(normalizeHeading(lines[0])) {
		return true
	}

	// 2) TOC page: markdown table rows ending in page-number cells (the
	// dominant corpus shape) or classic dot-leader/numbered lines.
	tableRows, pageRows := 0, 0
	for _, l := range lines {
		if tableRowRe.MatchString(l) {
			tableRows++
			if lastCellIsPage(l) {
				pageRows++
			}
		}
	}
	if pageRows >= 3 && pageRows*2 >= len(lines) {
		return true
	}
	tocLines := 0
	for _, l := range lines {
		if tocLineRe.MatchString(l) || dotLeaderRe.MatchString(l) {
			tocLines++
		}
	}
	if tocLines >= 4 && tocLines*2 >= len(lines) {
		return true
	}

	// 3) References chunk: dash-list entries with a dense 4-digit-year AND
	// citation-marker majority (venue signals separate bibliography from a
	// prose chronology of dated events). Body text never stacks ≥5 of them.
	dashEntries, withYear, withCite := 0, 0, 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "– ") || strings.HasPrefix(t, "— ") {
			dashEntries++
			if yearRe.MatchString(l) {
				withYear++
			}
			if citeMarkerRe.MatchString(l) {
				withCite++
			}
		}
	}
	if dashEntries >= 5 && withYear*10 >= dashEntries*7 && withCite*2 >= dashEntries {
		return true
	}
	return false
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
