package server

// #259: force-rebuild endpoint tests. Repo is faked; the enqueue semantics
// (fresh job, force key) are pinned by the repo IT
// (force_rebuild_it_test.go). Here: wiring, validation and the
// error→status mapping the operator actually sees.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type fakeRebuildRepo struct {
	job *repo.Job
	err error
}

func (f *fakeRebuildRepo) EnqueueForceRebuild(_ context.Context, _ string) (*repo.Job, error) {
	return f.job, f.err
}

func rebuildServer(t *testing.T, fr *fakeRebuildRepo) (*Server, *httptest.ResponseRecorder) {
	t.Helper()
	s := New(":0", nil)
	if fr != nil {
		s.SetForceRebuildAPI(fr)
	}
	return s, httptest.NewRecorder()
}

func TestForceRebuildUnwired503(t *testing.T) {
	s, rec := rebuildServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/documents/00000000-0000-0000-0000-000000000001/force-rebuild", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestForceRebuildBadUUID400(t *testing.T) {
	s, rec := rebuildServer(t, &fakeRebuildRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/documents/not-a-uuid/force-rebuild", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestForceRebuildAccepted(t *testing.T) {
	docID := "00000000-0000-0000-0000-000000000001"
	job := &repo.Job{ID: "j-1", Status: "pending", ForceRebuild: true}
	s, rec := rebuildServer(t, &fakeRebuildRepo{job: job})
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/documents/"+docID+"/force-rebuild", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (job enqueued, compute async)", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ID":"j-1"`) || !strings.Contains(body, `"ForceRebuild":true`) {
		t.Fatalf("body = %s, want the enqueued job id and the force flag", body)
	}
}

func TestForceRebuildErrorStatusMap(t *testing.T) {
	docID := "00000000-0000-0000-0000-000000000001"
	cases := []struct {
		err  error
		want int
	}{
		{repo.ErrNoPreferredAttachment, http.StatusNotFound},
		{repo.ErrRebuildInFlight, http.StatusConflict},
		{repo.ErrNoContentHash, http.StatusUnprocessableEntity},
		{errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		s, rec := rebuildServer(t, &fakeRebuildRepo{err: tc.err})
		req := httptest.NewRequest(http.MethodPost, "/api/ingest/documents/"+docID+"/force-rebuild", nil)
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("err = %v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}
