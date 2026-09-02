// #245 review C1: the persist boundary re-marshals the Locator from this
// typed struct — an unknown JSON field is silently dropped (W9 lesson).
// This test proves the APA fields survive the runner JSON → decode →
// re-marshal round-trip (it failed before the fields were added).
package repo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLocatorAPARoundTripSurvivesPersistBoundary(t *testing.T) {
	raw := `{"chunks":[{"ref":"chunk-0000","index":0,"text":"…","locator":{` +
		`"type":"epub_cfi","source":"epub","cfi_start":"/6/4!/4/2","cfi_end":"/6/4!/4/4",` +
		`"chapter":3,"chapter_number":5,"section_title":"Section B","paragraph_in_chapter":17}}]}`
	res, err := DecodeProcessorResult([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	loc := res.Chunks[0].Locator
	if loc.ChapterNumber == nil || *loc.ChapterNumber != 5 {
		t.Fatalf("chapter_number dropped at decode: %+v", loc)
	}
	if loc.SectionTitle != "Section B" {
		t.Fatalf("section_title dropped at decode: %+v", loc)
	}
	if loc.ParagraphInChapter == nil || *loc.ParagraphInChapter != 17 {
		t.Fatalf("paragraph_in_chapter dropped at decode: %+v", loc)
	}
	// the persist path re-marshals from the struct — keys must survive
	out, _ := json.Marshal(loc)
	for _, key := range []string{"chapter_number", "section_title", "paragraph_in_chapter"} {
		if !strings.Contains(string(out), `"`+key+`"`) {
			t.Fatalf("%s dropped at re-marshal: %s", key, out)
		}
	}
}
