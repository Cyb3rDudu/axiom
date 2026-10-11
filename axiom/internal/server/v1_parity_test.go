// v1_parity_test.go — #368 (ADR 0001 executed, v0.2.4): the compat edge
// is GONE. The old F05 #299 parity witness (one handler, two
// registrations, byte-identical responses) retired with the unversioned
// routes; this file is now the ROUTE-REMOVAL SONDE of the strand: every
// one of the 13 removed unversioned paths must answer 404, the canonical
// /api/v1 twin must still answer, and the unversioned operational
// surfaces (ws, runners/live, processor/source, kg/consolidate, repair)
// that were never part of the public contract must survive untouched.
package server

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom/internal/search"
	"github.com/go-chi/chi/v5"
)

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

func (f parityPassage) GetPassage(ctx context.Context, id string) (*search.Passage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &search.Passage{ChunkID: id, Text: "text", ChunkIndex: 3, Section: []string{"Kap. 1"}}, nil
}

// removedCompatRoutes is the complete 13-path set: the unversioned
// sisters of the canonical /api/v1 public surface.
var removedCompatRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/health"},
	{http.MethodPost, "/api/zotero/sync"},
	{http.MethodGet, "/api/ingest/jobs"},
	{http.MethodPost, "/api/ingest/documents/d1/force-rebuild"},
	{http.MethodGet, "/api/zotero/selection"},
	{http.MethodGet, "/api/zotero/selection/resolved"},
	{http.MethodPut, "/api/zotero/selection"},
	{http.MethodGet, "/api/zotero/documents"},
	{http.MethodPost, "/api/search"},
	{http.MethodGet, "/api/passage/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d"},
	{http.MethodGet, "/api/passage/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d/page"},
	{http.MethodGet, "/api/kg/entities"},
	{http.MethodGet, "/api/kg/entities/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d/neighbors"},
}

// TestRemovedCompatRoutesAreGone: every unversioned compat path 404s —
// re-adding one (a "quick fix" against a 0.1.x client) turns this red.
func TestRemovedCompatRoutesAreGone(t *testing.T) {
	h := newParityServer(paritySearch{}, parityPassage{}).Handler()
	for _, r := range removedCompatRoutes {
		req := httptest.NewRequest(r.method, r.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: compat edge must be gone (404), got %d", r.method, r.path, rec.Code)
		}
	}
}

// TestCanonicalV1SurfaceAnswers: the canonical twin of every removed
// route still answers (200/400/503 — anything but 404).
func TestCanonicalV1SurfaceAnswers(t *testing.T) {
	h := newParityServer(paritySearch{}, parityPassage{}).Handler()
	for _, path := range []string{
		"/api/v1/health",
		"/api/v1/zotero/sync",
		"/api/v1/ingest/jobs",
		"/api/v1/ingest/documents/d1/force-rebuild",
		"/api/v1/zotero/selection",
		"/api/v1/zotero/selection/resolved",
		"/api/v1/zotero/documents",
		"/api/v1/search",
		"/api/v1/passage/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d",
		"/api/v1/passage/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d/page",
		"/api/v1/kg/entities",
		"/api/v1/kg/entities/0b8f6c6e-6d4a-4a1f-9c6d-1e2f3a4b5c6d/neighbors",
		"/api/v1/kg/relations",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			req := httptest.NewRequest(method, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s %s: canonical edge must exist, got 404", method, path)
			}
		}
	}
}

// TestOperationalSurfacesStayUnversioned: the admin/internal edges never
// were public API — they keep their unversioned names (a registered
// route answers non-404 for the right method; kg/consolidate and repair
// are nil-gated and 404 by design, probed via the route inventory in
// baseline).
func TestOperationalSurfacesStayUnversioned(t *testing.T) {
	srv := newParityServer(paritySearch{}, parityPassage{})
	want := map[string]bool{
		"/api/ws":                       false,
		"/api/runners/live":             false,
		"/api/processor/source/{jobID}": false,
	}
	if err := chi.Walk(srv.Handler().(*chi.Mux), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if _, ok := want[route]; ok && method == http.MethodGet {
			want[route] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for route, seen := range want {
		if !seen {
			t.Errorf("%s: operational edge must stay registered", route)
		}
	}
}

// paritySearch returns one fixed hit (deterministic fake from the
// parity era — the removal sonde reuses it).
type paritySearch struct{ err error }

// parityPassage returns one fixed passage; err != nil maps the 404 path.
type parityPassage struct{ err error }

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
