// inspector.go — the real DocumentInspector (ladder rung 1, F07 #301):
// metadata-page scan of the document CONTENT. For PDF the inspector
// reads the first pages' text and extracts what is RELIABLY extractable:
// a DOI or ISBN printed on the title page/imprint, and the copyright-line
// year. Everything else stays empty — capability honesty beats guessing:
// a heuristic "first big line is the title" would poison rung 1 (document
// fields are LOCKED against weaker rungs), so the inspector does not
// guess titles. EPUB reports empty fields (unsupported, never emulated —
// the F06 fake stays the deterministic test stand-in).
package zoteroprovider

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
	pdflib "github.com/ledongthuc/pdf"
)

// PDFInspector extracts rung-1 fields from PDF content.
type PDFInspector struct{}

// NewPDFInspector builds the inspector.
func NewPDFInspector() PDFInspector { return PDFInspector{} }

// inspectPages bounds the text scan (title page + imprint are up front).
const inspectPages = 5

// Inspect implements library.DocumentInspector.
func (PDFInspector) Inspect(_ context.Context, mediaType, stagingPath string) (library.ResolvedFields, error) {
	if !strings.Contains(mediaType, "pdf") {
		// Capability honesty: only PDF content inspection exists.
		return library.ResolvedFields{}, nil
	}
	f, r, err := pdflib.Open(stagingPath)
	if err != nil {
		// An undecodable PDF is honest-empty, not failed: rung 1 simply
		// cannot read it and the ladder continues with hints/resolvers.
		return library.ResolvedFields{}, nil
	}
	defer f.Close()
	var b strings.Builder
	for i := 1; i <= inspectPages && i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, terr := p.GetPlainText(nil)
		if terr != nil {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
		if b.Len() > 1<<20 {
			break // bound: a broken "first page" must not OOM the rung
		}
	}
	return extractImprintFields(b.String()), nil
}

var (
	doiRe = regexp.MustCompile(`10\.\d{4,9}/[^\s"<>]+`)
	// ISBN-13: distinctive 978/979 prefix, separators optional, no
	// keyword needed. ISBN-10: no distinct prefix, so the ISBN keyword is
	// REQUIRED (a bare 10-digit run would false-positive on years, prices,
	// phone numbers). Two patterns tried in order — one combined
	// alternation lets the keyword branch shadow the 13-digit match
	// ("ISBN: 978-…" matched as a short keyworded form).
	isbn13Re = regexp.MustCompile(`\b97[89](?:[-\x{2010}\x{2011} ]?\d){10}`)
	isbn10Re = regexp.MustCompile(`ISBN[-\x{2010}\x{2011} :]+\d{1,5}[-\x{2010}\x{2011} ]?\d{1,7}[-\x{2010}\x{2011} ]?\d{1,7}[-\x{2010}\x{2011} ]?[\dX]`)
	yearRe   = regexp.MustCompile(`©\s*(\d{4})|(?:copyright|Copyright)\s*(?:©)?\s*(\d{4})`)
)

// extractImprintFields is pure (unit-testable without a PDF): DOI, ISBN
// and copyright year from the scanned imprint text.
func extractImprintFields(text string) library.ResolvedFields {
	fields := library.ResolvedFields{}
	if m := doiRe.FindString(text); m != "" {
		fields.DOI = library.NormalizeDOI(strings.TrimRight(m, ".,;)"))
	}
	if m := isbn13Re.FindString(text); m != "" {
		fields.ISBN = library.NormalizeISBN(m)
	} else if m := isbn10Re.FindString(text); m != "" {
		// the keyword is part of the ISBN-10 match — strip it
		m = strings.TrimLeft(strings.TrimPrefix(strings.ToUpper(m), "ISBN"), "-\u2010\u2011 :")
		fields.ISBN = library.NormalizeISBN(m)
	}
	if mm := yearRe.FindStringSubmatch(text); mm != nil {
		for _, g := range mm[1:] {
			if y, err := strconv.Atoi(g); err == nil && y >= 1000 && y <= 3000 {
				yy := y
				fields.Year = &yy
				break
			}
		}
	}
	return fields
}

// compile-time port assertion.
var _ library.DocumentInspector = PDFInspector{}
