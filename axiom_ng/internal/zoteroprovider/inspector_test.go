// inspector_test.go — the real PDF inspector (rung 1): DOI/ISBN/year
// extraction from generated PDFs + the pure extractor battery + the
// honesty pins (EPUB empty, undecodable empty, no title guessing).
package zoteroprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

// buildPDF assembles a minimal VALID one-page PDF (uncompressed content
// stream, proper xref) carrying the given text lines.
func buildPDF(lines ...string) []byte {
	return buildPDFPages(lines...)
}

// buildPDFPages assembles a minimal VALID multi-page PDF — one page per
// argument (each argument is that page's text block).
func buildPDFPages(pages ...string) []byte {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "\\", "\\\\")
		s = strings.ReplaceAll(s, "(", "\\(")
		s = strings.ReplaceAll(s, ")", "\\)")
		return s
	}
	var objs []string
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i*2)
	}
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(pages)))
	fontObj := 3 + len(pages)*2
	for _, page := range pages {
		var content strings.Builder
		content.WriteString("BT /F1 12 Tf 72 720 Td 14 TL\n")
		for _, l := range strings.Split(page, "\n") {
			if l != "" {
				content.WriteString("(" + esc(l) + ") Tj T*\n")
			}
		}
		content.WriteString("ET\n")
		objs = append(objs, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents "+
			fmt.Sprintf("%d 0 R", len(objs)+2)+" /Resources << /Font << /F1 "+
			fmt.Sprintf("%d 0 R", fontObj)+" >> >> >>")
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
	}
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1) // 1-based
	for i, o := range objs {
		offsets[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xrefAt := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefAt)
	return []byte(b.String())
}

func stagePDF(t *testing.T, pdf []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(p, pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPDFInspectorExtractsDOIISBNAndYear(t *testing.T) {
	pdf := buildPDF(
		"Netzwerke und Plattformen",
		"Ein Titelblatt",
		"DOI: 10.5555/abc.def123",
		"ISBN 978-3-16-148410-0",
		"© 2021 Der Verlag",
	)
	got, err := NewPDFInspector().Inspect(context.Background(), "application/pdf", stagePDF(t, pdf))
	if err != nil {
		t.Fatal(err)
	}
	if got.DOI != "10.5555/abc.def123" {
		t.Fatalf("DOI = %q", got.DOI)
	}
	if got.ISBN != "9783161484100" {
		t.Fatalf("ISBN = %q", got.ISBN)
	}
	if got.Year == nil || *got.Year != 2021 {
		t.Fatalf("year = %+v", got.Year)
	}
	// Capability honesty: rung 1 does not guess titles.
	if got.Title != "" || len(got.Authors) > 0 || got.Publisher != "" {
		t.Fatalf("inspector must not guess unverifiable fields: %+v", got)
	}
}

func TestPDFInspectorHonestEmpty(t *testing.T) {
	insp := NewPDFInspector()
	ctx := context.Background()
	isEmpty := func(f library.ResolvedFields) bool {
		return f.Title == "" && f.Publisher == "" && f.Language == "" &&
			f.RecordType == "" && f.DOI == "" && f.ISBN == "" &&
			f.Year == nil && len(f.Authors) == 0
	}

	// EPUB: unsupported, empty — never emulated.
	got, err := insp.Inspect(ctx, "application/epub+zip", stagePDF(t, buildPDF("x")))
	if err != nil || !isEmpty(got) {
		t.Fatalf("epub inspect = %+v %v, want empty", got, err)
	}
	// Undecodable PDF: rung 1 cannot read it — empty, ladder continues.
	got, err = insp.Inspect(ctx, "application/pdf", stagePDF(t, []byte("%PDF-1.4 not really a pdf")))
	if err != nil || !isEmpty(got) {
		t.Fatalf("garbage pdf inspect = %+v %v, want empty", got, err)
	}
	// Readable page without identifiers: empty.
	got, err = insp.Inspect(ctx, "application/pdf", stagePDF(t, buildPDF("Nur ein Titel ohne Impressum")))
	if err != nil || !isEmpty(got) {
		t.Fatalf("plain page inspect = %+v %v, want empty", got, err)
	}
}

// The page bound (inspectPages=5) is DELIBERATE: title page + imprint
// live up front. A DOI first appearing on page 6 stays invisible — the
// bound is pinned so a regression to "scan everything" (slow, and
// hostile-PDF bait) fails here.
func TestPDFInspectorPageBoundIsFive(t *testing.T) {
	pages := make([]string, 6)
	for i := range pages {
		pages[i] = fmt.Sprintf("plain filler page %d", i+1)
	}
	pages[5] = "DOI: 10.5555/hidden.on.page.six"
	insp := NewPDFInspector()
	got, err := insp.Inspect(context.Background(), "application/pdf", stagePDF(t, buildPDFPages(pages...)))
	if err != nil {
		t.Fatal(err)
	}
	if got.DOI != "" {
		t.Fatalf("page 6 must be outside the inspectPages bound, got DOI %q", got.DOI)
	}
	// The same DOI on page 5 (inside the bound) is found — the test
	// pins the BOUND, not blindness.
	pages[4], pages[5] = "DOI: 10.5555/on.page.five", "filler"
	got, err = insp.Inspect(context.Background(), "application/pdf", stagePDF(t, buildPDFPages(pages...)))
	if err != nil {
		t.Fatal(err)
	}
	if got.DOI != "10.5555/on.page.five" {
		t.Fatalf("page 5 must be inside the bound, got DOI %q", got.DOI)
	}
}

func TestExtractImprintFieldsPure(t *testing.T) {
	cases := []struct {
		name, text string
		doi, isbn  string
		year       int
	}{
		{"doi trailing punctuation", "doi: 10.1000/x.123).", "10.1000/x.123", "", 0},
		{"isbn13 with dashes", "ISBN: 978-0-306-40615-7", "", "9780306406157", 0},
		{"isbn10", "ISBN 0-306-40615-2", "", "0306406152", 0},
		{"copyright word year", "Copyright 2019 by the authors", "", "", 2019},
		{"copyright symbol year", "© 2022 Springer", "", "", 2022},
		{"noise year ignored", "First printing, 1043 items sold in 2020", "", "", 0},
		{"isbn13 unicode hyphens", "ISBN\u2010978\u20113\u201116\u2011148410\u20110", "", "9783161484100", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := extractImprintFields(c.text)
			if f.DOI != c.doi || f.ISBN != c.isbn {
				t.Fatalf("ids = %q/%q want %q/%q (text %q)", f.DOI, f.ISBN, c.doi, c.isbn, c.text)
			}
			if c.year > 0 && (f.Year == nil || *f.Year != c.year) {
				t.Fatalf("year = %+v want %d", f.Year, c.year)
			}
			if c.year == 0 && f.Year != nil {
				t.Fatalf("year = %+v, want none", f.Year)
			}
		})
	}
}
