// #173: the §11 page_source gate — DB-free witnesses for all three new
// codes plus the positive cases. The gate is the corpus's protection against
// unversioned runners; these tests are the sonde Hivemind flips.
package repo

import (
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
)

func trustFrozen() *FrozenInput {
	ct := "application/pdf"
	return &FrozenInput{Attachment: FrozenAttachment{ContentType: &ct}}
}

func trustResult(pageSource string) *processor.Result {
	return &processor.Result{
		Chunks: []processor.Chunk{{
			Ref: "chunk-0000", Index: 0, Text: "x",
			Locator: &processor.Locator{
				Type: "page_span", PageLabelStart: "1", Source: "marker_paginate",
				PageSource: pageSource,
			},
		}},
	}
}

func TestValidatePageSourceGate(t *testing.T) {
	// positive: all three page levels pass
	for _, lvl := range []string{
		processor.PageSourceFolioVerified,
		processor.PageSourcePDFLabelSane,
		processor.PageSourcePhysicalOnly,
	} {
		if err := validateLocatorsAndRelationships(trustResult(lvl), trustFrozen()); err != nil {
			t.Errorf("level %q must pass, got %v", lvl, err)
		}
	}
	// blank -> loud MISSING (the unversioned-runner guard)
	err := validateLocatorsAndRelationships(trustResult(""), trustFrozen())
	if err == nil || !strings.Contains(err.Error(), "LOCATOR_PAGE_SOURCE_MISSING") {
		t.Fatalf("blank page_source must fail with MISSING, got %v", err)
	}
	// garbage -> UNKNOWN
	err = validateLocatorsAndRelationships(trustResult("divined"), trustFrozen())
	if err == nil || !strings.Contains(err.Error(), "LOCATOR_PAGE_SOURCE_UNKNOWN") {
		t.Fatalf("unknown page_source must fail with UNKNOWN, got %v", err)
	}
	// epub_cfi consistency: must carry none (blank tolerated as legacy)
	epub := "application/epub+zip"
	frozen := &FrozenInput{Attachment: FrozenAttachment{ContentType: &epub}}
	ok := &processor.Result{Chunks: []processor.Chunk{{
		Ref: "chunk-0000", Index: 0, Text: "x",
		Locator: &processor.Locator{
			Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
			PageSource: "none",
		},
	}}}
	if err := validateLocatorsAndRelationships(ok, frozen); err != nil {
		t.Fatalf("epub_cfi with none must pass: %v", err)
	}
	bad := &processor.Result{Chunks: []processor.Chunk{{
		Ref: "chunk-0000", Index: 0, Text: "x",
		Locator: &processor.Locator{
			Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
			PageSource: processor.PageSourceFolioVerified,
		},
	}}}
	err = validateLocatorsAndRelationships(bad, frozen)
	if err == nil || !strings.Contains(err.Error(), "LOCATOR_PAGE_SOURCE_INCONSISTENT") {
		t.Fatalf("epub_cfi with folio_verified must fail with INCONSISTENT, got %v", err)
	}
	// #220: print_unverified claims a print page from an anchor map — the
	// claim is unfounded without page_start (additive field must be there).
	pagelist := &processor.Result{Chunks: []processor.Chunk{{
		Ref: "chunk-0000", Index: 0, Text: "x",
		Locator: &processor.Locator{
			Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
			PageSource: processor.PageSourcePrintUnverified,
		},
	}}}
	err = validateLocatorsAndRelationships(pagelist, frozen)
	if err == nil || !strings.Contains(err.Error(), "LOCATOR_PAGE_SOURCE_INCONSISTENT") ||
		!strings.Contains(err.Error(), "page_start") {
		t.Fatalf("print_unverified without page_start must fail with INCONSISTENT mentioning page_start, got %v", err)
	}
	pageTen := 10
	pagelistOk := &processor.Result{Chunks: []processor.Chunk{{
		Ref: "chunk-0000", Index: 0, Text: "x",
		Locator: &processor.Locator{
			Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
			PageSource: processor.PageSourcePrintUnverified,
			PageStart:  &pageTen, PageEnd: &pageTen,
		},
	}}}
	if err := validateLocatorsAndRelationships(pagelistOk, frozen); err != nil {
		t.Fatalf("print_unverified with page_start must pass: %v", err)
	}

	// #226: the FULL EPUB trust set — every level with pages passes,
	// derived_from_sibling without page_start fails like the others.
	for _, lvl := range []string{
		processor.PageSourcePrintVerified,
		processor.PageSourceDerivedFromSibling,
		processor.PageSourcePrintUnverified,
	} {
		okAll := &processor.Result{Chunks: []processor.Chunk{{
			Ref: "chunk-0000", Index: 0, Text: "x",
			Locator: &processor.Locator{
				Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
				PageSource: lvl, PageStart: &pageTen, PageEnd: &pageTen,
			},
		}}}
		if err := validateLocatorsAndRelationships(okAll, frozen); err != nil {
			t.Fatalf("epub_cfi with %s + page_start must pass: %v", lvl, err)
		}
		bare := &processor.Result{Chunks: []processor.Chunk{{
			Ref: "chunk-0000", Index: 0, Text: "x",
			Locator: &processor.Locator{
				Type: "epub_cfi", CFIStart: "/6/4", CFIEnd: "/6/8", Source: "epub",
				PageSource: lvl,
			},
		}}}
		if err := validateLocatorsAndRelationships(bare, frozen); err == nil ||
			!strings.Contains(err.Error(), "page_start") {
			t.Fatalf("epub_cfi with %s but no page_start must fail mentioning page_start, got %v", lvl, err)
		}
	}
}
