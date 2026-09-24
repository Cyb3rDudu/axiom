// v1_parity_test.go — F05 #299 parity witness: every canonical /api/v1
// route and its unversioned compat sister deliver IDENTICAL responses for
// identical requests. Compared dimensions: status, Content-Type, body
// bytes (per-request headers like X-Request-Id are deliberately NOT
// compared — the middleware stack is shared process-wide, not a route
// property). The handlers are literally the same closures registered
// twice; this test guards against a future edit that diverges them (a
// separate handler, a route-scoped middleware, a rewrite of one side) —
// the strangler zug's core promise is that the versioned edge NEVER
// drifts from the compat edge. Note on case coverage: against the FAKE
// services, "blank query" is a 200-shape comparison (validation lives in
// the real search service); the service-error → 503 class gets its own
// dedicated pair below.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/search"
	"github.com/go-chi/chi/v5"
)

// paritySearch returns one fixed hit so the happy-path response body is
// deterministic and comparable.
type paritySearch struct{ err error }

func (f paritySearch) Search(ctx context.Context, req search.Request) (*search.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &search.Response{
		Query: req.Query, TopN: 1, Reranked: true,
		Arms:   search.Arms{Dense: true, BM25: true},
		Hits:   []search.Hit{{ChunkID: "c1", Text: "text", Score: 1.5}},
		TookMS: 2,
	}, nil
}

// parityPassage returns one fixed passage; err != nil maps the 404 path.
type parityPassage struct{ err error }

func (f parityPassage) GetPassage(ctx context.Context, id string) (*search.Passage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &search.Passage{ChunkID: id, Text: "text", ChunkIndex: 3, Section: []string{"Kap. 1"}}, nil
}

func newParityServer(svc SearchService, psvc PassageService) *Server {
	srv := New("127.0.0.1:0", log.New(os.Stderr, "", 0))
	if svc != nil {
		srv.SetSearchService(svc)
	}
	if psvc != nil {
		srv.SetPassageService(psvc)
	}
	return srv
}

// parityOnce fires the same request at the v1 and the compat route and
// compares status, content type and body bytes; a difference is returned
// as an error (callers decide fail vs expect — the teeth sonde expects one).
func parityOnce(h http.Handler, method, v1Path, compatPath string, body []byte) error {
	do := func(path string) *httptest.ResponseRecorder {
		var rd *bytes.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		} else {
			rd = bytes.NewReader([]byte{})
		}
		req := httptest.NewRequest(method, path, rd)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	a, b := do(v1Path), do(compatPath)
	if a.Code == 0 {
		return fmt.Errorf("%s: handler did not answer", v1Path)
	}
	if a.Code != b.Code {
		return fmt.Errorf("%s: v1 status %d != compat status %d", v1Path, a.Code, b.Code)
	}
	if a.Header().Get("Content-Type") != b.Header().Get("Content-Type") {
		return fmt.Errorf("%s: v1 content-type %q != compat %q", v1Path,
			a.Header().Get("Content-Type"), b.Header().Get("Content-Type"))
	}
	if !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
		return fmt.Errorf("%s: response bodies diverged:\nv1:     %s\ncompat: %s", v1Path, a.Body.Bytes(), b.Body.Bytes())
	}
	return nil
}

const uuidOK = "0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d"

// errOops is the fake's plain service error (drives the 503 default path).
var errOops = errors.New("fake service failure")

func TestV1Parity(t *testing.T) {
	h := newParityServer(paritySearch{}, parityPassage{}).Handler()
	searchBody := []byte(`{"query":"test","top_n":5}`)

	check := func(what string, hh http.Handler, method, v1, compat string, body []byte) {
		t.Helper()
		if err := parityOnce(hh, method, v1, compat, body); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	check("health", h, http.MethodGet, "/api/v1/health", "/api/health", nil)
	check("search 200", h, http.MethodPost, "/api/v1/search", "/api/search", searchBody)
	check("search 400 bad body", h, http.MethodPost, "/api/v1/search", "/api/search", []byte(`{nope`))
	check("search 200 blank-query shape (fake does not validate)", h, http.MethodPost, "/api/v1/search", "/api/search", []byte(`{"query":"  "}`))

	// Service-error class: a failing search service must degrade on BOTH
	// edges alike (the fake returns a plain error → the handler's 503
	// default branch).
	h503svc := newParityServer(paritySearch{err: errOops}, parityPassage{}).Handler()
	check("search 503 service error", h503svc, http.MethodPost, "/api/v1/search", "/api/search", []byte(`{"query":"x"}`))
	check("passage 200", h, http.MethodGet, "/api/v1/passage/"+uuidOK, "/api/passage/"+uuidOK, nil)
	check("passage 400 non-uuid", h, http.MethodGet, "/api/v1/passage/not-a-uuid", "/api/passage/not-a-uuid", nil)

	h404 := newParityServer(nil, parityPassage{err: search.ErrPassageNotFound}).Handler()
	check("passage 404 unknown", h404, http.MethodGet, "/api/v1/passage/"+uuidOK, "/api/passage/"+uuidOK, nil)

	// Degraded shape: neither service wired — both edges must degrade alike.
	h503 := newParityServer(nil, nil).Handler()
	check("search 503", h503, http.MethodPost, "/api/v1/search", "/api/search", searchBody)
	check("passage 503", h503, http.MethodGet, "/api/v1/passage/"+uuidOK, "/api/passage/"+uuidOK, nil)
}

// TestV1ParityHasTeeth — mutation sonde: a DIVERGED handler on one edge
// must fail the parity compare (guards against a vacuous green if both
// routes ever pointed at stubs that trivially agree).
func TestV1ParityHasTeeth(t *testing.T) {
	outer := chi.NewMux()
	// Shadow the v1 edge with a diverging handler (the outer static route
	// wins over the mounted subtree), then prove the compare catches it.
	outer.Post("/api/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"diverged": "yes"})
	})
	outer.Mount("/", newParityServer(paritySearch{}, parityPassage{}).Handler())

	if err := parityOnce(outer, http.MethodPost, "/api/v1/search", "/api/search", []byte(`{"query":"x"}`)); err == nil {
		t.Fatal("parity compare did not catch the diverged edge — the witness has no teeth")
	}
}
