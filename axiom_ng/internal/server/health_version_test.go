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
