// Package frontmatter classifies frontmatter/back-matter section classes
// (#160 retrieval semantics + the #198 KG gate classes). One classifier,
// two consumers:
//
//   - search.IsFrontmatter (the #160 retrieval filter): TOC, preface and
//     bibliography only — byte-identical to the original detector, whose
//     corpus calibration (2026-08-17, axiom-ng-chunks-v1) lives on here;
//   - the KG paths (#198 item 1): additionally author lists, index/register
//     back-matter and chapter byline/title lines — the evidence classes
//     behind frontmatter-sourced KG junk edges (the "named_after Fifka /
//     discoverer Weber" class).
//
// Corpus reality (validated against the live KG evidence set, 2026-08-20,
// 76,301 evidence chunks): Marker output is markdown; TOC pages render as
// tables or dot-leader lists, references as dash-lists with dense years,
// indexes as term+page lists, bylines as `### heading` + `**24**` + author
// name. section_titles trails are UNRELIABLE in both directions — the
// detector stays TEXT-ONLY.
//
// PRECISION FIRST: a false positive deletes real KG evidence (or, via the
// retrieval filter, real candidates). Every pattern must be something body
// text does not do.
package frontmatter

import (
	"regexp"
	"strconv"
	"strings"
)

// Class is a frontmatter/back-matter section class. ClassNone means body
// content (ungated everywhere).
type Class string

const (
	ClassNone         Class = ""
	ClassTOC          Class = "toc"
	ClassAuthors      Class = "authors"
	ClassPreface      Class = "preface"
	ClassBibliography Class = "bibliography"
	ClassIndex        Class = "index"
	ClassTitleLines   Class = "title_lines"
)

var (
	// Standalone frontmatter/back-matter heading, first line. Markdown
	// decoration (####, **, <span>-anchors) and OCR trailing dashes
	// tolerated (normalizeHeading strips them).
	headingRe    = regexp.MustCompile(`(?i)^(inhaltsverzeichnis|table of contents|contents|vorwort|preface|geleitwort|foreword|literaturverzeichnis|bibliografie|bibliographie|bibliography|references|quellenverzeichnis|literatur|autorenverzeichnis|autoren|autorinnen und autoren|über die autoren|über die autorinnen|die autorinnen und autoren|über die herausgeber|mitarbeiterverzeichnis|beitragsverzeichnis|herausgeber und autoren|authors|about the authors|the authors|register|sachregister|stichwortverzeichnis|namensverzeichnis|personenregister|ortsregister|ortregister|index)-*\s*:?\s*$`)
	headingClass = map[string]Class{
		"inhaltsverzeichnis": ClassTOC, "table of contents": ClassTOC, "contents": ClassTOC,
		"vorwort": ClassPreface, "preface": ClassPreface, "geleitwort": ClassPreface, "foreword": ClassPreface,
		"literaturverzeichnis": ClassBibliography, "bibliografie": ClassBibliography, "bibliographie": ClassBibliography,
		"bibliography": ClassBibliography, "references": ClassBibliography, "quellenverzeichnis": ClassBibliography, "literatur": ClassBibliography,
		"autorenverzeichnis": ClassAuthors, "autoren": ClassAuthors, "autorinnen und autoren": ClassAuthors,
		"über die autoren": ClassAuthors, "über die autorinnen": ClassAuthors, "die autorinnen und autoren": ClassAuthors,
		"über die herausgeber":   ClassAuthors,
		"mitarbeiterverzeichnis": ClassAuthors, "beitragsverzeichnis": ClassAuthors, "herausgeber und autoren": ClassAuthors,
		"authors": ClassAuthors, "about the authors": ClassAuthors, "the authors": ClassAuthors,
		"register": ClassIndex, "sachregister": ClassIndex, "stichwortverzeichnis": ClassIndex,
		"namensverzeichnis": ClassIndex, "personenregister": ClassIndex, "ortsregister": ClassIndex, "ortregister": ClassIndex, "index": ClassIndex,
	}
	// Any markdown table row; TOC detection then requires the last cell to
	// be a page number (lastCellIsPage).
	tableRowRe = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	// Classic dot-leader TOC line ("5.1.2 Zieldefinition …… 148").
	dotLeaderRe = regexp.MustCompile(`\.{4,}(…|\s)*\d{1,4}\s*$`)
	// Numbered heading → page number line.
	tocLineRe = regexp.MustCompile(`^\s*(\d+(\.\d+)*|[A-Z]\.(?:\d+\.?)+)[.)]?\s+\S.*?[.…]?\s*\.{0,3}\s*\d{1,4}\s*$`)
	// Index entry line: term→page pairs. Two corpus shapes: one entry per
	// line ("Kapitalmarkt 41", "Advantage of efficiency, 11, 36") and
	// soft-wrapped runs ("... Just-in-time-Produktion 156 **K** Kalkulation
	// 158 Kapazitätsbeanspruchung 169 ..."). A line counts as an index line
	// with ≥3 word→small-number pairs and NO sentence punctuation (index
	// lines are not sentences; "f./ff." page ranges are stripped first).
	indexPairRe = regexp.MustCompile(`[a-zA-ZäöüÄÖÜß][a-zA-ZäöüÄÖÜß\-'’]{2,}[,;]? ?\d{1,4}\b`)
	// Chapter byline: a BOLD bare number line ("**24**") — the contributed-
	// chapter card number. Prose never bolds a standalone number.
	boldBareNumRe = regexp.MustCompile(`^\*\*\d{1,3}\.?\*\*$`)
	// Title-page editor line: ONE short line of names ending in the italic
	// *Hrsg.* role marker — title-page typography; prose citations use a
	// plain "(Hrsg.)" and full sentences (#198 rider, observed shape
	// "Marco Englert Anabel Ternès *Hrsg.*").
	editorLineRe = regexp.MustCompile(`^.{0,90}\*Hrsg\.\*$`)
	yearRe       = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	citeMarkerRe = regexp.MustCompile(`(?i)(\bin:|, vol\.\s|, no\.\s|\bpp?\.\s*\d|\bS\.\s?\d|\bNr\.\s?\d|\bVerlag\b|\bAufl\.|\bISBN|https?://|\[\d+\]|©)`)
)

