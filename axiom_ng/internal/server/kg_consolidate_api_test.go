package server

// #197: POST /api/kg/consolidate — the standing entity-consolidation
// surface. Admin gate follows the write-route pattern (repair API): the
// route is REGISTERED only when the service is wired, so an unwired server
// answers 404, and the loopback bind is the admin boundary. Unit tests pin
// the wire contract with a fake; the DB behavior is IT-pinned in
// kg_consolidate_it_test.go (second run = no-op).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type fakeConsolidate struct {
	rep   repo.ConsolidationReport
	err   error
	calls int
}

func (f *fakeConsolidate) ConsolidateEntitiesReport(ctx context.Context) (repo.ConsolidationReport, error) {
	f.calls++
	return f.rep, f.err
}

func TestKGConsolidateRunsOnce(t *testing.T) {
	fc := &fakeConsolidate{rep: repo.ConsolidationReport{Merged: 7, DuplicateFormsBefore: 7, DuplicateFormsAfter: 0}}
	s := New(":0", nil)
	s.SetConsolidateService(fc)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/kg/consolidate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got repo.ConsolidationReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad body: %s", rec.Body.String())
	}
	if got != fc.rep {
		t.Fatalf("response must carry the merge numbers (merged, duplicate forms before/after), got %+v want %+v", got, fc.rep)
	}
	if fc.calls != 1 {
		t.Fatalf("one POST must run consolidation exactly once, got %d", fc.calls)
	}
}

func TestKGConsolidateNotWiredIs404(t *testing.T) {
	// Admin gate (repair-API pattern): an unwired server never registers
	// the write route — 404, not 503.
	s := New(":0", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/kg/consolidate", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unwired consolidate must 404 (write-route gate), got %d", rec.Code)
	}
}

func TestKGConsolidateMethodGuard(t *testing.T) {
	s := New(":0", nil)
	s.SetConsolidateService(&fakeConsolidate{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/kg/consolidate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must 405 (POST-only write route), got %d", rec.Code)
	}
}

func TestKGConsolidateServiceError(t *testing.T) {
	s := New(":0", nil)
	s.SetConsolidateService(&fakeConsolidate{err: context.DeadlineExceeded})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/kg/consolidate", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("service error must 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("error body must say so: %s", rec.Body.String())
	}
}
