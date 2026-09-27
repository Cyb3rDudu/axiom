// store_api_test.go — the intake route: 503 unwired, 400 invalid, 409
// mismatch, 202 accepted (the contract error mapping reuses
// writeContractError).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/revision"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/contracts/store"
)

type fakeStoreAPI struct {
	job store.IngestJob
	err error
	got store.IngestRevisionRequest
}

func (f *fakeStoreAPI) IngestRevision(ctx context.Context, req store.IngestRevisionRequest) (store.IngestJob, error) {
	f.got = req
	return f.job, f.err
}

func newStoreAPIServer(t *testing.T, api StoreAPI) *httptest.Server {
	t.Helper()
	s := New(":0", nil)
	if api != nil {
		s.SetStoreAPI(api)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func validIntakeBody() map[string]any {
	return map[string]any{
		"idempotency_key": "route-key-1",
		"revision": map[string]any{
			"source_id": "src-1", "revision_id": "1", "rendition_id": "rend-1",
			"content_hash":   revision.HashContent([]byte("route")),
			"media_type":     revision.MediaTypePDF,
			"content_ticket": "ticket-1",
			"bibliography":   map[string]any{"record_id": "rec-1", "citation_class": "citable"},
		},
	}
}

func postIntake(t *testing.T, ts *httptest.Server, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/store/ingest", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestStoreIntakeRouteUnwired503(t *testing.T) {
	ts := newStoreAPIServer(t, nil)
	if resp := postIntake(t, ts, validIntakeBody()); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired intake must 503, got %d", resp.StatusCode)
	}
}

func TestStoreIntakeRouteAccepts(t *testing.T) {
	api := &fakeStoreAPI{job: store.IngestJob{JobID: "j1", Status: store.IngestReceived}}
	ts := newStoreAPIServer(t, api)
	resp := postIntake(t, ts, validIntakeBody())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fresh intake must 202, got %d", resp.StatusCode)
	}
	var job store.IngestJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil || job.JobID != "j1" {
		t.Fatalf("response must echo the job, got %+v (%v)", job, err)
	}
	// The route must DECODE the body (not swallow it): the revision's
	// identity fields reach the API verbatim.
	if api.got.IdempotencyKey != "route-key-1" {
		t.Fatalf("idempotency key lost in decode: %+v", api.got)
	}
	if api.got.Revision.RenditionID != "rend-1" || api.got.Revision.Bibliography.RecordID != "rec-1" {
		t.Fatalf("revision identity lost in decode: %+v", api.got.Revision)
	}
}

func TestStoreIntakeRouteContractErrors(t *testing.T) {
	ts := newStoreAPIServer(t, &fakeStoreAPI{err: contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "no")})
	if resp := postIntake(t, ts, validIntakeBody()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("InvalidArgument must 400, got %d", resp.StatusCode)
	}
	ts2 := newStoreAPIServer(t, &fakeStoreAPI{err: &contracterr.IdempotencyMismatch{Key: "k"}})
	if resp := postIntake(t, ts2, validIntakeBody()); resp.StatusCode != http.StatusConflict {
		t.Fatalf("IdempotencyMismatch must 409, got %d", resp.StatusCode)
	}
	// Malformed (non-JSON) body: 400 via the route's own decode.
	ts3 := newStoreAPIServer(t, &fakeStoreAPI{})
	b := bytes.NewReader([]byte("not json at all"))
	resp, err := http.Post(ts3.URL+"/api/v1/store/ingest", "application/json", b)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body must 400, got %d", resp.StatusCode)
	}
}
