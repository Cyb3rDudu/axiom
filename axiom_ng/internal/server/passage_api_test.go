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
