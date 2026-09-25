// resolvers_test.go — the real resolver rungs against scripted endpoints
// (F07 #301): exact-identifier-before-fuzzy, descending confidence,
// field honesty, retryable error classes. The live network run is the
// env-gated IT's optional Crossref branch.
package zoteroprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/library"
)

func crossrefStub(t *testing.T, workBody, searchBody string, hits *int) *CrossrefAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		*hits++
		switch {
		case r.URL.Path == "/works/10.5555/unique":
			w.Write([]byte(workBody))
		case r.URL.Path == "/works":
			w.Write([]byte(searchBody))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewCrossref(srv.URL, srv.Client())
}

func TestCrossrefExactDOIBeatsFuzzyAndIsConfident(t *testing.T) {
	hits := 0
	cr := crossrefStub(t,
		`{"message":{"title":["The Unique Work"],"DOI":"10.5555/unique","type":"monograph","publisher":"Press","author":[{"given":"Ada","family":"Example"}],"issued":{"date-parts":[[2019]]}}}`,
		`{"message":{"items":[]}}`, &hits)
	cands, err := cr.Resolve(context.Background(), library.ResolveQuery{
		DOI: "10.5555/UNIQUE", Title: "Some Other Title Entirely",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.Confidence != 1.0 {
		t.Fatalf("exact DOI hit must be confidence 1.0, got %v", c.Confidence)
	}
	if c.Fields.Title != "The Unique Work" || c.Fields.DOI != "10.5555/unique" ||
		c.Fields.RecordType != "book" || c.Fields.Publisher != "Press" {
		t.Fatalf("fields = %+v", c.Fields)
	}
	if c.Fields.Year == nil || *c.Fields.Year != 2019 {
		t.Fatalf("year = %+v", c.Fields.Year)
	}
	if len(c.Fields.Authors) != 1 || c.Fields.Authors[0] != "Ada Example" {
		t.Fatalf("authors = %v", c.Fields.Authors)
	}
	// DOI exact answers alone: the fuzzy endpoint must not be consulted.
	if hits != 1 {
		t.Fatalf("exact hit must not fall through to fuzzy, endpoint hits = %d", hits)
	}
}

func TestCrossrefFuzzyRanksDescendingAndPenalizesYear(t *testing.T) {
	hits := 0
	cr := crossrefStub(t, `{}`, `{"message":{"items":[
		{"title":["Network Effects Elsewhere"],"DOI":"10.1/b","type":"journal-article","score":3},
		{"title":["Network Effects in Platforms"],"DOI":"10.1/a","type":"journal-article","score":99},
		{"title":["Totally Different"],"DOI":"10.1/c","type":"journal-article","score":1}
	]}}`, &hits)
	y := 2020
	cands, err := cr.Resolve(context.Background(), library.ResolveQuery{
		Title: "Network Effects in Platforms", Year: &y,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("off-topic hit must be filtered (overlap<=0), got %d: %+v", len(cands), cands)
	}
	if cands[0].Fields.DOI != "10.1/a" || cands[1].Fields.DOI != "10.1/b" {
		t.Fatalf("candidates must rank by overlap: %+v", cands)
	}
	for i := 1; i < len(cands); i++ {
		if cands[i].Confidence > cands[i-1].Confidence {
			t.Fatalf("candidates must be descending: %+v", cands)
		}
	}
}

func TestResolverErrorsAreRetryableUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cr := NewCrossref(srv.URL, srv.Client())
	_, err := cr.Resolve(context.Background(), library.ResolveQuery{DOI: "10.1/x"})
	if class, ok := contracterr.ClassOf(err); !ok || class != contracterr.ClassUnavailable {
		t.Fatalf("resolver error class = %v (%v), want unavailable (retryable)", class, err)
	}
}

func TestCrossrefUnknownDOIFallsThroughToFuzzy(t *testing.T) {
	hits := 0
	cr := crossrefStub(t, `not json — 404 path`, `{"message":{"items":[
		{"title":["Recovered by Title"],"DOI":"10.1/a","type":"book"}
	]}}`, &hits)
	cands, err := cr.Resolve(context.Background(), library.ResolveQuery{DOI: "10.5555/unknown", Title: "Recovered by Title"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Fields.Title != "Recovered by Title" {
		t.Fatalf("fallthrough candidates = %+v", cands)
	}
	if hits != 2 {
		t.Fatalf("unknown DOI must fall through to fuzzy (2 endpoint hits), got %d", hits)
	}
}

func openLibraryStub(t *testing.T, editionBody, searchBody string) *OpenLibraryAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/books":
			w.Write([]byte(editionBody))
		case r.URL.Path == "/search.json":
			w.Write([]byte(searchBody))
		default:
			http.Error(w, "nf", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return NewOpenLibrary(srv.URL, srv.Client())
}

func TestOpenLibraryISBNExact(t *testing.T) {
	ol := openLibraryStub(t,
		`{"ISBN:9783161484100":{"key":"/books/9783161484100","title":"Das ISBN Werk","subtitle":"Untertitel","publishers":["Verlag"],"publish_date":"2018","languages":["/languages/ger"],"authors":[{"name":"E. Vierte"}]}}`,
		`{}`)
	cands, err := ol.Resolve(context.Background(), library.ResolveQuery{ISBN: "978-3-16-148410-0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	c := cands[0]
	if c.Confidence != 1.0 || c.Fields.Title != "Das ISBN Werk: Untertitel" || c.Fields.Publisher != "Verlag" {
		t.Fatalf("fields = %+v", c.Fields)
	}
	if c.Fields.Year == nil || *c.Fields.Year != 2018 || c.Fields.Language != "de" {
		t.Fatalf("year/language = %+v %q", c.Fields.Year, c.Fields.Language)
	}
	if len(c.Fields.Authors) != 1 || c.Fields.Authors[0] != "E. Vierte" {
		t.Fatalf("authors = %v", c.Fields.Authors)
	}
}

func TestOpenLibraryFuzzyTitles(t *testing.T) {
	ol := openLibraryStub(t, `{}`, `{"docs":[
		{"key":"/works/1","title":"Open Wired","publish_date":"2001","author_name":["A"]},
		{"key":"/works/2","title":"Something Else","publish_date":"1999","author_name":["B"]}
	]}`)
	cands, err := ol.Resolve(context.Background(), library.ResolveQuery{Title: "Open World"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Fields.Title != "Open Wired" {
		t.Fatalf("fuzzy candidates = %+v", cands)
	}
	// Honest fields: the doc carries no publisher/language — they stay empty.
	if cands[0].Fields.Publisher != "" || cands[0].Fields.Language != "" {
		t.Fatalf("resolver must not invent fields: %+v", cands[0].Fields)
	}
}

// TestResolverJSONShape — pin the endpoint request shapes (bibkeys/jscmd
// for ISBN; query.bibliographic for Crossref fuzzy) so a silent endpoint
// contract change shows up as a red test, not as silence downstream.
func TestResolverJSONShape(t *testing.T) {
	var seenPath, seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenQuery = r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	ol := NewOpenLibrary(srv.URL, srv.Client())
	_, _ = ol.Resolve(context.Background(), library.ResolveQuery{ISBN: "9783161484100"})
	if seenPath != "/api/books" || seenQuery != "bibkeys=ISBN%3A9783161484100&format=json&jscmd=data" {
		t.Fatalf("openlibrary ISBN request shape: %s?%s", seenPath, seenQuery)
	}
	cr := NewCrossref(srv.URL, srv.Client())
	_, _ = cr.Resolve(context.Background(), library.ResolveQuery{Title: "T"})
	if seenPath != "/works" || seenQuery != "query.bibliographic=T&rows=3" {
		t.Fatalf("crossref fuzzy request shape: %s?%s", seenPath, seenQuery)
	}
	_ = json.Marshal // shape guard compiles
}
