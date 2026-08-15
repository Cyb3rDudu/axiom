package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
)

type fakeSearch struct {
	req  search.Request
	resp *search.Response
	err  error
}

func (f *fakeSearch) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	f.req = req
	return f.resp, f.err
}

func TestSearchRouteNotConfigured(t *testing.T) {
	s := New(":0", log.Default())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/search",
		strings.NewReader(`{"query":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without service, got %d", rec.Code)
	}
}

func TestSearchRouteBadBody(t *testing.T) {
	s := New(":0", log.Default())
	s.SetSearchService(&fakeSearch{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/search",
		strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid body, got %d", rec.Code)
	}
}

func TestSearchRouteBadRequestMapped(t *testing.T) {
	s := New(":0", log.Default())
	s.SetSearchService(&fakeSearch{err: search.ErrBadRequest("query must not be blank")})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/search",
		strings.NewReader(`{"query":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for ErrBadRequest, got %d", rec.Code)
	}
}

func TestSearchRouteOK(t *testing.T) {
	s := New(":0", log.Default())
	s.SetSearchService(&fakeSearch{resp: &search.Response{
		Query: "csr", TopN: 1, Reranked: true,
		Arms: search.Arms{Dense: true, BM25: true},
		Hits: []search.Hit{{ChunkID: "c1", Text: "treffer", Score: 0.9,
			Source:  search.Source{Book: "B", Authors: []string{"A"}},
			Locator: search.LocatorView{Kind: "page", Label: "S. 47"}}},
	}})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/search",
		strings.NewReader(`{"query":"csr","top_n":1}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Reranked bool `json:"reranked"`
		Hits     []struct {
			ChunkID string `json:"chunk_id"`
			Source  struct {
				Book string `json:"book"`
			} `json:"source"`
			Locator struct {
				Label string `json:"label"`
			} `json:"locator"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Reranked || len(body.Hits) != 1 || body.Hits[0].Source.Book != "B" || body.Hits[0].Locator.Label != "S. 47" {
		t.Fatalf("response shape wrong: %s", rec.Body.String())
	}
}
