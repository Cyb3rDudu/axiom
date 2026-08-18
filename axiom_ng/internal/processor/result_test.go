package processor

import (
	"encoding/json"
	"strings"
	"testing"
)

// R5 review fix: sparse result weights arrive in BOTH forms — §10 strings
// from the real runner, native numbers from the reference backend — and both
// must decode. The internal shape stays string (validate/persist expect it).
func TestSparseValuesDecodeBothForms(t *testing.T) {
	// §10 canonical string form.
	var s SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":"0.5","7":"2"}}`), &s); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if s.Values["12"] != "0.5" || s.Values["7"] != "2" {
		t.Fatalf("string form wrong: %v", s.Values)
	}

	// Native numbers (reference backend): decoding must not fail, values
	// convert to the canonical string shape.
	var n SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":0.5,"7":2}}`), &n); err != nil {
		t.Fatalf("number form: %v", err)
	}
	if n.Values["12"] != "0.5" || n.Values["7"] != "2" {
		t.Fatalf("number form wrong: %v", n.Values)
	}

	// Mixed form decodes element-wise.
	var m SparseEmbedding
	if err := json.Unmarshal([]byte(`{"model":"m","values":{"12":"0.5","7":2}}`), &m); err != nil {
		t.Fatalf("mixed form: %v", err)
	}
	if m.Values["12"] != "0.5" || m.Values["7"] != "2" {
		t.Fatalf("mixed form wrong: %v", m.Values)
	}

	// Marshal keeps the §10 string wire shape.
	out, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"12":"0.5"`) || !strings.Contains(string(out), `"7":"2"`) {
		t.Fatalf("marshal must keep the string form: %s", out)
	}
}

func TestSparseValuesRejectBadWeights(t *testing.T) {
	for _, bad := range []string{
		`{"x":null}`,  // null weight
		`{"x":true}`,  // neither string nor number
		`{"x":["1"]}`, // array
	} {
		var v SparseValues
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
}

// TestLocatorChapterRoundTrip (#188 W12 fix window): the runner stamps
// locator.chapter (1-based ordinal, corroborated chapter-relative books);
// the persistence boundary re-marshals the Go Locator struct, so a field
// missing from the struct is silently DROPPED between contract and DB —
// exactly how the W9 wave lost every chapter stamp while both test tiers
// stayed green (Go ignores unknown JSON fields on unmarshal). This is the
// witness class for unknown-field drops at persistence boundaries.
func TestLocatorChapterRoundTrip(t *testing.T) {
	in := `{"type":"page_span","physical_page_start":33,"physical_page_end":33,
	        "page_label_start":"3","source":"marker_paginate",
	        "page_source":"folio_verified","chapter":2}`
	var loc Locator
	if err := json.Unmarshal([]byte(in), &loc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(loc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["chapter"] != float64(2) {
		t.Fatalf("locator.chapter did not survive the round-trip (got %v) — "+
			"the persist boundary drops it and the DB never sees the stamp", back["chapter"])
	}
	// Byte-identity for unstamped locators: nil *int + omitempty must
	// OMIT the key entirely (a dropped omitempty would emit "chapter":null).
	bare, _ := json.Marshal(Locator{Type: "page_span", Source: "marker_paginate"})
	var bareBack map[string]any
	_ = json.Unmarshal(bare, &bareBack)
	if _, present := bareBack["chapter"]; present {
		t.Fatalf("unstamped locator must not carry a chapter key: %s", bare)
	}
	// The chunk-level path too: a full contract chunk round-trips its locator.
	var ch Chunk
	chunkJSON := `{"ref":"chunk-0000","index":0,"text":"x","token_count":1,
	               "locator":` + in + `,
	               "structure":{"section_titles":[]}}`
	if err := json.Unmarshal([]byte(chunkJSON), &ch); err != nil {
		t.Fatal(err)
	}
	out2, _ := json.Marshal(ch.Locator)
	var back2 map[string]any
	_ = json.Unmarshal(out2, &back2)
	if back2["chapter"] != float64(2) {
		t.Fatalf("chunk locator round-trip lost chapter (got %v)", back2["chapter"])
	}
}
