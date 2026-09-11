package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/version"
)

// #205 DoD: /api/health must carry the same banner as `axiom-ng --version`.
func TestHealthReportsVersionBanner(t *testing.T) {
	s := New("127.0.0.1:0", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest("GET", "/api/health", nil))
	var body struct {
		Build string `json:"build"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Build != version.Banner() {
		t.Fatalf("health build %q != banner %q", body.Build, version.Banner())
	}
}

// #262 DoD: /api/health carries the contextual state — a permanently
// degraded deployment (rules configured, DB never synced) must be
// observable, not silent. Red under mutation: dropping the field from the
// response (or wiring a static value) fails the degraded assert.
func TestHealthReportsContextualState(t *testing.T) {
	s := New("127.0.0.1:0", nil)
	s.SetContextualState(func() string { return "degraded_no_sync" })
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest("GET", "/api/health", nil))
	var body struct {
		Contextual string `json:"contextual"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Contextual != "degraded_no_sync" {
		t.Fatalf("health contextual = %q, want degraded_no_sync", body.Contextual)
	}
}
