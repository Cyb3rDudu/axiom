// A2 #166: selection + documents route tests (validation at the boundary,
// round-trip, degradation).
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type stubSelection struct {
	put      []repo.SelectionInput
	collsPut []repo.CollectionSelectionInput
	colls    map[string]string
	mode     map[string]string
	docs     []repo.ZoteroDocumentState
	err      error
}

func (s *stubSelection) SetSelectionBatch(ctx context.Context, docs []repo.SelectionInput, colls []repo.CollectionSelectionInput) error {
	s.put = docs
	s.collsPut = colls
	for _, e := range docs {
		if e.Mode == "default" || e.Mode == "" {
			delete(s.mode, e.DocumentID)
		} else {
			s.mode[e.DocumentID] = e.Mode
		}
	}
	for _, e := range colls {
		if e.Mode == "default" || e.Mode == "" {
			delete(s.colls, e.CollectionKey)
		} else {
			s.colls[e.CollectionKey] = e.Mode
		}
	}
	return s.err
}

func (s *stubSelection) SetSelections(ctx context.Context, in []repo.SelectionInput) error {
	s.put = in
	for _, e := range in {
		if e.Mode == "default" || e.Mode == "" {
			delete(s.mode, e.DocumentID)
		} else {
			s.mode[e.DocumentID] = e.Mode
		}
	}
	return s.err
}

func (s *stubSelection) SelectionModes(ctx context.Context) (map[string]string, error) {
	return s.mode, s.err
}

func (s *stubSelection) SetCollectionSelections(ctx context.Context, in []repo.CollectionSelectionInput) error {
	s.collsPut = in
	return s.err
}

func (s *stubSelection) CollectionSelectionModes(ctx context.Context) (map[string]string, error) {
	return s.colls, s.err
}

func (s *stubSelection) ResolveSelectionView(ctx context.Context) (*repo.ResolvedSelection, error) {
	return &repo.ResolvedSelection{Documents: s.mode, Collections: []repo.ResolvedCollection{}}, s.err
}

func (s *stubSelection) ListZoteroDocuments(ctx context.Context, syncState string) ([]repo.ZoteroDocumentState, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := []repo.ZoteroDocumentState{}
	for _, d := range s.docs {
		if syncState == "" || syncState == d.SyncState {
			out = append(out, d)
		}
	}
	return out, nil
}

func selReq(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, *stubSelection) {
	t.Helper()
	s := New(":0", nil)
	stub := &stubSelection{mode: map[string]string{}}
	s.SetSelectionRepo(stub)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec, stub
}

