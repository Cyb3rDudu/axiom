package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRootHandlerShapeMatchesPythonBackend(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	RootHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"message": "Axiom API v2.0",
		"status":  "running",
		"docs":    "/docs",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("field %q: got %q, want %q", k, body[k], v)
		}
	}
}

func TestHealthHandlerShapeMatchesPythonBackend(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	HealthHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "healthy" {
		t.Errorf("status: got %q, want %q", body["status"], "healthy")
	}
}
