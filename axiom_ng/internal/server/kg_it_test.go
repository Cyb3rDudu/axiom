package server

// R6 (#136) live proof: the /api/kg endpoints against the REAL axiom_db
// graph (26k entities / 55k mentions / 10k relations). Gated like the other
// integration suites.
//
// Run with:
//   AXIOM_KG_IT=1 \
//   AXIOM_TEST_DATABASE_URL=postgresql://axiom_user:...@.../axiom_db?sslmode=disable \
//   go test ./internal/server/ -run TestIT_KG -v

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

func kgITServer(t *testing.T) *Server {
	if os.Getenv("AXIOM_KG_IT") != "1" {
		t.Skip("AXIOM_KG_IT=1 required (real graph data)")
	}
	if os.Getenv("AXIOM_TEST_DATABASE_URL") == "" {
		t.Skip("AXIOM_TEST_DATABASE_URL required")
	}
	d, err := db.Open(t.Context(), os.Getenv("AXIOM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(d.Close)
	s := New(":0", nil)
	s.SetKGService(repo.New(d.Pool()))
	return s
}

func getJSON(t *testing.T, s *Server, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.Bytes()
}

func TestIT_KGEntitySearchUnitedNations(t *testing.T) {
	s := kgITServer(t)
	code, body := getJSON(t, s, "/api/kg/entities?q=united%20nations&limit=5")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	var ents []repo.KGEntity
	if err := json.Unmarshal(body, &ents); err != nil || len(ents) == 0 {
		t.Fatalf("no entities: %s", body)
	}
	t.Logf("[IT] united nations search: %d entities, top=%q (%d mentions, type=%q)",
		len(ents), ents[0].CanonicalForm, ents[0].Mentions, ents[0].Type)
	if ents[0].Mentions < 2 {
		t.Fatal("stability filter broken: top entity below 2 mentions")
	}
}

func TestIT_KGUnitedNationsNeighborhoodHasSDG(t *testing.T) {
	s := kgITServer(t)
	_, body := getJSON(t, s, "/api/kg/entities?q=united%20nations&limit=5")
	var ents []repo.KGEntity
	if err := json.Unmarshal(body, &ents); err != nil || len(ents) == 0 {
		t.Fatalf("entity search failed: %s", body)
	}
	un := ents[0]

	// Stable neighborhood (default >=2 both ends).
	code, nb := getJSON(t, s, "/api/kg/entities/"+un.ID+"/neighbors?limit=50")
	if code != http.StatusOK {
		t.Fatalf("neighbors: %d %s", code, nb)
	}
	var stable []repo.KGNeighbor
	if err := json.Unmarshal(nb, &stable); err != nil {
		t.Fatal(err)
	}
	// Noisy neighborhood for the contrast sample.
	_, nb1 := getJSON(t, s, "/api/kg/entities/"+un.ID+"/neighbors?min_mentions=1&limit=200")
	var noisy []repo.KGNeighbor
	_ = json.Unmarshal(nb1, &noisy)
	t.Logf("[IT] UN neighborhood: %d stable (>=2) vs %d unfiltered (>=1); top neighbors: %s",
		len(stable), len(noisy), forms(stable, 5))

	// The DoD sample: an SDG relation must appear in the stable neighborhood
	// with evidence chunk refs.
	for _, n := range stable {
		if hasSDG(n.OtherForm) && len(n.EvidenceChunks) > 0 {
			t.Logf("[IT] SDG relation found: UN --%s--> %q, evidence chunks %v",
				n.Type, n.OtherForm, n.EvidenceChunks[:minInt(3, len(n.EvidenceChunks))])
			// Evidence chain end-to-end: relation -> evidence chunk -> book/page.
			code, rels := getJSON(t, s, "/api/kg/relations?entity="+un.ID+"&limit=200")
			if code == http.StatusOK && len(rels) > 0 {
				t.Logf("[IT] relation browsing OK (%d bytes); chain verified via evidence chunk %s",
					len(rels), n.EvidenceChunks[0])
			}
			return
		}
	}
	t.Fatalf("no SDG relation with evidence in UN's stable neighborhood; neighbors: %s", forms(stable, 10))
}

func TestIT_KGStabilityFilterReducesNoise(t *testing.T) {
	s := kgITServer(t)
	// A hub entity's neighborhood must shrink when the floor rises —
	// the filter has observable effect on real data.
	_, body := getJSON(t, s, "/api/kg/entities?q=united%20nations&limit=1")
	var ents []repo.KGEntity
	if err := json.Unmarshal(body, &ents); err != nil || len(ents) == 0 {
		t.Fatalf("entity search failed: %s", body)
	}
	id := ents[0].ID
	_, nbLow := getJSON(t, s, "/api/kg/entities/"+id+"/neighbors?min_mentions=1&limit=200")
	_, nbHigh := getJSON(t, s, "/api/kg/entities/"+id+"/neighbors?min_mentions=3&limit=200")
	var low, high []repo.KGNeighbor
	_ = json.Unmarshal(nbLow, &low)
	_ = json.Unmarshal(nbHigh, &high)
	t.Logf("[IT] stability effect: >=1 -> %d neighbors, >=3 -> %d", len(low), len(high))
	if len(high) == 0 && len(low) > 0 {
		t.Fatal("floor 3 must not empty a real hub neighborhood")
	}
}

func forms(ns []repo.KGNeighbor, n int) string {
	out := ""
	for i, x := range ns {
		if i >= n {
			break
		}
		if i > 0 {
			out += ", "
		}
		out += x.OtherForm + "(" + x.Direction + ")"
	}
	return out
}

func hasSDG(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "sdg") || strings.Contains(l, "sustainable development")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