func TestSelectionRoundTrip(t *testing.T) {
	id := "00000000-0000-4000-8000-000000000001"
	s := New(":0", nil)
	stub := &stubSelection{mode: map[string]string{}}
	s.SetSelectionRepo(stub)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/zotero/selection",
		strings.NewReader(`{"selection":[{"document_id":"`+id+`","mode":"excluded"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if len(stub.put) != 1 || stub.put[0].Mode != "excluded" {
		t.Fatalf("batch not passed through: %+v", stub.put)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zotero/selection", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"excluded"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
}

// #166 wiring witness (Hivemind Fix-Auftrag 2): the FULL route path —
// PUT with a collection entry -> GET shows it -> resolved expands it. The
// original bug validated collections in the handler and dropped them;
// repo-direct tests stayed green. Route-level witnesses are mandatory for
// this API's acceptance from here on.
func TestSelectionRouteWitness_CollectionsThroughHTTP(t *testing.T) {
	s := New(":0", nil)
	stub := &stubSelection{mode: map[string]string{}, colls: map[string]string{}}
	s.SetSelectionRepo(stub)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/zotero/selection",
		strings.NewReader(`{"selection":[{"document_id":"00000000-0000-4000-8000-000000000001","mode":"excluded"}],"collections":[{"collection_key":"VWLPRAXY","mode":"included"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if len(stub.collsPut) != 1 || stub.collsPut[0].CollectionKey != "VWLPRAXY" {
		t.Fatalf("collection entry must reach the batch write: %+v", stub.collsPut)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zotero/selection", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"VWLPRAXY":"included"`) {
		t.Fatalf("get must show the collection selection: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zotero/selection/resolved", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSelectionValidation(t *testing.T) {
	if rec, _ := selReq(t, http.MethodPut, "/api/zotero/selection",
		`{"selection":[{"document_id":"nope","mode":"excluded"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-uuid must 400: %d", rec.Code)
	}
	if rec, _ := selReq(t, http.MethodPut, "/api/zotero/selection",
		`{"selection":[{"document_id":"00000000-0000-4000-8000-000000000001","mode":"wah"}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode must 400: %d", rec.Code)
	}
	if rec, _ := selReq(t, http.MethodGet, "/api/zotero/documents?sync_state=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sync_state must 400: %d", rec.Code)
	}
	if rec, _ := selReq(t, http.MethodGet, "/api/zotero/selection", ""); rec.Code != http.StatusOK {
		// stub wired in selReq — not-configured covered separately below
		t.Fatalf("wired get must 200: %d", rec.Code)
	}
}

func TestSelectionNotConfigured(t *testing.T) {
	s := New(":0", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zotero/selection", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
}

func TestSelectionFKMaps422(t *testing.T) {
	s := New(":0", nil)
	stub := &stubSelection{mode: map[string]string{},
		err: fmt.Errorf("upsert selection: %w", &pgconn.PgError{Code: "23503", Message: "insert or update on table \"zotero_selections\" violates foreign key"})}
	s.SetSelectionRepo(stub)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/zotero/selection",
		strings.NewReader(`{"selection":[{"document_id":"00000000-0000-4000-8000-000000000001","mode":"excluded"}]}`)))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unknown document") {
		t.Fatalf("fk violation must 422 with hint, got %d %s", rec.Code, rec.Body.String())
	}
	// other errors stay 500
	stub.err = fmt.Errorf("boom")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/zotero/selection",
		strings.NewReader(`{"selection":[{"document_id":"00000000-0000-4000-8000-000000000001","mode":"excluded"}]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("non-fk error must stay 500, got %d", rec.Code)
	}
}

func TestDocumentsListing(t *testing.T) {
	s := New(":0", nil)
	stub := &stubSelection{mode: map[string]string{}, docs: []repo.ZoteroDocumentState{
		{DocumentID: "d1", Title: "Synced", SyncState: "synced", UpdatedAt: time.Now()},
		{DocumentID: "d2", Title: "Held", SyncState: "held", UpdatedAt: time.Now()},
	}}
	s.SetSelectionRepo(stub)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/zotero/documents?sync_state=held", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sync_state":"held"`) {
		t.Fatalf("listing: %d %s", rec.Code, rec.Body.String())
	}
	// the filter is honored server-side: the synced doc must NOT be in it
	if strings.Contains(rec.Body.String(), "Synced") {
		t.Fatalf("sync_state filter not honored: %s", rec.Body.String())
	}
}

func TestSyncOverrideBody(t *testing.T) {
	// the override reaches the sync service validated
	s := New(":0", nil)
	stub := &stubSync{}
	s.SetSyncAPI(stub)
	body := `{"include":["00000000-0000-4000-8000-000000000009"],"exclude":["00000000-0000-4000-8000-000000000008"]}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/zotero/sync", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync with override: %d %s", rec.Code, rec.Body.String())
	}
	if stub.lastOverride == nil || len(stub.lastOverride.Include) != 1 || len(stub.lastOverride.Exclude) != 1 {
		t.Fatalf("override not passed: %+v", stub.lastOverride)
	}
	// invalid ids are rejected before any sync runs
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/zotero/sync", strings.NewReader(`{"include":["junk"]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad override id must 400: %d", rec.Code)
	}
	// empty body = plain sync (nil override)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/zotero/sync", nil))
	if rec.Code != http.StatusOK || stub.lastOverride != nil {
		t.Fatalf("plain sync must pass nil override: %d %+v", rec.Code, stub.lastOverride)
	}
}
