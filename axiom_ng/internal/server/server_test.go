package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sync"
)

type fakeChecker struct {
	err error
}

func (f fakeChecker) Ready() error { return f.err }

func TestHealthZoteroOK(t *testing.T) {
	s := New(":0", log.Default())
	s.RegisterCheck("zotero", fakeChecker{nil})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Errorf("OK = false, want true")
	}
	if body.Checks["zotero"] != "ok" {
		t.Errorf("zotero = %v, want ok", body.Checks["zotero"])
	}
}

func TestHealthZoteroUnreachable(t *testing.T) {
	s := New(":0", log.Default())
	s.RegisterCheck("zotero", fakeChecker{errors.New("zotero local api unreachable")})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (health endpoint reports, does not fail)", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK {
		t.Errorf("OK = true, want false when zotero down")
	}
}

func TestHealthChecksAllRegistered(t *testing.T) {
	s := New(":0", log.Default())
	s.RegisterCheck("zotero", fakeChecker{nil})
	s.RegisterCheck("postgres", fakeChecker{nil})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.Handler().ServeHTTP(rec, req)

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Errorf("OK = false, want true")
	}
	if body.Checks["zotero"] != "ok" || body.Checks["postgres"] != "ok" {
		t.Errorf("checks = %v, want both ok", body.Checks)
	}
}

type stubSync struct {
	lastOverride *sync.SyncOverride
	called       bool
	res          sync.Result
}

func (s *stubSync) Run(ctx context.Context, ov *sync.SyncOverride) (sync.Result, error) {
	s.lastOverride = ov
	s.called = true
	return s.res, nil
}

func TestSyncRouteIsSinglePublicPath(t *testing.T) {
	s := New(":0", log.Default())
	svc := &stubSync{res: sync.Result{SourceID: "src-1", Items: 39, Collections: 20, Documents: 16, Enqueued: 0, NewVersion: 181}}
	s.SetSyncAPI(svc)

	// The single public writer: POST /api/zotero/sync must call the service.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/zotero/sync", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/zotero/sync status = %d, want 200", rec.Code)
	}
	if !svc.called {
		t.Fatal("/api/zotero/sync must invoke the sync service")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["canonical_items"] != float64(39) {
		t.Errorf("canonical_items = %v, want 39", body["canonical_items"])
	}

	// The legacy consolidated route must be gone: 404.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/zotero/canonical-sync", nil)
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("/api/zotero/canonical-sync status = %d, want 404 (route removed)", rec2.Code)
	}
}