// normalizeHeading strips markdown decoration from a heading line.
func normalizeHeading(l string) string {
	l = strings.TrimSpace(l)
	l = strings.TrimLeft(l, "#> ")
	// <span id="page-495-0"></span> marker anchors (Marker paginate output).
	if i := strings.Index(l, "</span>"); i >= 0 {
		l = l[i+len("</span>"):]
	}
	l = strings.ReplaceAll(l, "*", "")
	l = strings.ReplaceAll(l, "_", "")
	return strings.TrimSpace(l)
}

// looksLikePageNumber reports whether the last |-separated cell of a table row
// is a bare small integer (a page number cell). Years are excluded on
// purpose: a data table with a year column must not look like a TOC, and TOC
// pages above 1900 are vanishingly rare.
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
	return err == nil && n >= 0 && n <= 3000 && !(n >= 1900 && n <= 2099)
}

func nonEmptyLines(text string) []string {
	raw := strings.Split(text, "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Classify returns the frontmatter class of a chunk text (ClassNone for
// body content). Heading shapes win; shape detectors require line
// majorities so a stray line inside prose never gates a chunk.
func Classify(text string) Class {
	lines := nonEmptyLines(text)
	if len(lines) == 0 {
		return ClassNone
	}

	// 1) Heading chunk: standalone frontmatter/back-matter heading.
	if class, ok := headingClass[strings.ToLower(strings.TrimRight(normalizeHeading(lines[0]), ": -"))]; ok {
		return class
	}

	// 1b) Title-page editor line (#198 rider): a single short line of
	// names ending in the italic *Hrsg.* marker — heading-less frontmatter.
	if len(lines) == 1 && len(lines[0]) <= 100 && editorLineRe.MatchString(strings.TrimSpace(lines[0])) {
		return ClassAuthors
	}

	// 2) TOC shapes.
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
		return ClassTOC
	}
	tocLines := 0
	for _, l := range lines {
		if tocLineRe.MatchString(l) || dotLeaderRe.MatchString(l) {
			tocLines++
		}
	}
	if tocLines >= 4 && tocLines*2 >= len(lines) {
		return ClassTOC
	}

	// 3) Chapter byline / title lines: heading first line, exactly one
	// BOLD bare number (card number), few lines, and everything after the
	// number is short (author names / subtitles / italic epigraphs) — no
	// prose. Body chunks with numbered subsections use PLAIN numbers and
	// carry real sentences (measured false-positive class, stays ungated).
	if strings.HasPrefix(strings.TrimSpace(text), "#") {
		boldNums := 0
		for _, l := range lines {
			if boldBareNumRe.MatchString(strings.TrimSpace(l)) {
				boldNums++
			}
		}
		if boldNums == 1 && len(lines) <= 6 {
			prose := false
			for _, l := range lines[1:] {
				s := strings.TrimSpace(l)
				if boldBareNumRe.MatchString(s) {
					continue // the number line itself
				}
				if len(s) > 80 && !isItalicOrQuote(s) {
					prose = true
					break
				}
			}
			if !prose {
				return ClassTitleLines
			}
		}
	}

	// 4) References shape: dash-list entries with dense years AND citation
	// markers (venue signals separate a bibliography from a prose
	// chronology). Body text never stacks ≥5 of them.
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
		return ClassBibliography
	}

	// 5) Index shape: majority index lines AND enough term→page pairs in
	// total (≥6). Continuation chunks are ONE long soft-wrapped run — the
	// pair sum, not the line count, carries the signal.
	idxLines, idxPairs := 0, 0
	for _, l := range lines {
		if isIndexLine(l) {
			idxLines++
			idxPairs += len(indexPairRe.FindAllString(stripPageRanges(l), -1))
		}
	}
	if idxLines*2 >= len(lines) && idxPairs >= 6 {
		return ClassIndex
	}
	return ClassNone
}

// stripPageRanges removes the "f."/"ff." page-range markers so the
// sentence-punctuation check does not trip on them.
func stripPageRanges(l string) string {
	return strings.ReplaceAll(strings.ReplaceAll(l, " f.", " "), " ff.", " ")
}

// isIndexLine: ≥3 term→page pairs, no sentence punctuation (the "f."/"ff."
// page-range markers are removed before the check).
func isIndexLine(l string) bool {
	s := stripPageRanges(l)
	if strings.ContainsAny(s, ".!?") {
		return false
	}
	return len(indexPairRe.FindAllString(s, -1)) >= 3
}

// isItalicOrQuote: fully italic-wrapped (*...*) or blockquote-prefixed
// lines are epigraph decoration, not prose.
func isItalicOrQuote(s string) bool {
	if strings.HasPrefix(s, ">") {
		return true
	}
	return strings.HasPrefix(s, "*") && strings.HasSuffix(s, "*") && len(s) > 2
}

// IsFrontmatter is the #160 retrieval semantics: TOC / preface /
// bibliography only. The KG-only classes (authors, index, title lines)
// stay retrievable — a query about "the authors" must still find the
// author-list page.
func IsFrontmatter(text string) bool {
	switch Classify(text) {
	case ClassTOC, ClassPreface, ClassBibliography:
		return true
	}
	return false
}
