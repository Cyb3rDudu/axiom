// naming.go — the dudu schema filename convention (#287/#291), owned by
// the Library component since F06 (#300): attachment filenames are part
// of the import contract, not a repair-side convention.
//
//	{Autor|Herausgeber|Institution} - {Jahr} - {Titel}.pdf
//
// First author's lastName, else the first editor's (editors-only
// Sammelbände, #287), else the institutional single-field name. The
// publisher is NEVER a name component (#287: the custody upload of an
// editors-only volume produced "transcript - 2025 - …"). #291: the title
// follows the cleanup rules (cleanTitle), a document with existing
// attachments keeps its grown naming pattern (adoptGrownPattern), and the
// result is NFC-normalized (byte-identical API filename and on-disk name
// — the macOS umlaut trap). Moved verbatim from internal/repair (F06);
// repair keeps SchemaFilename wrappers over these.
package library

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// SchemaFilename builds the schema name with .pdf extension.
func SchemaFilename(creators []Creator, year int, title string) string {
	return schemaFilename(creators, year, title, ".pdf", nil)
}

// SchemaFilenameForFormat picks the extension from the attachment's
// content type (#220: EPUB repairs upload .epub, not .pdf) and carries
// the document's existing attachment filenames for the #291 grown-pattern
// exception (empty/nil = no existing attachments → global schema).
func SchemaFilenameForFormat(creators []Creator, year int, title, contentType string, existing []string) string {
	ext := ".pdf"
	if strings.Contains(contentType, "epub") {
		ext = ".epub"
	}
	return schemaFilename(creators, year, title, ext, existing)
}

// pickCreatorName returns the first lastName (or the institutional
// single-field name, fieldMode 1) among creators of the given type — ""
// when none exists.
func pickCreatorName(creators []Creator, creatorType string) string {
	for _, c := range creators {
		if c.CreatorType != creatorType {
			continue
		}
		if c.Name != "" { // institutional creator (fieldMode 1)
			return c.Name
		}
		if c.LastName != "" {
			return c.LastName
		}
	}
	return ""
}

func schemaFilename(creators []Creator, year int, title, ext string, existing []string) string {
	// #287 cascade: author → first editor → institution. Documents with
	// no creators at all are honest as "Unbekannt" (fixable in provider
	// metadata) — the publisher must not masquerade as a person.
	head := pickCreatorName(creators, "author")
	if head == "" {
		head = pickCreatorName(creators, "editor")
	}
	if head == "" {
		head = "Unbekannt"
	}
	y := ""
	if year > 0 {
		y = fmt.Sprintf("%d", year)
	}
	stem := sanitize(head + " - " + y + " - " + cleanTitle(title))
	// Degenerate titles ("", ":", "  ") leave the tail separator
	// dangling ("Autor - 2024 -"); a real title ending in '-' can never
	// produce " -" here — sanitize collapses whitespace, so this suffix
	// is always the empty-title artifact.
	stem = strings.TrimSuffix(stem, " -")
	stem = adoptGrownPattern(stem, existing, ext)
	// #291 NFC: the on-disk name must be byte-identical to the API
	// filename — macOS decomposes umlauts (NFD); ONE canonical form end
	// to end makes the bytes match everywhere.
	return norm.NFC.String(stem) + ext
}

// cleanTitle applies the #291 title rules: ':' and '/' read as ' - '
// separators, and the SUBTITLE (text after the first ':') ships only
// when the joined title fits the 80-rune budget — length decides
// (Bradford keeps its subtitle, Flew loses it). An over-budget main
// title alone still truncates at a word boundary (shorten).
func cleanTitle(title string) string {
	main, sub, hasSub := strings.Cut(title, ":")
	main = sepToDash(strings.TrimSpace(main))
	if !hasSub {
		return shorten(main, 80)
	}
	sub = sepToDash(strings.TrimSpace(sub))
	if sub == "" { // 'Titel:' — empty subtitle must not leave a dangling ' - '
		return shorten(main, 80)
	}
	if joined := main + " - " + sub; utf8.RuneCountInString(joined) <= 80 {
		return joined
	}
	return shorten(main, 80) // subtitle misses the budget — dropped
}

func sepToDash(s string) string {
	s = strings.ReplaceAll(s, ":", " - ")
	return strings.ReplaceAll(s, "/", " - ")
}

// markerRe matches a grown format-marker suffix — KNOWN TAGS only
// (#291 review: a year like ' (2024)' is not a format tag; provenance
// words like '(Kopie)' never matched by design — provenance suffixes are
// forbidden by the convention).
var markerRe = regexp.MustCompile(` \((PDF|EPUB)\)$`)

// adoptGrownPattern implements the #291 grown-pattern exception: a
// document that already has attachments keeps ITS established naming —
// local consistency beats global uniformity. The FIRST existing name is
// the reference (callers order preferred-first — usually the attachment
// being replaced, whose name IS the document's pattern): a '+'-encoded
// stem (Springer style, 'Dubs,+R.+-+2004+-+…') re-encodes the schema stem
// with '+' for every space; a trailing ' (FORMAT)' marker is carried over
// with the NEW upload's own format tag. Name-component depth (e.g. the
// 'R.' initial of the reference) stays what the schema cascade produces —
// the pattern is the encoding, the content is current metadata.
func adoptGrownPattern(stem string, existing []string, ext string) string {
	if len(existing) == 0 || existing[0] == "" {
		return stem
	}
	ref := strings.TrimSuffix(existing[0], path.Ext(existing[0]))
	switch {
	case strings.Contains(ref, "+-+"):
		// The ENCODED SEPARATOR '+-+' is the Springer signature — a bare '+'
		// from a title (C++, C#) is not: a space-separated reference name
		// carrying 'C++' in its title must NOT flip the new name into
		// +-encoding (#291 review false positive).
		return strings.ReplaceAll(stem, " ", "+")
	case markerRe.MatchString(ref):
		tag := "PDF"
		if ext == ".epub" {
			tag = "EPUB"
		}
		return stem + " (" + tag + ")"
	}
	return stem
}

// shorten trims to n runes at a word boundary.
func shorten(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " -–:,"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " -–:,") + "…"
}

// sanitize strips path separators and control characters; keeps umlauts,
// &, +, % (the provider upload form round-trips them url-encoded).
var sanitizeRe = regexp.MustCompile(`[/\\\x00-\x1f]`)

func sanitize(s string) string {
	s = sanitizeRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}
