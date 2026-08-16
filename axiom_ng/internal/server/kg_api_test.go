package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

type fakeKG struct {
	entities  []repo.KGEntity
	neighbors []repo.KGNeighbor
	relations []repo.KGRelationView
	lastQ     string
	lastMin   int
	lastEnt   string
}

func (f *fakeKG) SearchKGEntities(ctx context.Context, q string, minMentions, limit int) ([]repo.KGEntity, error) {
	f.lastQ, f.lastMin = q, minMentions
	return f.entities, nil
}

func (f *fakeKG) KGNeighbors(ctx context.Context, id string, minMentions, limit int) ([]repo.KGNeighbor, error) {
	f.lastEnt, f.lastMin = id, minMentions
	return f.neighbors, nil
}

func (f *fakeKG) KGRelations(ctx context.Context, relType, entityID string, minMentions, limit int) ([]repo.KGRelationView, error) {
	f.lastEnt = entityID + "|" + relType
	return f.relations, nil
}

func kgServer(t *testing.T, kg *fakeKG) *Server {
	s := New(":0", nil)
	if kg != nil {
		s.SetKGService(kg)
	}
	return s
}

func TestKGRoutesNotConfigured(t *testing.T) {
	s := kgServer(t, nil)
	for _, path := range []string{"/api/kg/entities", "/api/kg/entities/00000000-0000-0000-0000-000000000000/neighbors", "/api/kg/relations"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: want 503, got %d", path, rec.Code)
		}
	}
}

func TestKGEntitiesSearch(t *testing.T) {
	kg := &fakeKG{entities: []repo.KGEntity{{ID: "e1", CanonicalForm: "United Nations", Mentions: 12}}}
	s := kgServer(t, kg)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/kg/entities?q=united&limit=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []repo.KGEntity
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].CanonicalForm != "United Nations" {
		t.Fatalf("bad body: %s", rec.Body.String())
	}
	if kg.lastQ != "united" || kg.lastMin != 2 {
		t.Fatalf("stability default (2) must apply, got q=%q min=%d", kg.lastQ, kg.lastMin)
	}
}

func TestKGNeighborsUUIDGuard(t *testing.T) {
	kg := &fakeKG{}
	s := kgServer(t, kg)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/kg/entities/not-a-uuid/neighbors", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-uuid id must 400, got %d", rec.Code)
	}
	// Valid shape passes through with the custom stability floor.
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/api/kg/entities/00000000-0000-0000-0000-00000000000a/neighbors?min_mentions=3", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("valid id must 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if kg.lastMin != 3 {
		t.Fatalf("min_mentions override broken: %d", kg.lastMin)
	}
}

func TestKGRelationsFilters(t *testing.T) {
	st := float32(0.8)
	kg := &fakeKG{relations: []repo.KGRelationView{{
		ID: "r1", Type: "supports", SourceID: "a", SourceForm: "UN",
		TargetID: "b", TargetForm: "SDG", Strength: &st, EvidenceChunks: []string{"c1"},
	}}}
	s := kgServer(t, kg)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/kg/relations?type=supports&entity=00000000-0000-0000-0000-00000000000b", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"SDG"`) || !strings.Contains(rec.Body.String(), `"evidence_chunks":["c1"]`) {
		t.Fatalf("relation shape wrong: %s", rec.Body.String())
	}
	if kg.lastEnt != "00000000-0000-0000-0000-00000000000b|supports" {
		t.Fatalf("filters not passed: %q", kg.lastEnt)
	}
}
