// A1 #165: /api/passage route degradation + error mapping.
package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

type stubPassage struct {
	p   *search.Passage
	err error
}

func (s stubPassage) GetPassage(ctx context.Context, chunkID string) (*search.Passage, error) {
	return s.p, s.err
}

func passageReq(t *testing.T, svc PassageService, id string) *httptest.ResponseRecorder {
	t.Helper()
	s := New(":0", log.Default())
	if svc != nil {
		s.SetPassageService(svc)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/passage/"+id, nil))
	return rec
}

func TestPassageRoute_NotConfigured503(t *testing.T) {
	rec := passageReq(t, nil, "00000000-0000-4000-8000-000000000001")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

func TestPassageRoute_BadUUID400(t *testing.T) {
	rec := passageReq(t, stubPassage{}, "not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestPassageRoute_Unknown404(t *testing.T) {
	rec := passageReq(t, stubPassage{err: search.ErrPassageNotFound},
		"00000000-0000-4000-8000-000000000001")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != "chunk not found" {
		t.Fatalf("plain 404 must not hint: %s", rec.Body.String())
	}
}

func TestPassageRoute_InactiveSnapshot404WithHint(t *testing.T) {
	rec := passageReq(t, stubPassage{err: &search.InactiveSnapshotError{
		ChunkID: "00000000-0000-4000-8000-000000000001", SnapshotID: "snap-old", AttachmentID: "att-A"}},
		"00000000-0000-4000-8000-000000000001")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	var body struct {
		Hint     string `json:"hint"`
		Snapshot string `json:"snapshot"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Hint == "" || body.Snapshot != "snap-old" {
		t.Fatalf("inactive 404 must carry the hint payload: %s", rec.Body.String())
	}
}

func TestPassageRoute_OK(t *testing.T) {
	rec := passageReq(t, stubPassage{p: &search.Passage{ChunkID: "00000000-0000-4000-8000-000000000001"}},
		"00000000-0000-4000-8000-000000000001")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// #194 exact-page citations: /api/passage/{id}/page?at=N derives the exact
// print page from the per-paragraph map. The proven live case as fixture:
// Altenburger chunk 04881089 cites span S. 8-11; the sentence at offset 610
// sits on print page 9 — the endpoint must answer 9, not the span.
func TestPassagePageAt_ExactPageFromMap(t *testing.T) {
	stub := stubPassage{p: &search.Passage{
		ChunkID: "04881089-3833-420f-a8a9-5b493e4a7d56",
		Text:    "Absatz auf Seite acht ...\n\nDer zitierte Satz steht auf Seite neun.\n\nSeite zehn ...",
		Locator: search.LocatorView{Kind: "page", Label: "S. 8-11", PageSource: "folio_verified"},
		ParagraphPages: [][]string{
			{"0", "8"}, {"412", "9"}, {"1900", "10"},
		},
	}}
	s := New(":0", log.Default())
	s.SetPassageService(stub)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/passage/04881089-3833-420f-a8a9-5b493e4a7d56/page?at=610", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Page string `json:"page"`
		At   int    `json:"at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body.String())
	}
	if body.Page != "9" {
		t.Fatalf("the S.9 sentence must resolve to page 9, got %q", body.Page)
	}
}

func TestPassagePageAt_NoMap404(t *testing.T) {
	stub := stubPassage{p: &search.Passage{
		ChunkID: "00000000-0000-4000-8000-000000000002",
		Locator: search.LocatorView{Kind: "page", Label: "S. 8-11"},
	}}
	s := New(":0", log.Default())
	s.SetPassageService(stub)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/passage/00000000-0000-4000-8000-000000000002/page?at=5", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pre-#194 chunk without map must 404 with the span hint, got %d", rec.Code)
	}
}

func TestPassagePageAt_BadOffset400(t *testing.T) {
	s := New(":0", log.Default())
	s.SetPassageService(stubPassage{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/passage/04881089-3833-420f-a8a9-5b493e4a7d56/page?at=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for non-numeric ?at, got %d", rec.Code)
	}
}
