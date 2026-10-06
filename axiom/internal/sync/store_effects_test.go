package sync

import (
	"encoding/json"
	"testing"
)

// TestAuthorStringsTrimsWhitespace pins the projection's author
// flattening: outer-trimmed names, never a leading/trailing-space
// author, empty parts collapse away (outer-trim parity with the 0004
// backfill's btrim — #358 review).
func TestAuthorStringsTrimsWhitespace(t *testing.T) {
	raw := `[
	  {"creatorType":"author","firstName":"Ada","lastName":"Lovelace"},
	  {"creatorType":"author","firstName":" ","lastName":" Curie "},
	  {"name":"  Solo Name ","creatorType":"author"},
	  {"creatorType":"author","firstName":"","lastName":""}
	]`
	got := authorStrings(json.RawMessage(raw))
	want := []string{"Ada Lovelace", "Curie", "Solo Name"}
	if len(got) != len(want) {
		t.Fatalf("authors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("author[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
