package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeChecker struct {
	err error
}

func (f fakeChecker) Ready() error { return f.err }

func TestHealthZoteroOK(t *testing.T) {
	s := New(":0", fakeChecker{nil}, log.Default())
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
	s := New(":0", fakeChecker{errors.New("zotero local api unreachable")}, log.Default())
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

func TestHealthZoteroUnknown(t *testing.T) {
	s := New(":0", nil, log.Default())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.Handler().ServeHTTP(rec, req)

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK {
		t.Errorf("OK = true with no checker, want false")
	}
	if body.Checks["zotero"] != "unknown" {
		t.Errorf("zotero = %v, want unknown", body.Checks["zotero"])
	}
}
