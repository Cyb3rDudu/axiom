package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type fakeJobRepo struct{ jobs []repo.Job }

func (f fakeJobRepo) ListJobs(ctx context.Context, limit int) ([]repo.Job, error) {
	return f.jobs, nil
}

// TestJobsEndpointReturnsFailedJobWithNullHash verifies GET /api/ingest/jobs
// serializes a failed job whose content_hash is NULL as a 200 response, instead
// of crashing on the nil column like the earlier string scan.
func TestJobsEndpointReturnsFailedJobWithNullHash(t *testing.T) {
	code := "FILE_NOT_FOUND"
	msg := "no such file"
	jobs := []repo.Job{{
		ID: "job-1", SourceID: "s", DocumentID: "d", AttachmentID: "a",
		Status: "failed", ContentHash: nil, Attempt: 0, MaxAttempts: 0,
		ErrorCode: &code, ErrorMessage: &msg,
	}}
	s := New(":0", log.Default())
	s.SetJobRepo(fakeJobRepo{jobs})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ingest/jobs", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (failed job with NULL hash must be readable)", rec.Code)
	}
	var body struct {
		Jobs []struct {
			ID          string  `json:"ID"`
			Status      string  `json:"Status"`
			ContentHash *string `json:"ContentHash"`
			ErrorCode   *string `json:"ErrorCode"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(body.Jobs))
	}
	if body.Jobs[0].ContentHash != nil {
		t.Errorf("content_hash = %v, want null", body.Jobs[0].ContentHash)
	}
	if body.Jobs[0].ErrorCode == nil || *body.Jobs[0].ErrorCode != "FILE_NOT_FOUND" {
		t.Errorf("error_code = %v, want FILE_NOT_FOUND", body.Jobs[0].ErrorCode)
	}
}
