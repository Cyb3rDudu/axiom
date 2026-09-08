package search

// #255 unit proofs: contextual locator rendering ("Folie N", never a page
// citation form) and the always-present citation_class on the source block.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

func TestRenderLocatorContextualFolie(t *testing.T) {
	// Slide PDF: physical index only → "Folie 12" (not "PDF-S. 12").
	v := renderLocator(json.RawMessage(`{"type":"page_span","physical_page_start":11,"page_source":"physical_only"}`), nil, true)
	if v.Kind != "page" || v.Label != "Folie 12" {
		t.Fatalf("physical slide must render Folie 12, got kind=%s label=%q", v.Kind, v.Label)
	}
	// Physical span → "Folie 12-13".
	v = renderLocator(json.RawMessage(`{"type":"page_span","physical_page_start":11,"physical_page_end":12,"page_source":"physical_only"}`), nil, true)
	if v.Label != "Folie 12-13" {
		t.Fatalf("physical span must render Folie 12-13, got %q", v.Label)
	}
	// Chapter composition rides along: "Kap. 3, Folie 5".
	v = renderLocator(json.RawMessage(`{"type":"page_span","physical_page_start":4,"page_source":"physical_only","chapter":3}`), nil, true)
	if v.Label != "Kap. 3, Folie 5" {
		t.Fatalf("chapter+slide must render Kap. 3, Folie 5, got %q", v.Label)
	}
	// physical_only with NO physical index: nothing trustworthy — bare chapter,
	// never a dangling "Folie ".
	v = renderLocator(json.RawMessage(`{"type":"page_span","page_source":"physical_only"}`), []string{"Einführung"}, true)
	if v.Label != "Einführung" {
		t.Fatalf("no-number slide must render bare chapter, got %q", v.Label)
	}
	// Labeled slides (trusted labels) keep the label form under the Folie prefix.
	v = renderLocator(json.RawMessage(`{"type":"page_span","page_label_start":"12","page_source":"folio_verified"}`), nil, true)
	if v.Label != "Folie 12" {
		t.Fatalf("labeled slide must render Folie 12, got %q", v.Label)
	}
	// Transcript EPUB: contextual changes NOTHING on the epub_cfi arm (the
	// client composes provenance; paragraphs carry [MM:SS] in their text).
	ve := renderLocator(json.RawMessage(`{"type":"epub_cfi","cfi_start":"epubcfi(/6/4!/4/10,/1:0)","chapter_number":2,"section_title":"Angebot und Nachfrage"}`), []string{"Angebot und Nachfrage"}, true)
	if ve.Kind != "epub_cfi" || ve.Label != "Kap. 2" {
		t.Fatalf("contextual epub must keep the section form, got kind=%s label=%q", ve.Kind, ve.Label)
	}
	// Passenger guard: the SAME physical_only locator on a citable document
	// still renders the #173 trust form — the branch is class-gated, not global.
	c := renderLocator(json.RawMessage(`{"type":"page_span","physical_page_start":11,"page_source":"physical_only"}`), nil, false)
	if c.Label != "PDF-S. 12" {
		t.Fatalf("citable physical_only must keep PDF-S. 12, got %q", c.Label)
	}
}

func TestSourceViewCitationClassAlwaysPresent(t *testing.T) {
	// Explicit contextual class rides the contract.
	v := repo.DocumentMeta{Title: "VWL Vorlesung", CitationClass: "contextual"}.View("d1")
	if v.CitationClass != "contextual" {
		t.Fatalf("contextual must ride SourceView, got %q", v.CitationClass)
	}
	// Unknown/legacy degrades to citable — the field is ALWAYS on the wire.
	v = repo.DocumentMeta{Title: "Old Book"}.View("d2")
	if v.CitationClass != "citable" {
		t.Fatalf("missing class must default citable, got %q", v.CitationClass)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"citation_class":"citable"`) {
		t.Fatalf("citation_class must always serialize, got %s", b)
	}
}
